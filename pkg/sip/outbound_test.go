// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestOutboundReInviteHandling(t *testing.T) {
	// Test that RE-INVITE requests for outbound calls are properly handled.
	// Steps:
	// 1. Create an outbound call and establish it
	// 2. Simulate a RE-INVITE from the remote party
	// 3. Verify that the RE-INVITE is accepted with 200 OK and our SDP

	client := NewOutboundTestClient(t, TestClientConfig{})
	req := MinimalCreateSIPParticipantRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

	t.Log("Waiting for INVITE to be sent")

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected client to be created")
		return
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected transaction request to be created")
		return
	}

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.Equal(t, sip.INVITE, tr.req.Method)

	t.Log("INVITE received, sending 200 OK response")

	// Create minimal SDP response
	minimalSDP := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	response := sip.NewSDPResponseFromRequest(tr.req, minimalSDP)
	require.NotNil(t, response, "NewSDPResponseFromRequest returned nil")

	// Add To tag to complete the dialog
	if response.To() != nil && response.To().Params != nil {
		response.To().Params.Add("tag", "remote-tag-123")
	}

	tr.transaction.SendResponse(response)

	t.Log("Waiting for ACK")

	// Wait for ACK
	var ackReq *sipRequest
	select {
	case ackReq = <-sipClient.requests:
		require.Equal(t, sip.ACK, ackReq.req.Method)
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected ACK to be sent")
		return
	}

	t.Log("ACK received, call established")

	// Give some time for the call to be fully registered
	time.Sleep(50 * time.Millisecond)

	// Get the SIP Call-ID from the original INVITE
	sipCallID := tr.req.CallID().Value()
	t.Logf("SIP Call-ID: %s", sipCallID)

	// Verify the call is registered in byCallID
	client.cmu.Lock()
	call := client.byCallID[sipCallID]
	client.cmu.Unlock()
	require.NotNil(t, call, "outbound call should be registered in byCallID map")

	t.Log("Simulating RE-INVITE from remote")

	// Create a RE-INVITE request (simulating remote party sending session refresh)
	reInviteReq := sip.NewRequest(sip.INVITE, ackReq.req.Recipient)
	reInviteReq.AppendHeader(tr.req.CallID()) // Same Call-ID
	reInviteReq.AppendHeader(&sip.FromHeader{
		Address: response.To().Address,
		Params:  response.To().Params,
	})
	reInviteReq.AppendHeader(&sip.ToHeader{
		Address: tr.req.From().Address,
		Params:  tr.req.From().Params,
	})
	reInviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	reInviteReq.SetBody(minimalSDP)
	reInviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	// Test Client.onInvite handles the RE-INVITE
	mockTx := &mockServerTransaction{
		responses: make(chan *sip.Response, 1),
	}

	handled := client.onInvite(reInviteReq, mockTx)
	require.True(t, handled, "RE-INVITE should be handled by client")

	// Verify response was sent
	select {
	case resp := <-mockTx.responses:
		require.Equal(t, sip.StatusOK, resp.StatusCode, "RE-INVITE should be accepted with 200 OK")
		require.Contains(t, resp.ContentType().Value(), "application/sdp", "Response should contain SDP")
		require.NotEmpty(t, resp.Body(), "Response should have SDP body")
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "expected 200 OK response to RE-INVITE")
	}

	t.Log("RE-INVITE handled successfully")
	cancel()
}

func TestOutboundReInviteUnknownCall(t *testing.T) {
	// Test that RE-INVITE for unknown calls returns false (not handled)

	client := NewOutboundTestClient(t, TestClientConfig{})

	// Create a RE-INVITE request for a non-existent call
	reInviteReq := sip.NewRequest(sip.INVITE, sip.Uri{Host: "example.com"})
	callID := sip.CallIDHeader("unknown-call-id-12345")
	reInviteReq.AppendHeader(&callID)
	reInviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})

	mockTx := &mockServerTransaction{
		responses: make(chan *sip.Response, 1),
	}

	handled := client.onInvite(reInviteReq, mockTx)
	require.False(t, handled, "RE-INVITE for unknown call should not be handled")
}

func TestOutboundByCallIDCleanup(t *testing.T) {
	// Test that byCallID is properly cleaned up when call is closed

	client := NewOutboundTestClient(t, TestClientConfig{})
	req := MinimalCreateSIPParticipantRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected client to be created")
		return
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected transaction request to be created")
		return
	}

	// Send 200 OK to establish the call
	minimalSDP := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	response := sip.NewSDPResponseFromRequest(tr.req, minimalSDP)
	if response.To() != nil && response.To().Params != nil {
		response.To().Params.Add("tag", "remote-tag-456")
	}
	tr.transaction.SendResponse(response)

	// Wait for ACK
	select {
	case <-sipClient.requests:
		// ACK received
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected ACK to be sent")
		return
	}

	// Give time for registration
	time.Sleep(50 * time.Millisecond)

	sipCallID := tr.req.CallID().Value()

	// Verify call is registered
	client.cmu.Lock()
	call := client.byCallID[sipCallID]
	client.cmu.Unlock()
	require.NotNil(t, call, "call should be registered in byCallID")

	// Close the call
	call.Close()

	// Wait a bit for cleanup
	time.Sleep(50 * time.Millisecond)

	// Verify call is removed from byCallID
	client.cmu.Lock()
	callAfterClose := client.byCallID[sipCallID]
	client.cmu.Unlock()
	require.Nil(t, callAfterClose, "call should be removed from byCallID after close")

	cancel()
}

// mockServerTransaction implements sip.ServerTransaction for testing
type mockServerTransaction struct {
	responses chan *sip.Response
}

func (m *mockServerTransaction) Respond(res *sip.Response) error {
	select {
	case m.responses <- res:
		return nil
	default:
		return fmt.Errorf("response channel full")
	}
}

func (m *mockServerTransaction) Acks() <-chan *sip.Request {
	return nil
}

func (m *mockServerTransaction) Cancels() <-chan *sip.Request {
	return nil
}

func (m *mockServerTransaction) Done() <-chan struct{} {
	return nil
}

func (m *mockServerTransaction) Err() error {
	return nil
}

func (m *mockServerTransaction) Terminate() {
}

func (m *mockServerTransaction) Key() string {
	return ""
}

func (m *mockServerTransaction) Origin() *sip.Request {
	return nil
}

func TestOutboundRouteHeaderWithRecordRoute(t *testing.T) {
	// Make sure the ACK doesn't carry over initial Route header.
	// Steps:
	// 1. Create a SIP participant with an initial Route header.
	// 2. Make sure the Route header is properly populates in INVITE.
	// 3. Fake a 200 response with Record Route headers.
	// 4. Make sure the ACK doesn't carry over initial Route header..

	// Plumbing
	initialRouteURI := sip.Uri{Host: "initial-header.com", UriParams: sip.HeaderParams{"lr": ""}}
	addedRouteURI := sip.Uri{Host: "added-header.com", UriParams: sip.HeaderParams{"lr": ""}}
	initialRouteHeader := sip.RouteHeader{Address: initialRouteURI}
	addedRouteHeader := sip.RouteHeader{Address: addedRouteURI}
	client := NewOutboundTestClient(t, TestClientConfig{})
	req := MinimalCreateSIPParticipantRequest()
	req.Headers = map[string]string{
		"Route": initialRouteHeader.Value(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // Allow test to continue
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			// Only log error if context wasn't cancelled
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

	t.Log("Waiting for INVITE to be sent")

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected client to be created")
		return
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected transaction request to be created")
		return
	}

	fmt.Println("Received INVITE, validating")

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.NotNil(t, tr.transaction)
	require.Equal(t, sip.INVITE, tr.req.Method)
	routeHeaders := tr.req.GetHeaders("Route")
	require.Equal(t, 1, len(routeHeaders))
	require.Equal(t, initialRouteHeader.Value(), routeHeaders[0].Value())

	t.Log("INVITE okay, sending fake response")

	minimalSDP := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	response := sip.NewSDPResponseFromRequest(tr.req, minimalSDP)
	require.NotNil(t, response, "NewSDPResponseFromRequest returned nil")
	response.RemoveHeader("Record-Route")
	rr1 := sip.RecordRouteHeader{Address: addedRouteURI}
	rr2 := sip.RecordRouteHeader{Address: initialRouteURI}
	response.AppendHeader(&rr1)
	response.AppendHeader(&rr2)
	tr.transaction.SendResponse(response)

	t.Log("Wait for ACK to be sent")

	// Make sure ACK is okay
	var ackReq *sipRequest
	select {
	case ackReq = <-sipClient.requests:
		// All good
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected ACK request to be created")
		return
	}

	t.Log("Received ACK, validating")

	require.NotNil(t, ackReq)
	require.NotNil(t, ackReq.req)
	require.Equal(t, sip.ACK, ackReq.req.Method)
	require.Equal(t, tr.req.CSeq().SeqNo, ackReq.req.CSeq().SeqNo)
	require.Equal(t, tr.req.CallID(), ackReq.req.CallID())
	ackRouteHeaders := ackReq.req.GetHeaders("Route")
	require.Equal(t, 2, len(ackRouteHeaders)) // We expect this to fail prior to fixing our bug!
	require.Equal(t, initialRouteHeader.Value(), ackRouteHeaders[0].Value())
	require.Equal(t, addedRouteHeader.Value(), ackRouteHeaders[1].Value())

	cancel()
}
