// Package capture is the audio capture abstraction for captionbridge-media
// (buildplan.md §6.1). Capture is an interface with a synthetic-tone source so the
// pipeline can be smoke-tested end-to-end locally with no hardware; the real ALSA
// source is hardware-gated (BLOCKERS B3/B4) and lives behind the same Source seam.
//
// Captured audio is transient CONTENT: it lives only in memory and is never
// persisted or logged (guardrail #5, §9.4).
package capture

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Frame is a block of interleaved float64 samples in [-1, 1].
type Frame struct {
	Samples    []float64
	SampleRate int
	Channels   int
}

// Source produces capture frames until closed. Start returns a channel that is
// closed when the source stops (ctx cancel or Close) — a closed channel is the
// device telling the pipeline it has stopped delivering audio, never a reason to
// keep serving the last healthy verdict (guardrail #7).
//
// A Source MAY additionally implement HealthReporter (health.go) to report real
// presence/connection state; the pipeline prefers it when present.
type Source interface {
	Start(ctx context.Context) (<-chan Frame, error)
	Describe() string
	Close() error
}

// SyntheticTone is a deterministic sine-wave Source used to smoke-test the
// capture → qualify → map → encode chain without hardware (kickoff item 3).
type SyntheticTone struct {
	FreqHz     float64
	Amplitude  float64 // 0..1
	SampleRate int
	Channels   int
	FrameSize  int // samples per channel per frame
	// Realtime paces frame delivery at the frame's own duration instead of
	// generating as fast as the consumer reads. A stand-in that feeds the
	// §6.7 encoder MUST be paced: an unpaced tone delivers an hour of audio in
	// seconds, so the Opus stream a patron receives would play at many times
	// speed and every wall-clock measurement taken around it (capture gaps,
	// dropouts, encoder throughput) would describe the generator's speed rather
	// than a booth feed. Tests that only want N blocks leave it off.
	Realtime bool

	phase float64

	// healthMu guards the device-state fields below. Start runs on the pipeline's
	// Run goroutine while Close is called from whoever is shutting the stream
	// down (Stream.Stop), so the cancel func is shared state, not local state.
	healthMu sync.Mutex
	cancel   context.CancelFunc
	started  bool
	closed   bool
}

// Describe returns a human description (no content).
func (s *SyntheticTone) Describe() string {
	return fmt.Sprintf("synthetic-tone %gHz amp=%g %dch@%dHz", s.FreqHz, s.Amplitude, s.Channels, s.SampleRate)
}

// Start begins generating frames. Without Realtime it produces frames as fast as
// the consumer reads (for tests: read a bounded number then cancel); with
// Realtime it delivers one frame per frame-duration of wall clock.
func (s *SyntheticTone) Start(ctx context.Context) (<-chan Frame, error) {
	if s.SampleRate <= 0 {
		s.SampleRate = 48000
	}
	if s.Channels <= 0 {
		s.Channels = 1
	}
	if s.FrameSize <= 0 {
		s.FrameSize = 480 // 10 ms @ 48 kHz
	}
	ctx, cancel := context.WithCancel(ctx)
	s.healthMu.Lock()
	s.started, s.closed, s.cancel = true, false, cancel
	s.healthMu.Unlock()
	out := make(chan Frame)
	step := 2 * math.Pi * s.FreqHz / float64(s.SampleRate)
	period := time.Duration(s.FrameSize) * time.Second / time.Duration(s.SampleRate)
	go func() {
		defer close(out)
		next := time.Now()
		for {
			buf := make([]float64, s.FrameSize*s.Channels)
			for i := 0; i < s.FrameSize; i++ {
				v := s.Amplitude * math.Sin(s.phase)
				s.phase += step
				if s.phase > 2*math.Pi {
					s.phase -= 2 * math.Pi
				}
				for c := 0; c < s.Channels; c++ {
					buf[i*s.Channels+c] = v
				}
			}
			select {
			case out <- Frame{Samples: buf, SampleRate: s.SampleRate, Channels: s.Channels}:
			case <-ctx.Done():
				return
			}
			if !s.Realtime || period <= 0 {
				continue
			}
			// Pace against a running deadline rather than sleeping a period per
			// frame, so scheduling jitter does not accumulate into drift. A stall
			// long enough to fall a whole second behind resets the deadline
			// instead of sprinting to catch up: catching up would hand the
			// encoder a burst of audio that never existed in real time.
			next = next.Add(period)
			d := time.Until(next)
			if d < -time.Second {
				next = time.Now()
				continue
			}
			if d <= 0 {
				continue
			}
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			}
		}
	}()
	return out, nil
}

// Close stops the source. The Health seam then reports it as disconnected, so a
// stopped source surfaces as a failed input rather than a frozen verdict (§6.6).
func (s *SyntheticTone) Close() error {
	s.healthMu.Lock()
	s.closed = true
	cancel := s.cancel
	s.healthMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
