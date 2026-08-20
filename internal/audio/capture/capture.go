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

// Start begins generating frames. It produces frames as fast as the consumer
// reads; for tests, read a bounded number then cancel.
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
	go func() {
		defer close(out)
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
