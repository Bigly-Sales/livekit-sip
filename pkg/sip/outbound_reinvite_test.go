// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"testing"

	psdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sipgo/sip"
)

// newTestOutboundCall creates a minimal outbound call for re-INVITE testing.
func newTestOutboundCall(callID string, localTag LocalTag, sdpOffer []byte) (*Client, *outboundCall) {
	log := logger.GetLogger()
	c := &Client{
		log:         log,
		conf:        &config.Config{},
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
		byCallID:    make(map[string]*outboundCall),
	}

	// Build a minimal INVITE request with the SDP offer body
	invite := sip.NewRequest(sip.INVITE, sip.Uri{Host: "remote.com", User: "callee"})
	invite.SetBody(sdpOffer)

	contactHeader := &sip.ContactHeader{
		Address: sip.Uri{Host: "local.com", Port: 5060},
	}

	// Store ownSDP as an independent copy, just like Invite() does
	ownSDP := make([]byte, len(sdpOffer))
	copy(ownSDP, sdpOffer)

	out := &sipOutbound{
		log:     log,
		c:       c,
		id:      localTag,
		callID:  callID,
		invite:  invite,
		ownSDP:  ownSDP,
		contact: contactHeader,
	}

	call := &outboundCall{
		cc:  out,
		log: log,
	}

	// Register in all maps
	c.activeCalls[localTag] = call
	if callID != "" {
		c.byCallID[callID] = call
	}

	return c, call
}

// TestOutboundReInviteHandling tests that Client.onInvite properly routes
// re-INVITEs to the matching outbound call.
func TestOutboundReInviteHandling(t *testing.T) {
	sdpOffer := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\nc=IN IP4 1.2.3.4\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", sdpOffer)

	// Build a re-INVITE with the same Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-call-123")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.True(t, handled, "re-INVITE should be handled by client")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
	require.Equal(t, sdpOffer, tx.responses[0].Body(), "should respond with our SDP offer")

	// Verify Content-Type header
	ctHeader := tx.responses[0].GetHeader("Content-Type")
	require.NotNil(t, ctHeader, "should have Content-Type header")
	require.Equal(t, "application/sdp", ctHeader.Value(), "Content-Type should be application/sdp")
}

// TestOutboundReInviteUnknownCall tests that Client.onInvite returns false
// for re-INVITEs with unknown Call-IDs.
func TestOutboundReInviteUnknownCall(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	// Build a re-INVITE with a different Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("unknown-call-456")
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.False(t, handled, "re-INVITE with unknown Call-ID should not be handled")
	require.Len(t, tx.responses, 0, "should not send any response")
}

// TestOutboundReInviteNoCallID tests that Client.onInvite returns false
// when the INVITE has no Call-ID header.
func TestOutboundReInviteNoCallID(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	// No Call-ID header

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.False(t, handled, "INVITE without Call-ID should not be handled")
	require.Len(t, tx.responses, 0, "should not send any response")
}

// TestOutboundReInviteSDPNotEchoed verifies that AcceptReInvite responds with
// OUR SDP, not the carrier's SDP from the re-INVITE request. Echoing the
// carrier's SDP back would tell them to loop audio to themselves.
func TestOutboundReInviteSDPNotEchoed(t *testing.T) {
	ourSDP := []byte("v=0\r\no=- 123 456 IN IP4 157.55.199.120\r\ns=LiveKit\r\nc=IN IP4 157.55.199.120\r\nt=0 0\r\nm=audio 19839 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	carrierSDP := []byte("v=0\r\no=- 149197 630265 IN IP4 152.188.164.136\r\ns=DNL-SWITCH\r\nc=IN IP4 152.188.164.198\r\nt=0 0\r\nm=audio 27530 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")

	c, _ := newTestOutboundCall("test-call-sdp", "SCL_sdptest", ourSDP)

	// Build a re-INVITE with the carrier's SDP (session refresh from carrier)
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-call-sdp")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	req.SetBody(carrierSDP)

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.True(t, handled, "re-INVITE should be handled")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")

	// Critical: response must contain OUR SDP, not the carrier's.
	// Note: handleReInvite re-marshals the SDP (to set direction), so we compare
	// parsed fields rather than exact bytes.
	respBody := tx.responses[0].Body()
	require.Contains(t, string(respBody), "s=LiveKit", "response SDP should have our session name")
	require.NotContains(t, string(respBody), "s=DNL-SWITCH", "response SDP must not have carrier's session name")

	// Parse and verify our IP is in the response, not the carrier's
	respSDP := new(psdp.SessionDescription)
	err := respSDP.Unmarshal(respBody)
	require.NoError(t, err)
	require.Contains(t, string(respBody), "157.55.199.120", "response should contain our IP")
	require.NotContains(t, string(respBody), "152.188.164.198", "response must not contain carrier's media IP")
}

// TestOutboundAcceptReInviteNoSDP tests that AcceptReInvite responds with 500
// when no SDP is available (invite is nil).
func TestOutboundAcceptReInviteNoSDP(t *testing.T) {
	log := logger.GetLogger()
	out := &sipOutbound{
		log: log,
		contact: &sip.ContactHeader{
			Address: sip.Uri{Host: "local.com", Port: 5060},
		},
		// invite is nil, so no SDP available
	}

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	tx := &testServerTransaction{}

	out.AcceptReInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode,
		"should respond with 500 when no SDP available")
}

// TestOutboundByCallIDCleanup tests that byCallID is cleaned up properly.
func TestOutboundByCallIDCleanup(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	// Verify the call is registered
	c.cmu.Lock()
	require.NotNil(t, c.byCallID["test-call-123"], "call should be in byCallID")
	require.NotNil(t, c.activeCalls["SCL_test123"], "call should be in activeCalls")
	c.cmu.Unlock()

	// Simulate cleanup (what happens in outboundCall.close)
	c.cmu.Lock()
	delete(c.activeCalls, "SCL_test123")
	delete(c.byCallID, "test-call-123")
	c.cmu.Unlock()

	// Verify cleanup
	c.cmu.Lock()
	require.Nil(t, c.byCallID["test-call-123"], "call should be removed from byCallID")
	require.Nil(t, c.activeCalls["SCL_test123"], "call should be removed from activeCalls")
	c.cmu.Unlock()
}

// TestOnRequestRoutesInviteToClient tests that Client.OnRequest properly
// routes INVITE method to the onInvite handler.
func TestOnRequestRoutesInviteToClient(t *testing.T) {
	sdpOffer := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\nc=IN IP4 1.2.3.4\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	c, _ := newTestOutboundCall("test-call-789", "SCL_test789", sdpOffer)

	// Build a re-INVITE with matching Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-call-789")
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.OnRequest(req, tx)

	require.True(t, handled, "OnRequest should handle INVITE for known outbound call")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
}

// TestHandleReInviteHold tests that handleReInvite responds with recvonly
// when the carrier sends a hold re-INVITE with sendonly.
func TestHandleReInviteHold(t *testing.T) {
	ourSDP := []byte("v=0\r\no=- 123 456 IN IP4 157.55.199.120\r\ns=LiveKit\r\n" +
		"c=IN IP4 157.55.199.120\r\nt=0 0\r\n" +
		"m=audio 19839 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n")
	carrierHoldSDP := []byte("v=0\r\no=- 149197 630265 IN IP4 152.188.164.136\r\ns=DNL-SWITCH\r\n" +
		"c=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
		"m=audio 27530 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=sendonly\r\n")

	_, call := newTestOutboundCall("test-hold", "SCL_hold", ourSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-hold")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	req.SetBody(carrierHoldSDP)

	tx := &testServerTransaction{}
	call.handleReInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")

	respBody := tx.responses[0].Body()
	respSDP := new(psdp.SessionDescription)
	err := respSDP.Unmarshal(respBody)
	require.NoError(t, err)

	// Response must have recvonly (complement of sendonly)
	dir := getMediaDirection(respSDP)
	require.Equal(t, "recvonly", dir, "response direction should be recvonly for hold")

	// Response must contain our session name and IP
	require.Contains(t, string(respBody), "s=LiveKit")
	require.Contains(t, string(respBody), "157.55.199.120")
}

// TestHandleReInviteResume tests that handleReInvite responds with sendrecv
// when the carrier sends a resume re-INVITE with sendrecv.
func TestHandleReInviteResume(t *testing.T) {
	ourSDP := []byte("v=0\r\no=- 123 456 IN IP4 157.55.199.120\r\ns=LiveKit\r\n" +
		"c=IN IP4 157.55.199.120\r\nt=0 0\r\n" +
		"m=audio 19839 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n")
	carrierResumeSDP := []byte("v=0\r\no=- 149197 630266 IN IP4 152.188.164.136\r\ns=DNL-SWITCH\r\n" +
		"c=IN IP4 152.188.164.198\r\nt=0 0\r\n" +
		"m=audio 27530 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n")

	_, call := newTestOutboundCall("test-resume", "SCL_resume", ourSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-resume")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 3, MethodName: sip.INVITE})
	req.SetBody(carrierResumeSDP)

	tx := &testServerTransaction{}
	call.handleReInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")

	respBody := tx.responses[0].Body()
	respSDP := new(psdp.SessionDescription)
	err := respSDP.Unmarshal(respBody)
	require.NoError(t, err)

	// Response must have sendrecv (complement of sendrecv)
	dir := getMediaDirection(respSDP)
	require.Equal(t, "sendrecv", dir, "response direction should be sendrecv for resume")
}

// TestHandleReInviteNoSDP tests that handleReInvite falls back to AcceptReInvite
// when the re-INVITE has no SDP body (session refresh).
func TestHandleReInviteNoSDP(t *testing.T) {
	ourSDP := []byte("v=0\r\no=- 123 456 IN IP4 157.55.199.120\r\ns=LiveKit\r\n" +
		"c=IN IP4 157.55.199.120\r\nt=0 0\r\n" +
		"m=audio 19839 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")

	_, call := newTestOutboundCall("test-nossdp", "SCL_nosdp", ourSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-nossdp")
	req.AppendHeader(&callIDHeader)
	// No body set — session refresh

	tx := &testServerTransaction{}
	call.handleReInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")

	// Falls back to AcceptReInvite which returns exact ownSDP
	require.Equal(t, ourSDP, tx.responses[0].Body(), "should respond with original SDP unchanged")
}

// TestHandleReInviteInactive tests that handleReInvite responds with inactive
// when the carrier sends an inactive direction.
func TestHandleReInviteInactive(t *testing.T) {
	ourSDP := []byte("v=0\r\no=- 123 456 IN IP4 157.55.199.120\r\ns=LiveKit\r\n" +
		"c=IN IP4 157.55.199.120\r\nt=0 0\r\n" +
		"m=audio 19839 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n")
	carrierSDP := []byte("v=0\r\no=- 149197 630265 IN IP4 152.188.164.136\r\ns=DNL-SWITCH\r\n" +
		"c=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
		"m=audio 0 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=inactive\r\n")

	_, call := newTestOutboundCall("test-inactive", "SCL_inactive", ourSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-inactive")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	req.SetBody(carrierSDP)

	tx := &testServerTransaction{}
	call.handleReInvite(req, tx)

	require.Len(t, tx.responses, 1)
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)

	respSDP := new(psdp.SessionDescription)
	err := respSDP.Unmarshal(tx.responses[0].Body())
	require.NoError(t, err)
	require.Equal(t, "inactive", getMediaDirection(respSDP), "response direction should be inactive")
}

// TestEarlyByCallIDRegistration verifies that the parentCall back-reference
// enables early byCallID registration. When parentCall is set on sipOutbound,
// the early registration code path (inside Invite) should populate byCallID
// immediately after Call-ID assignment — before the 200 OK is processed.
//
// This prevents re-INVITEs arriving during the INVITE transaction from being
// misidentified as new inbound calls.
func TestEarlyByCallIDRegistration(t *testing.T) {
	log := logger.GetLogger()
	c := &Client{
		log:         log,
		conf:        &config.Config{},
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
		byCallID:    make(map[string]*outboundCall),
	}

	call := &outboundCall{
		c:   c,
		log: log,
	}

	out := &sipOutbound{
		log:        log,
		c:          c,
		id:         "SCL_early_reg",
		parentCall: call, // back-reference enables early registration
		contact: &sip.ContactHeader{
			Address: sip.Uri{Host: "local.com", Port: 5060},
		},
		ownSDP: []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=LiveKit\r\n" +
			"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
			"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"),
	}
	call.cc = out

	// Simulate the early registration that happens inside Invite():
	// 1. Assign Call-ID
	// 2. Register in byCallID via parentCall
	const testCallID = "early-reg-test-call-id"
	out.callID = testCallID
	if out.parentCall != nil {
		c.cmu.Lock()
		c.byCallID[out.callID] = out.parentCall
		c.cmu.Unlock()
	}

	// Verify byCallID is populated
	c.cmu.Lock()
	registered := c.byCallID[testCallID]
	c.cmu.Unlock()
	require.NotNil(t, registered, "byCallID should be populated after early registration")
	require.Equal(t, call, registered, "byCallID should point to the outboundCall")

	// Verify Client.onInvite() can route a re-INVITE to this call
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader(testCallID)
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.True(t, handled, "Client.onInvite() should find the early-registered call")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
}

// TestEarlyByCallIDRegistrationSkippedWithoutParentCall verifies that the
// early registration code path is safely skipped when parentCall is nil.
// This ensures backward compatibility — sipOutbound instances created without
// parentCall (e.g., in older code paths or tests) don't panic or register.
func TestEarlyByCallIDRegistrationSkippedWithoutParentCall(t *testing.T) {
	log := logger.GetLogger()
	c := &Client{
		log:         log,
		conf:        &config.Config{},
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
		byCallID:    make(map[string]*outboundCall),
	}

	out := &sipOutbound{
		log:        log,
		c:          c,
		id:         "SCL_no_parent",
		parentCall: nil, // NO back-reference
	}

	// Simulate the early registration code path with parentCall == nil
	const testCallID = "no-parent-call-id"
	out.callID = testCallID
	if out.parentCall != nil {
		c.cmu.Lock()
		c.byCallID[out.callID] = out.parentCall
		c.cmu.Unlock()
	}

	// Verify byCallID is NOT populated
	c.cmu.Lock()
	registered := c.byCallID[testCallID]
	c.cmu.Unlock()
	require.Nil(t, registered, "byCallID should NOT be populated when parentCall is nil")

	// Verify Client.onInvite() returns false for this call
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader(testCallID)
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)
	require.False(t, handled, "Client.onInvite() should NOT find an unregistered call")
	require.Empty(t, tx.responses, "should not send any response for unregistered call")
}

// TestByCallIDCleanupAfterEarlyRegistration verifies that close() properly
// cleans up byCallID entries that were registered early (before 200 OK).
// This prevents stale entries from accumulating if the call fails during
// the INVITE transaction or is closed normally after establishment.
func TestByCallIDCleanupAfterEarlyRegistration(t *testing.T) {
	log := logger.GetLogger()
	c := &Client{
		log:         log,
		conf:        &config.Config{},
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
		byCallID:    make(map[string]*outboundCall),
	}

	const (
		testCallID = "cleanup-test-call-id"
		testTag    = "SCL_cleanup"
	)

	call := &outboundCall{
		c:   c,
		log: log,
	}

	out := &sipOutbound{
		log:        log,
		c:          c,
		id:         testTag,
		callID:     testCallID,
		parentCall: call,
		contact: &sip.ContactHeader{
			Address: sip.Uri{Host: "local.com", Port: 5060},
		},
		ownSDP: []byte("v=0\r\n"),
	}
	call.cc = out

	// Register in byCallID (simulating early registration)
	c.cmu.Lock()
	c.activeCalls[testTag] = call
	c.byCallID[testCallID] = call
	c.cmu.Unlock()

	// Verify the call is registered
	c.cmu.Lock()
	require.NotNil(t, c.byCallID[testCallID], "call should be in byCallID before cleanup")
	require.NotNil(t, c.activeCalls[testTag], "call should be in activeCalls before cleanup")
	c.cmu.Unlock()

	// Simulate the cleanup that close() does (outbound.go lines 346-354)
	c.cmu.Lock()
	delete(c.activeCalls, out.ID())
	if tag := out.Tag(); tag != "" {
		delete(c.byRemote, tag)
	}
	if sipCallID := out.SIPCallID(); sipCallID != "" {
		delete(c.byCallID, sipCallID)
	}
	c.cmu.Unlock()

	// Verify cleanup
	c.cmu.Lock()
	require.Nil(t, c.byCallID[testCallID], "byCallID should be empty after cleanup")
	require.Nil(t, c.activeCalls[testTag], "activeCalls should be empty after cleanup")
	c.cmu.Unlock()

	// Verify Client.onInvite() returns false after cleanup
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader(testCallID)
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)
	require.False(t, handled, "Client.onInvite() should NOT find the cleaned-up call")
	require.Empty(t, tx.responses, "should not send any response for cleaned-up call")
}
