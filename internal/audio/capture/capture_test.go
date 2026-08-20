package capture

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestSyntheticToneFrames(t *testing.T) {
	src := &SyntheticTone{FreqHz: 440, Amplitude: 0.5, SampleRate: 48000, Channels: 1, FrameSize: 480}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var maxAbs float64
	frames := 0
	for f := range ch {
		if f.SampleRate != 48000 || f.Channels != 1 || len(f.Samples) != 480 {
			t.Fatalf("bad frame: %+v len=%d", f, len(f.Samples))
		}
		for _, s := range f.Samples {
			if a := math.Abs(s); a > maxAbs {
				maxAbs = a
			}
		}
		frames++
		if frames >= 5 {
			cancel()
		}
	}
	if frames < 5 {
		t.Fatalf("expected >=5 frames, got %d", frames)
	}
	// Amplitude 0.5 sine: peak should approach 0.5, never exceed 1.
	if maxAbs > 1.0 || maxAbs < 0.4 {
		t.Fatalf("unexpected peak amplitude %v", maxAbs)
	}
}

func TestSyntheticToneStereo(t *testing.T) {
	src := &SyntheticTone{FreqHz: 1000, Amplitude: 0.3, SampleRate: 48000, Channels: 2, FrameSize: 240}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, _ := src.Start(ctx)
	f := <-ch
	if f.Channels != 2 || len(f.Samples) != 240*2 {
		t.Fatalf("stereo frame wrong: ch=%d len=%d", f.Channels, len(f.Samples))
	}
	cancel()
}

// TestSyntheticToneHealth asserts the §6.1 hot-plug seam: a Source that
// implements HealthReporter reports its real state, and a stopped source is
// reported as disconnected instead of continuing to look healthy.
func TestSyntheticToneHealth(t *testing.T) {
	src := &SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: 48000, Channels: 1, FrameSize: 480}
	var hr HealthReporter = src
	if h := hr.Health(); h.Present || h.Connected {
		t.Fatalf("an unstarted source must not claim to be connected: %+v", h)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := src.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := hr.Health(); !h.Present || !h.Connected {
		t.Fatalf("a running source should be present and connected: %+v", h)
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := hr.Health()
	if h.Present || h.Connected {
		t.Fatalf("a closed source must report disconnected: %+v", h)
	}
	if h.Detail == "" {
		t.Error("a disconnected source should explain why")
	}
}
