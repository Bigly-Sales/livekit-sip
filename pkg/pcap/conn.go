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
	"net/netip"
	"sync/atomic"
)

// CaptureConn wraps a UDP connection to capture packets
type CaptureConn struct {
	conn   UDPConn
	writer atomic.Pointer[SessionWriter]
	proto  Protocol

	localAddr  net.Addr
	localIP    net.IP
	localPort  int
}

// UDPConn is the interface for UDP connections that can be wrapped
type UDPConn interface {
	net.Conn
	ReadFromUDPAddrPort(b []byte) (n int, addr netip.AddrPort, err error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

// NewCaptureConn creates a new packet-capturing UDP connection wrapper
func NewCaptureConn(conn UDPConn, proto Protocol) *CaptureConn {
	addr := conn.LocalAddr()
	udpAddr, _ := addr.(*net.UDPAddr)

	cc := &CaptureConn{
		conn:      conn,
		proto:     proto,
		localAddr: addr,
	}

	if udpAddr != nil {
		cc.localIP = udpAddr.IP
		cc.localPort = udpAddr.Port
	}

	return cc
}

// SetWriter sets the PCAP session writer for capture
func (c *CaptureConn) SetWriter(w *SessionWriter) {
	if w != nil {
		c.writer.Store(w)
	} else {
		c.writer.Store(nil)
	}
}

// Read reads from the connection and captures the packet
func (c *CaptureConn) Read(b []byte) (int, error) {
	n, _, err := c.ReadFromUDPAddrPort(b)
	return n, err
}

// ReadFromUDPAddrPort reads from the connection with address info
func (c *CaptureConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	n, addr, err := c.conn.ReadFromUDPAddrPort(b)
	if err != nil {
		return n, addr, err
	}

	// Capture inbound packet
	if w := c.writer.Load(); w != nil && n > 0 {
		srcIP := addr.Addr().AsSlice()
		srcPort := int(addr.Port())
		_ = w.WritePacket(b[:n], srcIP, srcPort, c.localIP, c.localPort, c.proto, DirectionInbound)
	}

	return n, addr, err
}

// Write writes to the connection and captures the packet
func (c *CaptureConn) Write(b []byte) (int, error) {
	// For Write without address, we can't capture properly
	return c.conn.Write(b)
}

// WriteToUDPAddrPort writes to the connection with address info
func (c *CaptureConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	// Capture outbound packet before writing
	if w := c.writer.Load(); w != nil && len(b) > 0 {
		dstIP := addr.Addr().AsSlice()
		dstPort := int(addr.Port())
		_ = w.WritePacket(b, c.localIP, c.localPort, dstIP, dstPort, c.proto, DirectionOutbound)
	}

	return c.conn.WriteToUDPAddrPort(b, addr)
}

// Close closes the underlying connection
func (c *CaptureConn) Close() error {
	return c.conn.Close()
}

// LocalAddr returns the local address
func (c *CaptureConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote address
func (c *CaptureConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets the deadline for the connection
func (c *CaptureConn) SetDeadline(t interface{ UnixNano() int64 }) error {
	return nil // Ignore for simplicity
}

// SetReadDeadline sets the read deadline
func (c *CaptureConn) SetReadDeadline(t interface{ UnixNano() int64 }) error {
	return nil
}

// SetWriteDeadline sets the write deadline
func (c *CaptureConn) SetWriteDeadline(t interface{ UnixNano() int64 }) error {
	return nil
}

