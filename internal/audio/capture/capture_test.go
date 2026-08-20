package capture

import (
	"context"
	"math"
	"sync"
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

// TestSyntheticToneStartCloseAreRaceFree pins the concurrency contract the
// pipeline actually relies on: Stream.Run calls Start on its own goroutine while
// Stream.Stop calls Close from whoever is shutting the appliance down (see
// cmd/captionbridge-media, which cancels and stops from the signal handler). The
// cancel func is therefore shared state and must be guarded — it previously was
// not, and `go test -race` reported a write/read race between the two.
func TestSyntheticToneStartCloseAreRaceFree(t *testing.T) {
	for i := 0; i < 100; i++ {
		src := &SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: 48000, Channels: 1, FrameSize: 480}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := src.Start(ctx); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := src.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			// Health must stay readable from a third caller (the pipeline polls
			// it every analysis block) while both of the above are in flight.
			_ = src.Health()
		}()
		wg.Wait()
		cancel()
	}
}

// A stand-in that FEEDS AN ENCODER has to run at 1x (§6.7). Unpaced, the source
// generates as fast as the consumer reads, so a patron would receive minutes of
// audio in seconds — and every wall-clock figure measured around it (capture
// gaps, dropouts, encoder throughput) would describe the generator rather than a
// booth feed. Tests that only want N blocks still get the unpaced source.
func TestRealtimeToneIsPacedAtOneTimesSpeed(t *testing.T) {
	const frameSize, rate, want = 480, 48000, 10 // 10 ms frames, 100 ms of audio
	src := &SyntheticTone{
		FreqHz: 440, Amplitude: 0.3, SampleRate: rate, Channels: 1,
		FrameSize: frameSize, Realtime: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := time.Now()
	for i := 0; i < want; i++ {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("the source stopped early")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d frames arrived in 3s", i)
		}
	}
	elapsed := time.Since(start)
	audio := time.Duration(want) * time.Duration(frameSize) * time.Second / time.Duration(rate)
	// Generous lower bound (scheduling jitter, a loaded box): the point is that
	// 100 ms of audio does not arrive in microseconds.
	if elapsed < audio/2 {
		t.Fatalf("%s of audio was delivered in %s: the source is not paced in real time", audio, elapsed)
	}
	if elapsed > 4*audio {
		t.Fatalf("%s of audio took %s to deliver: the pacing is drifting badly", audio, elapsed)
	}
}

// Without Realtime the source stays as fast as the consumer, which is what the
// qualifier's block-count tests depend on.
func TestUnpacedToneStaysFast(t *testing.T) {
	src := &SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: 48000, Channels: 1, FrameSize: 480}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		<-ch
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("100 unpaced frames took %s", elapsed)
	}
}
