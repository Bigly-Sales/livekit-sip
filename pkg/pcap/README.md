# PCAP Capture for LiveKit SIP

This package provides per-session PCAP capture functionality for debugging SIP calls, similar to what's available in LiveKit Cloud.

## Features

- **Per-session capture**: Each SIP call gets its own PCAP file
- **RTP capture**: Captures all RTP media packets (audio, DTMF)
- **SIP signaling capture**: Captures SIP messages (INVITE, ACK, BYE, etc.)
- **Organized storage**: Files organized by project ID and date
- **Size limits**: Configurable maximum file size to prevent disk exhaustion
- **Protocol filtering**: Enable/disable SIP and RTP capture independently

## Configuration

Add the following to your SIP server configuration YAML:

```yaml
pcap:
  # Enable PCAP capture
  enabled: true
  
  # Base directory for PCAP files
  directory: /var/log/livekit-sip/pcap
  
  # Maximum file size in bytes (0 = unlimited)
  max_file_size: 104857600  # 100MB
  
  # Capture SIP signaling messages
  capture_sip: true
  
  # Capture RTP media packets
  capture_rtp: true
  
  # Enable gzip compression (not yet implemented)
  compression: false
```

## File Organization

PCAP files are organized in the following structure:

```
<directory>/
└── <project_id>/
    └── <date>/
        └── <call_id>_<timestamp>.pcap
```

For example:
```
/var/log/livekit-sip/pcap/
└── proj_abc123/
    └── 2024-12-19/
        └── CL_xyz789_153045.pcap
```

## Analyzing PCAP Files

### Using Wireshark

1. Open the PCAP file in Wireshark
2. For SIP analysis: `Telephony → VoIP Calls`
3. For RTP analysis: `Telephony → RTP → RTP Streams`
4. For call flow: `Telephony → VoIP Calls → Flow Sequence`

### Using tshark (CLI)

```bash
# View SIP messages
tshark -r call.pcap -Y "sip"

# View RTP streams
tshark -r call.pcap -Y "rtp"

# Extract SIP INVITE requests
tshark -r call.pcap -Y "sip.Method == INVITE" -V
```

## Handler Integration

When a call ends, the `OnSessionEnd` handler receives the PCAP file path in the `CallIdentifier`:

```go
func (s *Service) OnSessionEnd(ctx context.Context, callIdentifier *sip.CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
    if callIdentifier.PCAPPath != "" {
        s.log.Infow("PCAP file available",
            "callID", callInfo.CallId,
            "pcapPath", callIdentifier.PCAPPath,
        )
        // Upload to cloud storage, process, etc.
    }
}
```

## Technical Details

### PCAP Format

The package generates standard PCAP files with:
- Magic number: `0xa1b2c3d4` (microsecond resolution)
- Link type: Raw IP (101)
- Snap length: 65535 bytes

### Packet Encapsulation

UDP packets (SIP and RTP) are encapsulated in IP headers:
- IP version: 4
- Protocol: UDP (17)
- TTL: 64
- Checksums: Not calculated (set to 0)

### Thread Safety

All PCAP operations are thread-safe using mutex locks.

## Performance Considerations

- PCAP capture adds minimal overhead to call processing
- Consider disk I/O impact for high-volume deployments
- Use `max_file_size` to prevent disk exhaustion on long calls
- Disable RTP capture (`capture_rtp: false`) if only SIP signaling is needed

