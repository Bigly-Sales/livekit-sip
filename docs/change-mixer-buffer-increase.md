# Change Doc: Increase Mixer Input Buffer to Reduce Audio Artifacts

**Date:** 2026-03-20
**Branch:** biglysales-v1
**Author:** Biswajit Pain
**Related Issues:** livekit/sip#608, livekit/sip#348, livekit/agents#4026

---

## Problem

Users report scratchy/crackling noise after each word on SIP calls routed through the Kamailio B2BUA + RTPEngine + LiveKit SIP stack.

### Root Cause Analysis

A pcap-based investigation on 2026-03-20 isolated the problem to **LiveKit SIP's audio mixer** experiencing excessive input starvation ("restarts"). Key evidence:

| Metric | Before Fix (observed) | Healthy Target |
|--------|----------------------|----------------|
| Mixer restarts per 2-min call | 124-185 | < 5 |
| `input_frames_dropped` | 66-90 | 0 |
| `input_samples_dropped` | 63,360-86,400 (~1-1.8s audio) | 0 |
| Outbound RTP max delta | 278ms | < 30ms |
| Outbound RTP packet loss | 0.2% (12 pkts) | 0% |

**What happens on each restart:**
1. The mixer's ring buffer for an input runs empty (WebRTC Opus track delivery is bursty)
2. Mixer transitions the input to "buffering" mode
3. Mixer outputs **silence** for that input until `inputBufferMin` frames (60ms with default=3) accumulate
4. Audio resumes, but the 60ms+ gap produces an audible click/scratch

With 124-185 restarts per call, this creates roughly **one audio glitch every 0.7 seconds** — matching the user-reported "scratchy noise after each word."

### Verified: Kamailio and RTPEngine are clean

The pcap analysis confirmed:
- RTPEngine kernel forwarding preserves identical packet timing (distributions match on both legs)
- Kamailio→Trunk outbound RTP: 99%+ packets in 18-22ms range, 0% loss
- All RTP streams go through kernel (not userspace)
- The jitter/loss on outbound RTP originates from LiveKit SIP's mixer, not from network or Kamailio

### Verified: Trunk also contributes jitter (separate issue)

PureCallerID SBCs send bursty RTP inbound (mean jitter 7-10ms, max delta 140-249ms). This is mitigated by enabling `enable_jitter_buffer: true` in LiveKit SIP config. The mixer restart issue is independent — it affects the **outbound** direction (agent voice → phone caller).

---

## Change

Increase the mixer input buffer from the default 5 frames (100ms) to **15 frames (300ms)** in two locations:

### Files Modified

**`pkg/sip/room.go:208`** — Main room mixer (agent audio → SIP RTP output)
```go
// Before
r.mix, err = mixer.NewMixer(r.out, rtp.DefFrameDur, 1,
    mixer.WithStats(&st.Mixer), mixer.WithOutputChannel())

// After
r.mix, err = mixer.NewMixer(r.out, rtp.DefFrameDur, 1,
    mixer.WithStats(&st.Mixer), mixer.WithOutputChannel(),
    mixer.WithInputBufferFrames(15))
```

**`pkg/sip/media_port.go:801`** — DTMF audio mixer
```go
// Before
mix, err := mixer.NewMixer(audioOut, rtp.DefFrameDur, 1,
    mixer.WithOutputChannel())

// After
mix, err := mixer.NewMixer(audioOut, rtp.DefFrameDur, 1,
    mixer.WithOutputChannel(), mixer.WithInputBufferFrames(15))
```

### Buffer Arithmetic

| Parameter | Default (before) | New (after) |
|-----------|-----------------|-------------|
| `inputBufferFrames` | 5 | **15** |
| `inputBufferMin` (`frames/2 + 1`) | 3 | **8** |
| Max buffer depth | 100ms | **300ms** |
| Min buffer before playback | 60ms | **160ms** |
| Ring buffer size (at 48kHz, 960 samples/frame) | 4,800 samples | **14,400 samples** |
| Memory per input | ~9.6 KB | ~28.8 KB |

### Tradeoffs

| Aspect | Impact |
|--------|--------|
| **Latency** | +100-160ms added to agent→phone direction. Acceptable for conversational calls (human perception threshold ~150ms). |
| **Memory** | +19.2 KB per mixer input. Negligible even at hundreds of concurrent calls. |
| **Starvation tolerance** | Can absorb up to ~200ms of bursty WebRTC track delivery without restart. Previously only ~40ms. |
| **Recovery time** | After starvation, takes 160ms (8 frames) to resume vs 60ms (3 frames). Slightly longer silence gap per restart, but far fewer restarts overall. |

---

## Testing

### Unit Tests Added

**`pkg/sip/room_mixer_test.go`** — Verifies the room mixer uses the increased buffer:
- `TestNewRoomMixerBufferConfig` — Creates a Room, writes frames with gaps simulating bursty WebRTC delivery, verifies zero restarts with the larger buffer
- `TestNewRoomMixerStarvationRecovery` — Verifies the mixer recovers after genuine starvation (complete input silence) and that the restart counter increments correctly

### Manual Verification

1. Deploy patched binary to LiveKit SIP server
2. Trigger test calls through Kamailio
3. Verify in logs: `"restarts"` count should drop from 124-185 to < 10 per call
4. Verify: `jitterBuf: true` still active (independent config)
5. Listen for scratchy noise — should be eliminated or greatly reduced

---

## Rollback

Revert the two `WithInputBufferFrames(15)` additions. The mixer falls back to `DefaultInputBufferFrames = 5`.

---

## Upstream Status

This is a workaround for a known upstream issue:
- **livekit/sip#608** — "Outbound SIP calls have audio artifacts" (open, filed 2026-03-09)
- **livekit/sip#348** — "Audio stream stutter/lag when talking over SIP" (open, filed 2025-05)
- The buffer size is not exposed as a config option in upstream LiveKit SIP

When upstream fixes the root cause (likely by making the buffer configurable or improving the mixer's timing tolerance), this patch can be removed.
