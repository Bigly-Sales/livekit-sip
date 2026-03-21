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

package sip

import (
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// expectedInputBufferFrames is the buffer size we configure in NewRoom.
// This must match the value passed to mixer.WithInputBufferFrames() in room.go.
const expectedInputBufferFrames = 15

// expectedInputBufferMin is frames/2 + 1, matching the mixer's formula.
const expectedInputBufferMin = expectedInputBufferFrames/2 + 1

// TestNewRoomMixerBufferConfig verifies that the room mixer is created with an
// increased input buffer (15 frames / 300ms) and that bursty input delivery
// does not cause mixer restarts (starvation events).
//
// Background: With the default 5-frame buffer, WebRTC Opus track delivery
// jitter caused 124-185 restarts per 2-min call, each producing a 60ms+
// silence gap heard as scratchy audio. The 15-frame buffer absorbs up to
// ~200ms of delivery jitter.
func TestNewRoomMixerBufferConfig(t *testing.T) {
	st := &RoomStats{}
	r := NewRoom(logger.GetLogger(), st)
	defer r.Close()

	inp := r.mix.NewInput()
	defer inp.Close()

	// samplesPerFrame at 48kHz with 20ms frame duration = 960
	const samplesPerFrame = 960

	frame := make(msdk.PCM16Sample, samplesPerFrame)
	for i := range frame {
		frame[i] = 1000
	}

	// Fill buffer to inputBufferMin to exit initial buffering.
	for i := 0; i < expectedInputBufferMin; i++ {
		err := inp.WriteSample(frame)
		require.NoError(t, err)
	}

	// Allow mixer ticker to consume some frames.
	time.Sleep(80 * time.Millisecond)

	// Simulate bursty delivery: write a burst of frames, then pause briefly,
	// then another burst. With the old 5-frame buffer this would cause
	// starvation during the pause. With 15 frames we have headroom.
	for burst := 0; burst < 3; burst++ {
		// Write a burst of 5 frames quickly (simulates WebRTC delivering
		// multiple frames at once after a network hiccup).
		for i := 0; i < 5; i++ {
			err := inp.WriteSample(frame)
			require.NoError(t, err)
		}
		// Pause for 60ms — enough to starve a 5-frame buffer but not a 15-frame one.
		time.Sleep(60 * time.Millisecond)
	}

	// Give mixer time to process.
	time.Sleep(100 * time.Millisecond)

	// With a 15-frame buffer, bursts followed by 60ms pauses should not
	// cause any restarts. The buffer has enough headroom.
	restarts := st.Mixer.Restarts.Load()
	require.Zero(t, restarts,
		"expected zero mixer restarts with 15-frame buffer, got %d; "+
			"bursty delivery should be absorbed by the larger buffer", restarts)
}

// TestNewRoomMixerStarvationRecovery verifies that the mixer correctly detects
// starvation and recovers when input resumes after a long gap. This ensures
// the restart counter works and the mixer doesn't get stuck.
func TestNewRoomMixerStarvationRecovery(t *testing.T) {
	st := &RoomStats{}
	r := NewRoom(logger.GetLogger(), st)
	defer r.Close()

	inp := r.mix.NewInput()
	defer inp.Close()

	const samplesPerFrame = 960

	frame := make(msdk.PCM16Sample, samplesPerFrame)
	for i := range frame {
		frame[i] = 2000
	}

	// Fill buffer and let mixer start consuming.
	for i := 0; i < expectedInputBufferFrames; i++ {
		err := inp.WriteSample(frame)
		require.NoError(t, err)
	}

	// Wait long enough for the mixer to drain all frames and detect starvation.
	// With 15 frames at 20ms each = 300ms drain time, plus margin.
	time.Sleep(500 * time.Millisecond)

	// Should have at least one restart from the deliberate starvation.
	restarts := st.Mixer.Restarts.Load()
	require.Greater(t, restarts, uint64(0),
		"expected at least one restart after complete input starvation")

	// Now resume writing — mixer should recover.
	restartsBefore := restarts
	for i := 0; i < expectedInputBufferFrames; i++ {
		err := inp.WriteSample(frame)
		require.NoError(t, err)
	}

	// Let mixer process the new data.
	time.Sleep(100 * time.Millisecond)

	// Verify mixer produced output (mixes counter should be advancing).
	mixes := st.Mixer.Mixes.Load()
	require.Greater(t, mixes, uint64(0),
		"expected mixer to produce output after recovery")

	// No additional restarts after we resumed feeding data.
	restartsAfter := st.Mixer.Restarts.Load()
	require.Equal(t, restartsBefore, restartsAfter,
		"expected no additional restarts after input resumed; got %d new restarts",
		restartsAfter-restartsBefore)
}

// TestNewRoomMixerOutputContinuity verifies that with the increased buffer,
// the mixer produces continuous output (no silence gaps) when input is
// delivered at a steady rate slightly jittered around 20ms.
func TestNewRoomMixerOutputContinuity(t *testing.T) {
	st := &RoomStats{}
	r := NewRoom(logger.GetLogger(), st)
	defer r.Close()

	inp := r.mix.NewInput()
	defer inp.Close()

	const samplesPerFrame = 960

	frame := make(msdk.PCM16Sample, samplesPerFrame)
	for i := range frame {
		frame[i] = 500
	}

	// Pre-fill buffer.
	for i := 0; i < expectedInputBufferMin; i++ {
		err := inp.WriteSample(frame)
		require.NoError(t, err)
	}

	// Simulate steady-ish delivery for 1 second with timing jitter.
	// Some frames arrive at 15ms, some at 25ms — mimicking real WebRTC.
	for i := 0; i < 50; i++ {
		err := inp.WriteSample(frame)
		require.NoError(t, err)
		if i%2 == 0 {
			time.Sleep(15 * time.Millisecond)
		} else {
			time.Sleep(25 * time.Millisecond)
		}
	}

	time.Sleep(100 * time.Millisecond)

	restarts := st.Mixer.Restarts.Load()
	outputFrames := st.Mixer.OutputFrames.Load()

	require.Zero(t, restarts,
		"expected zero restarts with jittered but continuous delivery, got %d", restarts)
	require.Greater(t, outputFrames, uint64(0),
		"expected mixer to produce output frames")
}
