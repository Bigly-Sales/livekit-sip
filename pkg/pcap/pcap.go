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

// Package pcap provides per-session PCAP capture functionality for SIP calls.
// It captures both SIP signaling and RTP media traffic for debugging and analysis.
package pcap

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

const (
	// PCAP file magic number (microseconds resolution)
	pcapMagicNumber uint32 = 0xa1b2c3d4
	// PCAP version
	pcapVersionMajor uint16 = 2
	pcapVersionMinor uint16 = 4
	// Link type for raw IP
	linkTypeRawIP uint32 = 101
	// Link type for Ethernet (we'll use this for UDP encapsulation)
	linkTypeEthernet uint32 = 1
	// Max packet length to capture
	snapLen uint32 = 65535
)

// Direction indicates the packet direction
type Direction int

const (
	DirectionInbound  Direction = 0
	DirectionOutbound Direction = 1
)

// Protocol indicates the packet protocol
type Protocol int

const (
	ProtocolSIP Protocol = 0
	ProtocolRTP Protocol = 1
)

// Config holds PCAP capture configuration
type Config struct {
	// Enabled enables PCAP capture
	Enabled bool `yaml:"enabled"`
	// Directory is the base directory for PCAP files
	Directory string `yaml:"directory"`
	// MaxFileSize is the maximum size of a single PCAP file in bytes (0 = unlimited)
	MaxFileSize int64 `yaml:"max_file_size"`
	// CaptureSIP enables SIP signaling capture
	CaptureSIP bool `yaml:"capture_sip"`
	// CaptureRTP enables RTP media capture
	CaptureRTP bool `yaml:"capture_rtp"`
	// Compression enables gzip compression of PCAP files
	Compression bool `yaml:"compression"`
}

// DefaultConfig returns default PCAP configuration
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		Directory:   "/var/log/livekit-sip/pcap",
		MaxFileSize: 100 * 1024 * 1024, // 100MB
		CaptureSIP:  true,
		CaptureRTP:  true,
		Compression: false,
	}
}

// SessionWriter handles PCAP capture for a single SIP session
type SessionWriter struct {
	log       logger.Logger
	config    Config
	sessionID string
	callID    string
	sipCallID string
	projectID string

	mu       sync.Mutex
	file     *os.File
	writer   io.Writer
	filePath string
	size     int64
	closed   bool

	startTime time.Time

	// Packet counters for stats
	sipPackets uint64
	rtpPackets uint64
}

// NewSessionWriter creates a new PCAP writer for a session
func NewSessionWriter(log logger.Logger, config Config, sessionID, callID, sipCallID, projectID string) (*SessionWriter, error) {
	if !config.Enabled {
		return nil, nil
	}

	sw := &SessionWriter{
		log:       log,
		config:    config,
		sessionID: sessionID,
		callID:    callID,
		sipCallID: sipCallID,
		projectID: projectID,
		startTime: time.Now(),
	}

	if err := sw.openFile(); err != nil {
		return nil, err
	}

	log.Infow("PCAP capture started",
		"sessionID", sessionID,
		"callID", callID,
		"sipCallID", sipCallID,
		"path", sw.filePath,
	)

	return sw, nil
}

// openFile creates and opens the PCAP file
func (sw *SessionWriter) openFile() error {
	// Create directory structure: <base>/<projectID>/<date>/<callID>.pcap
	dateDir := time.Now().Format("2006-01-02")
	dir := filepath.Join(sw.config.Directory, sw.projectID, dateDir)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create PCAP directory: %w", err)
	}

	// Generate filename: <callID>_<timestamp>.pcap
	timestamp := time.Now().Format("150405")
	filename := fmt.Sprintf("%s_%s.pcap", sw.callID, timestamp)
	sw.filePath = filepath.Join(dir, filename)

	file, err := os.OpenFile(sw.filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create PCAP file: %w", err)
	}
	sw.file = file
	sw.writer = file

	// Write PCAP global header
	if err := sw.writeGlobalHeader(); err != nil {
		file.Close()
		os.Remove(sw.filePath)
		return fmt.Errorf("failed to write PCAP header: %w", err)
	}

	return nil
}

// writeGlobalHeader writes the PCAP global header
func (sw *SessionWriter) writeGlobalHeader() error {
	header := make([]byte, 24)

	// Magic number
	binary.LittleEndian.PutUint32(header[0:4], pcapMagicNumber)
	// Version
	binary.LittleEndian.PutUint16(header[4:6], pcapVersionMajor)
	binary.LittleEndian.PutUint16(header[6:8], pcapVersionMinor)
	// Timezone offset (GMT)
	binary.LittleEndian.PutUint32(header[8:12], 0)
	// Timestamp accuracy
	binary.LittleEndian.PutUint32(header[12:16], 0)
	// Snap length
	binary.LittleEndian.PutUint32(header[16:20], snapLen)
	// Link type (Raw IP)
	binary.LittleEndian.PutUint32(header[20:24], linkTypeRawIP)

	n, err := sw.writer.Write(header)
	if err != nil {
		return err
	}
	sw.size += int64(n)
	return nil
}

// WritePacket writes a packet to the PCAP file
func (sw *SessionWriter) WritePacket(data []byte, srcIP net.IP, srcPort int, dstIP net.IP, dstPort int, proto Protocol, dir Direction) error {
	if sw == nil || !sw.config.Enabled {
		return nil
	}

	// Check protocol filter
	if proto == ProtocolSIP && !sw.config.CaptureSIP {
		return nil
	}
	if proto == ProtocolRTP && !sw.config.CaptureRTP {
		return nil
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}

	// Check file size limit
	if sw.config.MaxFileSize > 0 && sw.size >= sw.config.MaxFileSize {
		sw.log.Warnw("PCAP file size limit reached", nil,
			"path", sw.filePath,
			"size", sw.size,
			"limit", sw.config.MaxFileSize,
		)
		return nil
	}

	// Build IP/UDP packet
	packet := sw.buildUDPPacket(data, srcIP, srcPort, dstIP, dstPort)

	// Write packet header
	now := time.Now()
	if err := sw.writePacketHeader(now, len(packet)); err != nil {
		return err
	}

	// Write packet data
	n, err := sw.writer.Write(packet)
	if err != nil {
		return err
	}
	sw.size += int64(n)

	// Update counters
	if proto == ProtocolSIP {
		sw.sipPackets++
	} else if proto == ProtocolRTP {
		sw.rtpPackets++
	}

	return nil
}

// writePacketHeader writes the PCAP packet header
func (sw *SessionWriter) writePacketHeader(ts time.Time, length int) error {
	header := make([]byte, 16)

	// Timestamp seconds
	binary.LittleEndian.PutUint32(header[0:4], uint32(ts.Unix()))
	// Timestamp microseconds
	binary.LittleEndian.PutUint32(header[4:8], uint32(ts.Nanosecond()/1000))
	// Captured length
	binary.LittleEndian.PutUint32(header[8:12], uint32(length))
	// Original length
	binary.LittleEndian.PutUint32(header[12:16], uint32(length))

	n, err := sw.writer.Write(header)
	if err != nil {
		return err
	}
	sw.size += int64(n)
	return nil
}

// buildUDPPacket builds an IP/UDP packet from payload data
func (sw *SessionWriter) buildUDPPacket(payload []byte, srcIP net.IP, srcPort int, dstIP net.IP, dstPort int) []byte {
	// Normalize IPs to IPv4
	srcIP4 := srcIP.To4()
	dstIP4 := dstIP.To4()
	if srcIP4 == nil {
		srcIP4 = net.IPv4zero.To4()
	}
	if dstIP4 == nil {
		dstIP4 = net.IPv4zero.To4()
	}

	udpLen := 8 + len(payload)
	ipLen := 20 + udpLen

	packet := make([]byte, ipLen)

	// IP Header (20 bytes)
	packet[0] = 0x45                                       // Version (4) + IHL (5)
	packet[1] = 0x00                                       // DSCP + ECN
	binary.BigEndian.PutUint16(packet[2:4], uint16(ipLen)) // Total length
	binary.BigEndian.PutUint16(packet[4:6], 0)             // Identification
	binary.BigEndian.PutUint16(packet[6:8], 0x4000)        // Flags + Fragment offset (Don't fragment)
	packet[8] = 64                                         // TTL
	packet[9] = 17                                         // Protocol (UDP)
	binary.BigEndian.PutUint16(packet[10:12], 0)           // Checksum (will be 0, not calculated)
	copy(packet[12:16], srcIP4)                            // Source IP
	copy(packet[16:20], dstIP4)                            // Destination IP

	// UDP Header (8 bytes)
	binary.BigEndian.PutUint16(packet[20:22], uint16(srcPort)) // Source port
	binary.BigEndian.PutUint16(packet[22:24], uint16(dstPort)) // Destination port
	binary.BigEndian.PutUint16(packet[24:26], uint16(udpLen))  // Length
	binary.BigEndian.PutUint16(packet[26:28], 0)               // Checksum (0 = not calculated)

	// Payload
	copy(packet[28:], payload)

	return packet
}

// Close closes the PCAP writer
func (sw *SessionWriter) Close() error {
	if sw == nil {
		return nil
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}
	sw.closed = true

	duration := time.Since(sw.startTime)

	sw.log.Infow("PCAP capture complete",
		"sessionID", sw.sessionID,
		"callID", sw.callID,
		"path", sw.filePath,
		"size", sw.size,
		"sipPackets", sw.sipPackets,
		"rtpPackets", sw.rtpPackets,
		"duration", duration,
	)

	if sw.file != nil {
		return sw.file.Close()
	}
	return nil
}

// FilePath returns the path to the PCAP file
func (sw *SessionWriter) FilePath() string {
	if sw == nil {
		return ""
	}
	return sw.filePath
}

// Stats returns capture statistics
func (sw *SessionWriter) Stats() (sipPackets, rtpPackets uint64, size int64) {
	if sw == nil {
		return 0, 0, 0
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.sipPackets, sw.rtpPackets, sw.size
}
