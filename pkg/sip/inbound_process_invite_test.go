package sip

import (
	"log/slog"
	"math/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type InboundTest struct {
	Server        *Server
	Handler       Handler
	Client        *sipgo.Client
	addr          netip.AddrPort
	LiveKitClient *Client
}

func (it *InboundTest) NewInvite(t *testing.T, callID string, cseq uint32, offer []byte) (*sip.Request, []byte) {
	if offer == nil {
		sdpOffer, err := sdp.NewOffer(it.addr.Addr(), 0xB0B, sdp.EncryptionNone)
		require.NoError(t, err)
		offer, err = sdpOffer.SDP.Marshal()
		require.NoError(t, err)
	}

	inviteReq := sip.NewRequest(sip.INVITE, sip.Uri{User: "to", Host: it.addr.String()})
	fromTag := sip.GenerateTagN(16)
	inviteReq.AppendHeader(&sip.FromHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   "caller",
			Host:   it.addr.String(),
		},
		Params: sip.HeaderParams{
			{"tag", fromTag},
		},
	})
	inviteReq.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   "callee",
			Host:   it.addr.String(),
		},
	})
	inviteReq.SetDestination(it.addr.String())
	inviteReq.SetBody(offer)
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	callIDHdr := sip.CallIDHeader(callID)
	inviteReq.AppendHeader(&callIDHdr)
	inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.INVITE})
	return inviteReq, offer
}

func (it *InboundTest) NewInviteWithToTag(t *testing.T, callID string, cseq uint32, toTag LocalTag, offer []byte) (*sip.Request, []byte) {
	inviteReq, offer := it.NewInvite(t, callID, cseq, offer)
	to := inviteReq.To()
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Remove("tag")
	to.Params.Add("tag", string(toTag))
	return inviteReq, offer
}

func (it *InboundTest) TransactionRequest(t *testing.T, req *sip.Request) *sip.Response {
	tx, err := it.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	resp := getFinalResponseOrFail(t, tx, req)
	if resp.StatusCode < 300 { // Need to send ACK for 2xx, sipgo sends ACK for 3xx+
		ack := sip.NewAckRequest(req, resp, nil)
		err = it.Client.WriteRequest(ack)
		require.NoError(t, err)
	}
	return resp
}

func (it *InboundTest) Address() netip.AddrPort {
	return it.addr
}

func NewInboundTest(t *testing.T) *InboundTest {
	t.Helper()

	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	loopback := netip.MustParseAddr("127.0.0.1")

	conf := &config.Config{
		MaxCpuUtilization:  0.9,
		SIPPort:            sipPort,
		SIPPortListen:      sipPort,
		RTPPort:            rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
		SIPRingingInterval: time.Second,
	}
	mon, err := stats.NewMonitor(conf)
	require.NoError(t, err)
	require.NoError(t, mon.Start(conf), "start monitor so metrics (e.g. inviteReqRaw) are registered")

	log := logger.NewTestLogger(t)

	cli := NewClient("", conf, log, mon, func(projectID string) rpc.IOInfoClient { return &MockIOInfoClient{} })
	srv := NewServer(
		"",
		conf,
		log,
		mon,
		func(projectID string) rpc.IOInfoClient { return &MockIOInfoClient{} },
		WithGetRoomServer(newTestRoom),
		WithClient(cli),
	)
	require.NotNil(t, srv)

	sconf := &ServiceConfig{
		SignalingIP:      loopback,
		SignalingIPLocal: loopback,
		MediaIP:          loopback,
	}

	// Wire the client's OnRequest as the unhandled-request handler, mirroring
	// Service.Start. This lets the server delegate re-INVITEs for outbound calls
	// to the client (Call-ID based matching), as in production.
	err = srv.Start(nil, sconf, nil, cli.OnRequest)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	handler := &TestHandler{}
	srv.SetHandler(handler)

	addr := netip.AddrPortFrom(loopback, uint16(sipPort))

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("from@test"),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(srv.log))),
	)
	require.NoError(t, err)

	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	t.Cleanup(func() { ua.Close() })

	return &InboundTest{Server: srv, Handler: handler, Client: client, addr: addr, LiveKitClient: cli}
}

// RegisterOutboundCallForReinvite registers a fake outbound call keyed by sipCallID
// so that a mid-dialog re-INVITE carrying the same Call-ID is delegated to the client
// (Client.onInvite -> byCallID) and answered via AcceptReInvite with the call's SDP offer.
// It returns the SDP offer that AcceptReInvite echoes back in the 200 OK.
func (it *InboundTest) RegisterOutboundCallForReinvite(t *testing.T, localTag LocalTag, sipCallID string) (offer []byte) {
	t.Helper()

	sdpOffer, err := sdp.NewOffer(netip.MustParseAddr("1.2.3.4"), 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offer, err = sdpOffer.SDP.Marshal()
	require.NoError(t, err)
	sdpAnswer, _, err := sdpOffer.Answer(netip.MustParseAddr("4.3.2.1"), 0xB00, sdp.EncryptionNone)
	require.NoError(t, err)
	answer, err := sdpAnswer.SDP.Marshal()
	require.NoError(t, err)

	log := logger.NewTestLogger(t).WithValues("callID", localTag)
	from := CreateURIFromUserAndAddress("out", it.addr.String(), TransportUDP)
	contact := CreateURIFromUserAndAddress("out", it.addr.String(), TransportUDP)
	so := it.LiveKitClient.newOutbound(log, localTag, from, contact, nil, nil)
	// AcceptReInvite responds with the original outbound INVITE's body, so set it to the offer.
	fauxInvite := sip.NewRequest(sip.INVITE, sip.Uri{User: "to", Host: it.addr.String()})
	fauxInvite.SetBody(offer)
	faux200 := sip.NewResponseFromRequest(fauxInvite, sip.StatusOK, "OK", answer)
	so.mu.Lock()
	so.invite = fauxInvite
	so.inviteOk = faux200
	so.callID = sipCallID
	so.mu.Unlock()
	oc := &outboundCall{cc: so, log: log}
	it.LiveKitClient.cmu.Lock()
	it.LiveKitClient.activeCalls[localTag] = oc
	if sipCallID != "" {
		it.LiveKitClient.byCallID[sipCallID] = oc
	}
	it.LiveKitClient.cmu.Unlock()

	return offer
}

func TestProcessInvite_Reinvite(t *testing.T) {
	it := NewInboundTest(t)

	cseq := uint32(2)
	callID := "reinvite-new@test"
	origInviteReq, _ := it.NewInvite(t, callID, cseq, nil)
	firstResp := it.TransactionRequest(t, origInviteReq.Clone())
	require.Equal(t, sip.StatusCode(200), firstResp.StatusCode, "200 OK")
	answer := string(firstResp.Body())

	// Test prev CSeq
	req2 := origInviteReq.Clone()
	req2.CSeq().SeqNo = cseq - 1
	resp2 := it.TransactionRequest(t, req2)
	require.Equal(t, sip.StatusCode(200), resp2.StatusCode, "200 OK")
	require.NotEqual(t, answer, string(resp2.Body()), "answer should not be the same as original answer")

	// Test next CSeq
	req3 := origInviteReq.Clone()
	req3.CSeq().SeqNo = cseq + 1
	req3.ReplaceHeader(sip.HeaderClone(firstResp.To()))
	resp3 := it.TransactionRequest(t, req3)
	require.Equal(t, sip.StatusCode(200), resp3.StatusCode, "200 OK")
	require.Equal(t, answer, string(resp3.Body()), "answer should be the same")
	require.NotEqual(t, resp2.To().Params.GetOr("tag", ""), resp3.To().Params.GetOr("tag", ""), "to tag should not be the same")
}

func TestProcessInvite_ReinviteOutbound(t *testing.T) {
	it := NewInboundTest(t)

	localTag := LocalTag("out-reinvite-2")
	callID := "reinvite-outbound@test"
	offer := it.RegisterOutboundCallForReinvite(t, localTag, callID)

	// Mid-dialog re-INVITE for the outbound call: same Call-ID, with a To tag so the
	// server delegates the unmatched dialog to the client (Call-ID based matching).
	req, _ := it.NewInviteWithToTag(t, callID, 2, localTag, offer)
	resp := it.TransactionRequest(t, req)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
	require.Equal(t, offer, resp.Body(), "reinvite 200 OK should return the outbound call's SDP")

	// A subsequent re-INVITE with the same Call-ID is still matched and answered.
	req2, _ := it.NewInviteWithToTag(t, callID, 3, localTag, offer)
	resp2 := it.TransactionRequest(t, req2)
	require.Equal(t, sip.StatusCode(200), resp2.StatusCode, "reinvite for outbound call should get 200 OK")
	require.Equal(t, offer, resp2.Body(), "reinvite 200 OK should return the outbound call's SDP")
}

func TestProcessInvite_ReinviteOutbound_Miss(t *testing.T) {
	it := NewInboundTest(t)

	outboundTag := LocalTag("outbound-call-1")
	callID := "reinvite-outbound@test"
	offer := it.RegisterOutboundCallForReinvite(t, outboundTag, callID)

	// Control: a re-INVITE with the registered Call-ID is matched and answered with the call's SDP.
	req, _ := it.NewInviteWithToTag(t, callID, 2, outboundTag, offer)
	resp := it.TransactionRequest(t, req)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
	require.Equal(t, offer, resp.Body(), "reinvite 200 OK should return the outbound call's SDP")

	// Experiment: an INVITE with an unknown Call-ID is not matched to the outbound call;
	// it falls through to new-call processing and must not echo the existing call's SDP.
	req2, _ := it.NewInviteWithToTag(t, "reinvite-outbound-miss@test", 2, LocalTag("outbound-call-2"), offer)
	resp2 := it.TransactionRequest(t, req2)
	require.Equal(t, sip.StatusCode(200), resp2.StatusCode, "unmatched reinvite should be processed as a new call")
	require.NotEqual(t, offer, resp2.Body(), "unmatched reinvite must not return the existing call's SDP")
}
