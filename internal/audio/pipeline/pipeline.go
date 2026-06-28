// Package pipeline assembles the captionbridge-media chain (buildplan.md §6):
// capture → channel mapper → four-state qualifier → Opus encode. It runs one
// independent chain per named stream (HI, VI-N) so the two are fully independent
// (§6.1), and continuously reports each stream's transient AudioStreamState (no
// audio samples are ever stored or logged — guardrail #5, §9.4).
package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/model"
)

// StreamConfig configures one named audio stream.
type StreamConfig struct {
	DeviceID         string
	Stream           model.StreamType
	Source           model.SourceChannel
	RequiredChannels int // mapping requirement (e.g. 2 for a stereo split)
	// AnalysisWindow is how much audio each qualification block covers.
	AnalysisWindow time.Duration
}

// Stream runs one capture→map→qualify chain and exposes its latest state.
type Stream struct {
	cfg     StreamConfig
	src     capture.Source
	backend encode.Backend
	th      qualifier.Thresholds

	mu        sync.Mutex
	state     model.AudioStreamState
	latencyMS int
}

// NewStream builds a Stream. backend may be nil (no encode smoke).
func NewStream(src capture.Source, backend encode.Backend, cfg StreamConfig) *Stream {
	if cfg.AnalysisWindow <= 0 {
		cfg.AnalysisWindow = 200 * time.Millisecond
	}
	return &Stream{
		cfg:     cfg,
		src:     src,
		backend: backend,
		th:      qualifier.DefaultThresholds(),
		state: model.AudioStreamState{
			InputDeviceID: cfg.DeviceID,
			StreamType:    cfg.Stream,
			SourceChannel: cfg.Source,
			Status:        model.QualFailed,
			Reasons:       []string{"not yet started"},
		},
	}
}

// Run drives the chain until ctx is cancelled. It accumulates capture frames into
// analysis blocks, extracts the mapped channel(s), qualifies the block, and updates
// the stream state. It does NOT block on a slow consumer and never persists audio.
func (s *Stream) Run(ctx context.Context, duringPlayback bool) error {
	frames, err := s.src.Start(ctx)
	if err != nil {
		s.setFailed("capture source failed to start")
		return err
	}
	// Optional one-shot encode smoke so the dashboard can show the path is alive
	// and estimate latency (real glass-to-ear latency is hardware-gated, §11.4).
	if s.backend != nil && s.backend.Available() {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.backend.SmokeTest(sctx, 200*time.Millisecond, encode.DefaultOpusParams()); err == nil {
			s.mu.Lock()
			s.latencyMS = 60 // nominal local estimate; replaced by a real probe later
			s.mu.Unlock()
		}
		cancel()
	}

	var (
		acc        []float64
		sampleRate int
		channels   int
		target     int
	)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-frames:
			if !ok {
				return nil
			}
			if target == 0 {
				sampleRate, channels = f.SampleRate, f.Channels
				target = int(float64(sampleRate)*s.cfg.AnalysisWindow.Seconds()) * channels
			}
			acc = append(acc, f.Samples...)
			if len(acc) >= target {
				s.qualify(acc, sampleRate, channels, duringPlayback)
				acc = acc[:0]
			}
		}
	}
}

func (s *Stream) qualify(interleaved []float64, sampleRate, channels int, duringPlayback bool) {
	// Map the configured source channel(s) into the stream's mono analysis lane.
	lane, err := mapper.Extract(s.cfg.Source, interleaved, channels)
	if err != nil {
		s.setFailed("channel mapping impossible for this input: " + err.Error())
		return
	}
	st := qualifier.Analyze(qualifier.Input{
		Samples:          lane,
		SampleRate:       sampleRate,
		Channels:         1,
		RequiredChannels: s.cfg.RequiredChannels,
		Present:          true,
		DeviceConnected:  true,
		DuringPlayback:   duringPlayback,
		InputDeviceID:    s.cfg.DeviceID,
		StreamType:       s.cfg.Stream,
		SourceChannel:    s.cfg.Source,
	}, s.th)

	// If the mapping needs more channels than the device offers, that is a
	// pre-show Failed (§6.5) — flag it even if the lane qualified.
	if s.cfg.RequiredChannels > channels {
		st.Status = model.QualFailed
		st.Reasons = append(st.Reasons, "configured mapping needs more channels than the input provides")
	}

	s.mu.Lock()
	st.LatencyMSEst = s.latencyMS
	st.DeviceConnected = true
	s.state = st
	s.mu.Unlock()
}

func (s *Stream) setFailed(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Status = model.QualFailed
	s.state.Present = false
	s.state.Reasons = []string{reason}
}

// Status returns the latest per-stream state (transient; no samples).
func (s *Stream) Status() model.AudioStreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Stop stops the capture source.
func (s *Stream) Stop() error { return s.src.Close() }
