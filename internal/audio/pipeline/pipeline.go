// Package pipeline assembles the captionbridge-media chain (buildplan.md §6):
// capture → channel mapper → four-state qualifier → Opus encode. It runs one
// independent chain per named stream (HI, VI-N) so the two are fully independent
// (§6.1), and continuously reports each stream's transient AudioStreamState (no
// audio samples are ever stored or logged — guardrail #5, §9.4).
//
// The Stream also owns the per-input history the qualifier cannot see, because
// those §6.3 measurements are properties of TIME rather than of one block:
// sustained-silence duration with re-arm hysteresis, capture-format stability
// (device renumbering / sample-rate glitches), dropouts, and device
// connect/disconnect. A source that stops delivering audio is reported as Failed
// — never left showing its last healthy verdict (guardrail #7).
package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Encoder states reported on AudioStreamState.EncoderState (§6.6 "encoder
// state"). They describe what the media backend actually did, never more.
const (
	// EncoderUnknown: the encode path has not been exercised yet.
	EncoderUnknown = "unknown"
	// EncoderUnavailable: no Opus encoder ran — either none is installed on this
	// host (BLOCKERS B4) or the smoke encode failed. Nothing is being encoded,
	// and no latency figure is reported for the encode path.
	EncoderUnavailable = "unavailable"
	// EncoderReadyNoTransport: a real Opus encode was proven locally, but there
	// is still no WebRTC subscriber transport (webrtcbin is gated by B2/B4), so
	// ConnectedClients is honestly 0 rather than unreported.
	EncoderReadyNoTransport = "ready_no_transport"
)

// DefaultSampleRate is the §6.1 fixed internal capture format. A device running
// at any other rate is a configuration fault, not something to resample around.
const DefaultSampleRate = 48000

// dropoutRecentWindow is how long a dropout keeps demoting the verdict. Older
// dropouts stay in the count (§6.6 reports them) but stop holding the stream
// down, so a single glitch an hour ago does not look like a live fault.
const dropoutRecentWindow = 30 * time.Second

// StreamConfig configures one named audio stream. Every field is optional except
// the stream identity: the zero value gives the shipped defaults.
type StreamConfig struct {
	DeviceID         string
	Stream           model.StreamType
	Source           model.SourceChannel
	RequiredChannels int // mapping requirement (e.g. 2 for a stereo split)
	// AnalysisWindow is how much audio each qualification block covers.
	AnalysisWindow time.Duration
	// Tier is the §6.2 input tier of the connected hardware; it selects the
	// qualifier threshold profile. Unset = the default shipped input (tier 2).
	Tier qualifier.Tier
	// Thresholds overrides individual qualifier knobs on top of the tier
	// profile. Zero-valued fields keep the tier's value.
	Thresholds qualifier.Thresholds
	// RequiredSampleRate is the fixed internal capture format (§6.1); 0 = 48 kHz.
	RequiredSampleRate int
	// StaleFrameTimeout is how long the chain waits for audio before declaring
	// the device dead. 0 = five analysis windows, never less than a second.
	StaleFrameTimeout time.Duration
	// DropoutAfter is the inter-frame gap that counts as a dropout. 0 = three
	// capture periods, never less than 150 ms.
	DropoutAfter time.Duration
	// Logger receives the §9.4 audio events (verdict transitions, clipping,
	// sustained silence, device connect/disconnect). nil = discard.
	Logger *logging.Logger
}

// Stream runs one capture→map→qualify chain and exposes its latest state.
type Stream struct {
	cfg     StreamConfig
	src     capture.Source
	backend encode.Backend
	th      qualifier.Thresholds
	log     *logging.Logger

	mu    sync.Mutex
	state model.AudioStreamState

	// Everything below is owned by the Run goroutine.
	encoderState    string
	opusFrameMS     int
	capturePeriodMS int
	rate            int
	channels        int
	silentMS        int
	prevSilent      bool
	dropouts        int
	lastDropout     time.Time
	formatChanges   int
	frames          int
	lastVerdict     model.QualifierState
	lastClipping    bool
	lastSilenceFail bool
	connected       bool
	connKnown       bool
}

// NewStream builds a Stream. backend may be nil (no encode smoke).
func NewStream(src capture.Source, backend encode.Backend, cfg StreamConfig) *Stream {
	if cfg.AnalysisWindow <= 0 {
		cfg.AnalysisWindow = 200 * time.Millisecond
	}
	log := cfg.Logger
	if log == nil {
		log = logging.Nop()
	}
	return &Stream{
		cfg:     cfg,
		src:     src,
		backend: backend,
		// Per-tier profile (§6.2) with any caller override layered on top.
		th:           cfg.Thresholds.WithDefaults(qualifier.ThresholdsForTier(cfg.Tier)),
		log:          log,
		encoderState: EncoderUnknown,
		opusFrameMS:  encode.DefaultOpusParams().FrameMS,
		state: model.AudioStreamState{
			InputDeviceID:  cfg.DeviceID,
			StreamType:     cfg.Stream,
			SourceChannel:  cfg.Source,
			Status:         model.QualFailed,
			LevelDBFS:      qualifier.MinDBFS,
			NoiseFloorDBFS: qualifier.MinDBFS,
			EncoderState:   EncoderUnknown,
			Reasons:        []string{"not yet started"},
		},
	}
}

// Run drives the chain until ctx is cancelled. It accumulates capture frames into
// analysis blocks, re-checks the capture format on every frame, extracts the
// mapped channel(s), qualifies the block, and updates the stream state. It does
// NOT block on a slow consumer and never persists audio.
func (s *Stream) Run(ctx context.Context, duringPlayback bool) error {
	frames, err := s.src.Start(ctx)
	if err != nil {
		s.noteConnection(false, "capture source failed to start")
		s.fail("capture source failed to start", false, false)
		return err
	}
	s.noteConnection(true, "")
	s.probeEncoder(ctx)

	stale := s.staleTimeout()
	tick := stale / 2
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	watchdog := time.NewTicker(tick)
	defer watchdog.Stop()

	var acc []float64
	target := 0
	lastFrame := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-watchdog.C:
			// Stale-frame watchdog: a device that stops delivering must surface
			// as Failed rather than freeze the last healthy verdict (§1.2
			// guardrail 7, §11.6 soak/hot-plug).
			if now.Sub(lastFrame) > stale {
				s.noteConnection(false, "no capture frames within the stale-frame deadline")
				s.fail("capture device stopped delivering audio", false, false)
			}
		case f, ok := <-frames:
			if !ok {
				// The source closed its channel: the device has stopped.
				s.noteConnection(false, "capture source closed its frame channel")
				s.fail("capture device disconnected", false, false)
				return nil
			}
			now := time.Now()
			s.observeArrival(f, now.Sub(lastFrame))
			lastFrame = now
			s.noteConnection(true, "")

			// --- §6.3 stability: the format is re-checked on EVERY frame, not
			// latched from the first one. A device that renumbers or re-opens at
			// another rate/width is a fault to report, not something to
			// de-interleave blindly. ---
			if f.SampleRate != s.rate || f.Channels != s.channels {
				if s.frames > 1 {
					s.formatChanges++
					s.failLive("input device changed capture format mid-stream — it may be renumbering or re-enumerating")
				}
				s.rate, s.channels = f.SampleRate, f.Channels
				s.capturePeriodMS = framePeriodMS(f)
				target = s.blockSamples()
				acc = acc[:0]
			}
			// --- §6.1 fixed internal capture format. ---
			if f.SampleRate != s.requiredRate() {
				s.failLive(fmt.Sprintf("capture device is running at %d Hz; this appliance uses a fixed %d Hz internal capture format",
					f.SampleRate, s.requiredRate()))
				acc = acc[:0]
				continue
			}
			if target <= 0 {
				continue
			}
			acc = append(acc, f.Samples...)
			if len(acc) >= target {
				s.qualify(acc, s.rate, s.channels, duringPlayback)
				acc = acc[:0]
			}
		}
	}
}

// qualify maps one analysis block into the stream's lane, qualifies it, and
// folds in the per-stream history the qualifier cannot see.
func (s *Stream) qualify(interleaved []float64, sampleRate, deviceChannels int, duringPlayback bool) {
	// Map the configured source channel(s) into the stream's analysis lane.
	lane, err := mapper.Extract(s.cfg.Source, interleaved, deviceChannels)
	if err != nil {
		s.failLive("channel mapping impossible for this input: " + err.Error())
		return
	}
	present, connected := s.health()
	windowMS := int(s.cfg.AnalysisWindow.Milliseconds())
	if windowMS < 1 {
		windowMS = 1
	}
	st := qualifier.Analyze(qualifier.Input{
		Samples:    lane,
		SampleRate: sampleRate,
		// The lane is what the mapper produced; the DEVICE count is what the
		// hardware offers. Passing the device count is what makes a legal stereo
		// mapping qualify instead of being failed as "mono" (§6.5).
		Channels:         laneChannels(s.cfg.Source),
		DeviceChannels:   deviceChannels,
		RequiredChannels: s.cfg.RequiredChannels,
		Present:          present,
		DeviceConnected:  connected,
		DuringPlayback:   duringPlayback,
		SilentForMS:      s.silentMS + windowMS,
		PrevSilent:       s.prevSilent,
		InputDeviceID:    s.cfg.DeviceID,
		StreamType:       s.cfg.Stream,
		SourceChannel:    s.cfg.Source,
	}, s.th)

	// Sustained-silence accumulator for the next block (§6.3 duration context).
	if st.Silence {
		s.silentMS += windowMS
		s.prevSilent = true
	} else {
		s.silentMS = 0
		s.prevSilent = false
	}

	// --- §6.3 stability defects, which span blocks. ---
	if s.formatChanges > 0 {
		demote(&st, model.QualMarginal,
			fmt.Sprintf("input device changed capture format %d time(s) this session — check the device and its driver", s.formatChanges))
	}
	st.Dropouts = s.dropouts
	if s.dropouts > 0 && time.Since(s.lastDropout) < dropoutRecentWindow {
		demote(&st, model.QualMarginal, "audio dropouts detected on this input")
	}

	st.EncoderState = s.encoderState
	st.LatencyMSEst = s.latencyEstimateMS()
	// LatencyMeasured stays false: no end-to-end latency has been measured (see
	// EncoderPathLatencyMS). ConnectedClients stays 0: there is no WebRTC
	// subscriber transport yet, so any other number would be invented.
	s.publish(st)
}

// fail replaces the state with a Failed verdict. Measurements are CLEARED rather
// than carried over: a dead or misconfigured input must never keep showing the
// level it had when it was healthy (§1.2 guardrail 7).
func (s *Stream) fail(reason string, present, connected bool) {
	s.publish(model.AudioStreamState{
		InputDeviceID: s.cfg.DeviceID,
		StreamType:    s.cfg.Stream,
		SourceChannel: s.cfg.Source,
		// Last observed capture format — device configuration, not a live
		// measurement, so it stays useful while diagnosing the failure.
		SampleRate:     s.rate,
		Channels:       s.channels,
		Status:         model.QualFailed,
		LevelDBFS:      qualifier.MinDBFS,
		NoiseFloorDBFS: qualifier.MinDBFS,
		Present:        present,
		// DeviceConnected is whatever the caller established — never forced true.
		DeviceConnected: connected,
		Reasons:         []string{reason},
		EncoderState:    s.encoderState,
		Dropouts:        s.dropouts,
		LatencyMSEst:    s.latencyEstimateMS(),
	})
}

// failLive records a Failed verdict for an input that is still DELIVERING audio
// — a configuration or stability fault rather than a disconnect. Presence comes
// from the source's own health seam, never from an assumption.
func (s *Stream) failLive(reason string) {
	present, connected := s.health()
	s.fail(reason, present, connected)
}

// publish stores the new state and emits the §9.4 audio events on TRANSITION, so
// "continuously report" (§6.6) is a stream of state changes and not only a
// pollable snapshot. No caption text, sample values or client IDs are logged.
func (s *Stream) publish(st model.AudioStreamState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()

	if st.Status != s.lastVerdict {
		s.log.Info(logging.EvAudioQualifierVerdict,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("device_id", s.cfg.DeviceID),
			logging.Field("status", string(st.Status)),
			logging.Field("level_dbfs", round1(st.LevelDBFS)))
		s.lastVerdict = st.Status
	}
	if st.Clipping && !s.lastClipping {
		s.log.Warn(logging.EvAudioLevelClipping,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("level_dbfs", round1(st.LevelDBFS)))
	}
	s.lastClipping = st.Clipping

	silenceFail := st.Silence && st.Status == model.QualFailed
	if silenceFail && !s.lastSilenceFail {
		s.log.Warn(logging.EvAudioSilenceSustained,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("silent_ms", s.silentMS),
			logging.Field("noise_floor_dbfs", round1(st.NoiseFloorDBFS)))
	}
	s.lastSilenceFail = silenceFail
}

// noteConnection records a device connect/disconnect and emits the event on
// TRANSITION only (§6.1 "detect device connect/disconnect and surface it
// immediately"; §6.6 "device connected/disconnected").
func (s *Stream) noteConnection(connected bool, detail string) {
	if s.connKnown && s.connected == connected {
		return
	}
	s.connKnown, s.connected = true, connected
	fields := []logging.F{
		logging.Field("stream", string(s.cfg.Stream)),
		logging.Field("device_id", s.cfg.DeviceID),
	}
	if connected {
		s.log.Info(logging.EvAudioInputConnected, fields...)
		return
	}
	s.log.Warn(logging.EvAudioInputDisconnected, append(fields, logging.Field("detail", detail))...)
}

// health reports the input's presence/connection state (§6.3 "Presence / device
// health"). A Source that implements capture.HealthReporter is authoritative —
// that is the seam a real ALSA source reports hot-plug through (§6.1). For one
// that does not, the honest answer is what was just observed: a full analysis
// window of frames arrived, so the device is present and delivering. The
// closed-channel and stale-frame paths in Run cover the other direction.
func (s *Stream) health() (present, connected bool) {
	if hr, ok := s.src.(capture.HealthReporter); ok {
		h := hr.Health()
		return h.Present, h.Connected
	}
	return true, true
}

// probeEncoder runs the one-shot encode smoke so the dashboard can show whether
// the media path is alive (§6.6 encoder state). A backend that encodes nothing
// reports EncoderUnavailable and contributes NO latency figure.
func (s *Stream) probeEncoder(ctx context.Context) {
	if s.backend == nil || !s.backend.Available() {
		s.encoderState = EncoderUnavailable
		return
	}
	p := encode.DefaultOpusParams()
	s.opusFrameMS = p.FrameMS
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.backend.SmokeTest(sctx, 200*time.Millisecond, p); err != nil {
		// encode.ErrNoEncoder (the no-op stand-in) or a real failure: either way
		// nothing was encoded, and nothing may be claimed about the path.
		s.encoderState = EncoderUnavailable
		return
	}
	s.encoderState = EncoderReadyNoTransport
}

// observeArrival counts capture discontinuities (§6.3 stability, §6.6 dropouts).
// A gap between frames far longer than the capture period means audio the device
// never delivered — the count is of MISSED capture periods, not of events.
func (s *Stream) observeArrival(f capture.Frame, gap time.Duration) {
	s.frames++
	period := time.Duration(framePeriodMS(f)) * time.Millisecond
	if period <= 0 || s.frames <= 1 {
		// The first frame's "gap" is start-up latency, not a dropout.
		return
	}
	limit := s.cfg.DropoutAfter
	if limit <= 0 {
		limit = 3 * period
		if limit < 150*time.Millisecond {
			limit = 150 * time.Millisecond
		}
	}
	if gap <= limit {
		return
	}
	missed := int(gap/period) - 1
	if missed < 1 {
		missed = 1
	}
	s.dropouts += missed
	s.lastDropout = time.Now()
}

// latencyEstimateMS returns the encode-path estimate, or 0 when no encoder ran.
func (s *Stream) latencyEstimateMS() int {
	if s.encoderState != EncoderReadyNoTransport {
		return 0
	}
	return EncoderPathLatencyMS(s.capturePeriodMS, s.opusFrameMS)
}

// EncoderPathLatencyMS estimates the APPLIANCE-side audio latency: the capture
// period (the ALSA buffer this process consumes) plus the Opus frame — rows 1
// and 3 of the §6.7 latency table. Nothing else is added, and nothing is
// invented:
//
//   - the level-analysis tap runs BESIDE the encode path rather than in front of
//     it (§6.1), so the analysis window is not part of delivery latency;
//   - the local Wi-Fi hop, the BROWSER JITTER BUFFER (the dominant and least
//     controllable term, 40–300+ ms) and the phone's audio output all happen
//     outside this process and are UNMEASURED here.
//
// So this is never a glass-to-ear figure and must not be compared against the
// §6.7 / D10 end-to-end budgets as if it were one. Measuring the real number
// needs the Phase 0C loopback rig and a phone fleet (BLOCKERS B3, §11.4); until
// that runs, AudioStreamState.LatencyMeasured stays false.
func EncoderPathLatencyMS(capturePeriodMS, opusFrameMS int) int {
	if capturePeriodMS < 0 {
		capturePeriodMS = 0
	}
	if opusFrameMS <= 0 {
		opusFrameMS = encode.DefaultOpusParams().FrameMS
	}
	return capturePeriodMS + opusFrameMS
}

// Status returns the latest per-stream state (transient; no samples).
func (s *Stream) Status() model.AudioStreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Stop stops the capture source.
func (s *Stream) Stop() error { return s.src.Close() }

// Thresholds returns the resolved qualifier profile this stream judges with
// (the §6.2 tier profile plus any override), so the admin surface can show the
// numbers a verdict was actually measured against.
func (s *Stream) Thresholds() qualifier.Thresholds { return s.th }

func (s *Stream) requiredRate() int {
	if s.cfg.RequiredSampleRate > 0 {
		return s.cfg.RequiredSampleRate
	}
	return DefaultSampleRate
}

func (s *Stream) staleTimeout() time.Duration {
	if s.cfg.StaleFrameTimeout > 0 {
		return s.cfg.StaleFrameTimeout
	}
	d := 5 * s.cfg.AnalysisWindow
	if d < time.Second {
		d = time.Second
	}
	return d
}

// blockSamples is how many interleaved samples one analysis window holds.
func (s *Stream) blockSamples() int {
	if s.rate <= 0 || s.channels <= 0 {
		return 0
	}
	n := int(float64(s.rate)*s.cfg.AnalysisWindow.Seconds()) * s.channels
	if n < s.channels {
		n = s.channels
	}
	return n
}

// laneChannels is the width of the lane the mapper produces for a source mode
// (§6.5): every mode collapses to one lane except stereo-pair, which passes the
// L/R pair through as 2-channel interleaved. Measuring that lane as mono would
// halve every window and mix the two lanes together.
func laneChannels(src model.SourceChannel) int {
	if src == model.ChannelStereo {
		return 2
	}
	return 1
}

// framePeriodMS is how much real time one capture frame covers.
func framePeriodMS(f capture.Frame) int {
	if f.Channels <= 0 || f.SampleRate <= 0 {
		return 0
	}
	return (len(f.Samples) / f.Channels) * 1000 / f.SampleRate
}

// demote lowers a verdict to at most `to` and records why. §6.3 classification is
// conservative: any one defect demotes, and the reason is always kept even when
// the verdict was already lower.
func demote(st *model.AudioStreamState, to model.QualifierState, reason string) {
	if st.Status.Rank() < to.Rank() {
		st.Status = to
	}
	st.Reasons = append(st.Reasons, reason)
}

// round1 rounds a dB measurement for logging (measurements are loggable; samples
// are not — §9.4).
func round1(v float64) float64 { return float64(int(v*10)) / 10 }
