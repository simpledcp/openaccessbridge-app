package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/model"
)

// TestPipelineSyntheticToneQualifies runs the full capture→map→qualify chain on a
// synthetic tone with no hardware and asserts a usable verdict is produced.
func TestPipelineSyntheticToneQualifies(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: 48000, Channels: 1, FrameSize: 480}
	st := NewStream(src, encode.NullBackend{}, StreamConfig{
		DeviceID: "synthetic-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.Run(ctx, true)

	// Wait for the chain to produce a qualified state.
	deadline := time.After(2 * time.Second)
	for {
		state := st.Status()
		if state.Status.Usable() && state.LevelDBFS < 0 {
			st.Stop()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("pipeline did not qualify a synthetic tone: %+v", st.Status())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestPipelineImpossibleMappingFails asserts a stereo-split mapping on a mono
// input is surfaced as Failed (pre-show, not mid-show — §6.5).
func TestPipelineImpossibleMappingFails(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: 48000, Channels: 1, FrameSize: 480}
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "mono-0", Stream: model.StreamHI, Source: model.ChannelLeft, RequiredChannels: 2,
		AnalysisWindow: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.Run(ctx, true)

	deadline := time.After(2 * time.Second)
	for {
		if st.Status().Status == model.QualFailed {
			st.Stop()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("impossible mapping should be Failed, got %+v", st.Status())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
