// Copyright 2024 LiveKit, Inc.
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

package pcap

import (
	"net"
	"sync/atomic"
)

// SIPCaptureHook provides a hook for capturing SIP messages.
// It can be used with sipgo's transport layer hooks.
type SIPCaptureHook struct {
	writer atomic.Pointer[SessionWriter]
}

// NewSIPCaptureHook creates a new SIP capture hook
func NewSIPCaptureHook() *SIPCaptureHook {
	return &SIPCaptureHook{}
}

// SetWriter sets the PCAP session writer
func (h *SIPCaptureHook) SetWriter(w *SessionWriter) {
	if w != nil {
		h.writer.Store(w)
	} else {
		h.writer.Store(nil)
	}
}

// CaptureInbound captures an inbound SIP message
func (h *SIPCaptureHook) CaptureInbound(msg []byte, srcAddr, dstAddr net.Addr) {
	w := h.writer.Load()
	if w == nil || len(msg) == 0 {
		return
	}

	srcIP, srcPort := parseAddr(srcAddr)
	dstIP, dstPort := parseAddr(dstAddr)

	_ = w.WritePacket(msg, srcIP, srcPort, dstIP, dstPort, ProtocolSIP, DirectionInbound)
}

// CaptureOutbound captures an outbound SIP message
func (h *SIPCaptureHook) CaptureOutbound(msg []byte, srcAddr, dstAddr net.Addr) {
	w := h.writer.Load()
	if w == nil || len(msg) == 0 {
		return
	}

	srcIP, srcPort := parseAddr(srcAddr)
	dstIP, dstPort := parseAddr(dstAddr)

	_ = w.WritePacket(msg, srcIP, srcPort, dstIP, dstPort, ProtocolSIP, DirectionOutbound)
}

// parseAddr extracts IP and port from a net.Addr
func parseAddr(addr net.Addr) (net.IP, int) {
	if addr == nil {
		return net.IPv4zero, 0
	}

	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP, a.Port
	case *net.TCPAddr:
		return a.IP, a.Port
	default:
		// Try to parse the string representation
		host, port, err := net.SplitHostPort(addr.String())
		if err != nil {
			return net.IPv4zero, 0
		}
		ip := net.ParseIP(host)
		if ip == nil {
			ip = net.IPv4zero
		}
		var portNum int
		for _, c := range port {
			if c >= '0' && c <= '9' {
				portNum = portNum*10 + int(c-'0')
			}
		}
		return ip, portNum
	}
}

// SIPMessageCapture is a convenience type for capturing SIP messages
// when you have access to message bytes and addressing info directly.
type SIPMessageCapture struct {
	writer *SessionWriter
}

// NewSIPMessageCapture creates a new SIP message capture helper
func NewSIPMessageCapture(w *SessionWriter) *SIPMessageCapture {
	return &SIPMessageCapture{writer: w}
}

// Capture captures a SIP message
func (c *SIPMessageCapture) Capture(msg []byte, srcIP net.IP, srcPort int, dstIP net.IP, dstPort int, dir Direction) {
	if c == nil || c.writer == nil {
		return
	}
	_ = c.writer.WritePacket(msg, srcIP, srcPort, dstIP, dstPort, ProtocolSIP, dir)
}

