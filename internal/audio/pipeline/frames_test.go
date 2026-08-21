package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// TestPipelineSilenceCountsRealAudioTime pins the §6.3 duration rule to the
// audio that actually arrived rather than to a count of nominal windows.
//
// Frames are accumulated until the analysis window is FILLED, so a device whose
// capture period is longer than the window delivers blocks longer than it. The
// accumulator used to add one AnalysisWindow per block regardless, which
// stretches a configured sustained-silence threshold by the ratio between the
// two — here 4×, i.e. it delays a real mid-show fault instead of reporting it
// when the operator asked for it (§1.2 guardrail 7).
func TestPipelineSilenceCountsRealAudioTime(t *testing.T) {
	// ONE frame carrying a full 200 ms of digital silence, against a 50 ms
	// analysis window and a 200 ms sustained-silence threshold. That single
	// block is exactly the configured duration, so it must fail on its own.
	oneBlock := frames(1, silence(1), testRate, 1)
	oneBlock[0].Samples = make([]float64, testRate/5) // 200 ms @ 48 kHz, mono
	src := scripted(oneBlock)
	src.hold = true

	st := NewStream(src, nil, StreamConfig{
		DeviceID: "long-period-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
		Thresholds:        qualifier.Thresholds{SustainedSilenceMS: 200},
	})
	run(t, st)
	got := waitFor(t, st, "the sustained-silence threshold to be met by one 200 ms block",
		func(s model.AudioStreamState) bool { return settled(s) && s.Status == model.QualFailed })
	if !strings.Contains(strings.Join(got.Reasons, " "), "no signal at all") {
		t.Errorf("reasons %v should report the dead line", got.Reasons)
	}
}

// TestBlockDurationMS documents the block clock the duration rules are measured
// against, including the degenerate inputs that make it untimeable.
func TestBlockDurationMS(t *testing.T) {
	tests := []struct {
		name                          string
		samples, channels, sampleRate int
		want                          int
	}{
		{name: "200 ms mono block", samples: testRate / 5, channels: 1, sampleRate: testRate, want: 200},
		{name: "a stereo block covers half as much time per sample", samples: testRate / 5, channels: 2, sampleRate: testRate, want: 100},
		{name: "no channel count is untimeable", samples: 480, channels: 0, sampleRate: testRate, want: 0},
		{name: "no sample rate is untimeable", samples: 480, channels: 1, sampleRate: 0, want: 0},
		{name: "an empty block covers no time", samples: 0, channels: 1, sampleRate: testRate, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockDurationMS(tc.samples, tc.channels, tc.sampleRate); got != tc.want {
				t.Errorf("blockDurationMS(%d, %d, %d) = %d, want %d",
					tc.samples, tc.channels, tc.sampleRate, got, tc.want)
			}
		})
	}
}

// TestPipelineRejectsUndescribedFrames covers the quietest failure mode a
// capture chain has: a device that keeps DELIVERING frames the driver could not
// describe. Such frames cannot be de-interleaved, so nothing is ever qualified —
// but they keep feeding the stale-frame watchdog, so the stream would sit on its
// startup verdict ("not yet started") indefinitely while the booth sees a device
// that is plainly connected. §1.2 guardrail 7: a failure must surface loudly.
func TestPipelineRejectsUndescribedFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame capture.Frame
	}{
		{
			name:  "no channel count",
			frame: capture.Frame{Samples: make([]float64, 480), SampleRate: testRate, Channels: 0},
		},
		{
			name:  "no sample rate",
			frame: capture.Frame{Samples: make([]float64, 480), SampleRate: 0, Channels: 1},
		},
		{
			name:  "truncated interleaved buffer",
			frame: capture.Frame{Samples: make([]float64, 481), SampleRate: testRate, Channels: 2},
		},
		{
			name:  "no samples at all",
			frame: capture.Frame{Samples: nil, SampleRate: testRate, Channels: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := make([]capture.Frame, 30)
			for i := range script {
				script[i] = tc.frame
			}
			src := scripted(script)
			src.hold = true // the device stays plugged in and keeps delivering
			st := NewStream(src, nil, StreamConfig{
				DeviceID: "undescribed-0", Stream: model.StreamHI, Source: model.ChannelMono,
				AnalysisWindow:    50 * time.Millisecond,
				StaleFrameTimeout: 10 * time.Second, // the watchdog must NOT be what saves us
			})
			run(t, st)
			got := waitFor(t, st, "the malformed frames to be reported", func(s model.AudioStreamState) bool {
				return settled(s)
			})
			if got.Status != model.QualFailed {
				t.Fatalf("Status = %q, want failed; reasons=%v", got.Status, got.Reasons)
			}
			if !strings.Contains(strings.Join(got.Reasons, " "), "unusable frame") &&
				!strings.Contains(strings.Join(got.Reasons, " "), "fixed 48000 Hz") {
				t.Errorf("reasons %v should name the undescribed capture format", got.Reasons)
			}
		})
	}
}
