// Package pipeline assembles the openaccessbridge-media chain (buildplan.md
// §6): capture → channel mapper → four-state qualifier → Opus encode. It runs
// one independent chain per named stream (HI, VI-N) so the two are fully
// independent (§6.1), and continuously reports each stream's transient
// AudioStreamState (no audio samples are ever stored or logged — guardrail #5,
// §9.4).
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

	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/encode"
	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Encoder states reported on AudioStreamState.EncoderState (§6.6 "encoder
// state"). They describe what the media backend actually did, never more.
const (
	// EncoderUnknown: the encode path has not been exercised yet.
	EncoderUnknown = "unknown"
	// EncoderUnavailable: no Opus encoder ran — either none is installed on this
	// host (no gst-launch/opusenc) or the encode failed. Nothing is being encoded,
	// and no latency figure is reported for the encode path.
	EncoderUnavailable = "unavailable"
	// EncoderReadyNoTransport: a real Opus encode was proven locally, but this
	// stream has no subscriber transport attached (no DeliverySink), so
	// ConnectedClients is honestly 0 rather than unreported.
	EncoderReadyNoTransport = "ready_no_transport"
	// EncoderStreaming: a live Opus encode is running for this stream AND has
	// produced RTP packets that were read out of it and handed to the WebRTC
	// fan-out. It is set from that measured packet count, never from "the
	// encoder process started" — a pipeline that spawned and then emitted
	// nothing is reported as unavailable (§6.6, guardrail #7).
	EncoderStreaming = "streaming"
)

// deliveryProofDeadline is how long a freshly started live encode has to produce
// its first RTP packet before it is reported as unavailable. The §6.7 encoder
// emits a packet per 10 ms frame, so a second is two orders of magnitude of
// slack — long enough for process start-up, short enough that a wedged encoder
// reaches the dashboard as a fault within one poll.
const deliveryProofDeadline = time.Second

// deliveryRetryInterval bounds how often a failed live encode is retried. A
// missing encoder (no gst-launch) must not be re-spawned once per capture frame.
const deliveryRetryInterval = 5 * time.Second

// DeliverySink is the WebRTC fan-out a running stream publishes its encoder to
// (§6.7). It is declared here, on the consumer side, so this package stays free
// of the WebRTC stack: internal/audio/webrtcpeer.Hub implements it.
//
// Publish hands over ONE encoder for a named stream — §6.7's "one
// capture/analysis/Opus source per named stream" — which the sink then
// distributes to one peer connection per subscriber.
type DeliverySink interface {
	Publish(stream model.StreamType, rtp encode.RTPStream) error
	Unpublish(stream model.StreamType)
	// Listeners is §6.6's "connected listener count" for this stream.
	Listeners(stream model.StreamType) int
}

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
	// Delivery is the WebRTC fan-out this stream feeds (§6.7). When it is set
	// and the backend can stream, the stream starts ONE live Opus/RTP encode of
	// its mapped lane and publishes it there. Nil means this chain analyses its
	// input but delivers to nobody — the calibration wizard's measurement runs
	// (§6.4), and every test that only wants a verdict.
	Delivery DeliverySink
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
	// Live-delivery state (§6.7), owned by the Run goroutine.
	rtp        encode.RTPStream
	rtpStarted time.Time
	rtpRetryAt time.Time
	lastDrops  uint64
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
	// Whatever ends this chain — cancellation, a device that stopped, an error —
	// takes the live encode and its subscribers with it. Leaving a gst-launch
	// child and a published track behind would keep a dead stream on the air.
	defer s.stopDelivery()

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

			// --- A frame the driver could not describe (no channel count, no
			// rate, or a length that is not a whole number of frames) cannot be
			// de-interleaved or measured. Report it; do NOT drop it quietly.
			// Dropping it looks harmless but is the worst outcome available: the
			// arriving frames keep the stale-frame watchdog fed while nothing is
			// ever qualified, so the stream sits on its startup verdict forever
			// (§6.3 stability, §1.2 guardrail 7 no silent degradation). ---
			if !frameUsable(f) {
				s.failLive("capture device delivered an unusable frame — it reported no channel count, no sample rate, or a truncated buffer")
				acc = acc[:0]
				continue
			}

			// --- §6.3 stability: the format is re-checked on EVERY frame, not
			// latched from the first one. A device that renumbers or re-opens at
			// another rate/width is a fault to report, not something to
			// de-interleave blindly. ---
			if f.SampleRate != s.rate || f.Channels != s.channels {
				if s.frames > 1 {
					s.formatChanges++
					s.failLive("input device changed capture format mid-stream — it may be renumbering or re-enumerating")
					// The live encode was started for the OLD format, so it can
					// no longer describe this input. Tear it down; the next
					// usable frame starts one for the format now arriving.
					s.stopDelivery()
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
			// --- §6.7 delivery. The mapped lane goes to the encoder frame by
			// frame, BESIDE the analysis tap rather than behind it: the
			// qualifier accumulates a 200 ms analysis window, and routing
			// delivery through that accumulator would add the whole window to
			// the latency budget for no benefit. This is the split
			// EncoderPathLatencyMS documents. ---
			s.deliver(ctx, f)

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
	// How much real time this block covers. It is MEASURED from the block, not
	// assumed to be AnalysisWindow: frames are accumulated until the window is
	// filled, so a device whose capture period is longer than the window
	// delivers longer blocks. §6.3's sustained-silence rule is a duration test,
	// so it has to count the audio that actually arrived — counting nominal
	// windows would silently stretch a 2 s threshold to 8 s on a device with a
	// 200 ms period, i.e. delay a real fault.
	blockMS := blockDurationMS(len(interleaved), deviceChannels, sampleRate)
	if blockMS < 1 {
		blockMS = int(s.cfg.AnalysisWindow.Milliseconds())
	}
	if blockMS < 1 {
		blockMS = 1
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
		SilentForMS:      s.silentMS + blockMS,
		PrevSilent:       s.prevSilent,
		InputDeviceID:    s.cfg.DeviceID,
		StreamType:       s.cfg.Stream,
		SourceChannel:    s.cfg.Source,
	}, s.th)

	// Sustained-silence accumulator for the next block (§6.3 duration context).
	if st.Silence {
		s.silentMS += blockMS
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

	s.checkDelivery()
	st.EncoderState = s.encoderState
	st.LatencyMSEst = s.latencyEstimateMS()
	// §6.6's connected listener count, read from the WebRTC fan-out. It is 0
	// when there is no transport attached, which is a fact rather than a
	// placeholder. LatencyMeasured stays false either way: a listener count is
	// not a glass-to-ear measurement (§11.4, BLOCKER B2).
	st.ConnectedClients = s.listeners()
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

// deliver hands one capture frame's mapped lane to the live encoder, starting
// the encode on the first frame whose format is known (§6.7).
func (s *Stream) deliver(ctx context.Context, f capture.Frame) {
	if s.cfg.Delivery == nil {
		return
	}
	lane, err := mapper.Extract(s.cfg.Source, f.Samples, f.Channels)
	if err != nil {
		// The mapping cannot be satisfied by this input. qualify() reports that
		// as a Failed verdict with the reason; delivering a lane we could not
		// build is the one thing that must not happen.
		return
	}
	if s.rtp == nil {
		s.startDelivery(ctx, f.SampleRate)
	}
	if s.rtp == nil {
		return
	}
	if err := s.rtp.WritePCM(lane); err != nil {
		s.log.Warn(logging.EvAudioEncoderStopped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("reason", "encoder_write_failed"))
		s.stopDelivery()
	}
}

// startDelivery opens ONE live Opus/RTP encode for this named stream and
// publishes it to the fan-out. A backend that cannot stream (no gst-launch on
// this host) leaves the stream reported as having no encoder — never as
// streaming something nobody encoded.
func (s *Stream) startDelivery(ctx context.Context, sampleRate int) {
	if !s.rtpRetryAt.IsZero() && time.Now().Before(s.rtpRetryAt) {
		return
	}
	s.rtpRetryAt = time.Now().Add(deliveryRetryInterval)

	streamer, ok := s.backend.(encode.Streamer)
	if !ok {
		s.encoderState = EncoderUnavailable
		return
	}
	p := encode.DefaultOpusParams()
	s.opusFrameMS = p.FrameMS
	rtp, err := streamer.StartRTP(ctx, encode.PCMFormat{
		SampleRate: sampleRate, Channels: laneChannels(s.cfg.Source),
	}, p)
	if err != nil {
		s.encoderState = EncoderUnavailable
		s.log.Warn(logging.EvAudioEncoderStopped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("reason", "encoder_start_failed"))
		return
	}
	if err := s.cfg.Delivery.Publish(s.cfg.Stream, rtp); err != nil {
		_ = rtp.Close()
		s.encoderState = EncoderUnavailable
		s.log.Warn(logging.EvAudioEncoderStopped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("reason", "publish_failed"))
		return
	}
	s.rtp, s.rtpStarted, s.lastDrops = rtp, time.Now(), 0
	// EncoderUnknown, not "streaming": the encoder has been STARTED, and nothing
	// has been encoded until a packet comes out of it (checkDelivery).
	s.encoderState = EncoderUnknown
	s.log.Info(logging.EvStateTransition,
		logging.Field("phase", "audio_encode_started"),
		logging.Field("stream", string(s.cfg.Stream)),
		// "capture_rate_hz", not "sample_rate": the §9.4 redaction guard denies
		// any key containing "sample" (audio samples are content), and a field
		// that logs as [REDACTED] tells an operator nothing.
		logging.Field("capture_rate_hz", sampleRate),
		logging.Field("frame_ms", p.FrameMS))
}

// stopDelivery closes the live encode and takes its subscribers off it.
func (s *Stream) stopDelivery() {
	if s.rtp == nil {
		return
	}
	if s.cfg.Delivery != nil {
		s.cfg.Delivery.Unpublish(s.cfg.Stream)
	}
	_ = s.rtp.Close()
	s.rtp = nil
	if s.encoderState == EncoderStreaming {
		s.encoderState = EncoderUnavailable
	}
}

// checkDelivery turns the live encode's OBSERVED counters into the §6.6 encoder
// state. Nothing here is inferred from the encoder having been launched: the
// state says "streaming" only once RTP packets have actually been read out of
// it, and an encode that stops or never produces one is reported as unavailable
// so the dashboard raises its loud warning (readiness §8.3).
func (s *Stream) checkDelivery() {
	if s.cfg.Delivery == nil || s.rtp == nil {
		return
	}
	if err := s.rtp.Err(); err != nil {
		s.log.Warn(logging.EvAudioEncoderStopped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("reason", "encoder_exited"))
		s.stopDelivery()
		s.encoderState = EncoderUnavailable
		return
	}
	st := s.rtp.Stats()
	if st.FramesDropped > s.lastDrops {
		// Audio the encoder could not take is a gap in a patron's ear. It is
		// reported as a COUNT, never left silent (guardrail #7).
		s.log.Warn(logging.EvAudioEncoderDropped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("dropped_blocks", st.FramesDropped))
		s.lastDrops = st.FramesDropped
	}
	switch {
	case st.PacketsOut > 0:
		s.encoderState = EncoderStreaming
	case time.Since(s.rtpStarted) > deliveryProofDeadline:
		s.log.Warn(logging.EvAudioEncoderStopped,
			logging.Field("stream", string(s.cfg.Stream)),
			logging.Field("reason", "no_rtp_produced"))
		s.stopDelivery()
		s.encoderState = EncoderUnavailable
	}
}

// listeners is §6.6's connected listener count for this stream, read from the
// fan-out. With no transport attached it is 0 — which is the truth, not a
// placeholder.
func (s *Stream) listeners() int {
	if s.cfg.Delivery == nil {
		return 0
	}
	return s.cfg.Delivery.Listeners(s.cfg.Stream)
}

// probeEncoder runs the one-shot encode smoke so the dashboard can show whether
// the media path is alive (§6.6 encoder state). A backend that encodes nothing
// reports EncoderUnavailable and contributes NO latency figure.
//
// A stream with a DeliverySink skips it: the live encode this stream is about to
// start is stronger evidence than a smoke test of a different pipeline, and
// spending 200 ms proving an encoder twice delays the audio a patron is waiting
// for.
func (s *Stream) probeEncoder(ctx context.Context) {
	if s.cfg.Delivery != nil {
		return
	}
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
// Only the two states in which an encode really happened contribute a figure.
func (s *Stream) latencyEstimateMS() int {
	switch s.encoderState {
	case EncoderReadyNoTransport, EncoderStreaming:
		return EncoderPathLatencyMS(s.capturePeriodMS, s.opusFrameMS)
	default:
		return 0
	}
}

// EncoderPathLatencyMS estimates the APPLIANCE-side FRAMING latency: how long the
// oldest sample in a block waits before the Opus packet carrying it can exist.
//
// IT IS ONE ACCUMULATION, NOT TWO, and correcting that is the point of this
// function's current shape. It used to return capturePeriod + opusFrame, which at
// the shipped settings (a 10 ms capture period feeding 10 ms Opus frames) claimed
// 20 ms — but those are the SAME wait. The captured block is handed straight to
// the encoder and becomes exactly one Opus frame; nothing accumulates twice. The
// §11.4 harness measured the whole appliance-side path at ~10.7 ms, which is the
// 10 ms framing term plus a measured 0.65 ms of transit, and it is the first
// thing able to say so.
//
// The general rule is the smallest whole number of capture periods that holds at
// least one Opus frame:
//
//	period 10, frame 10 -> 10   (one block IS one frame)
//	period  5, frame 10 -> 10   (two blocks make a frame)
//	period 20, frame 10 -> 20   (a block makes two frames; the wait is the block)
//	period  7, frame 10 -> 14   (two blocks, because a frame does not divide them)
//
// That last case is why this is a ceiling rather than a max: with a period that
// does not divide the frame, the encoder waits for a whole extra block.
//
// WHAT IS DELIBERATELY NOT IN IT. This is the STRUCTURAL term only:
//
//   - the level-analysis tap runs BESIDE the encode path rather than in front of
//     it (§6.1), so the analysis window is not part of delivery latency;
//   - the measured transit through the encoder and the WebRTC fan-out (§11.4:
//     0.65 ms at p95 on this host) is a MEASUREMENT and does not belong in a
//     structural estimate;
//   - the local Wi-Fi hop, the BROWSER JITTER BUFFER (the dominant and least
//     controllable term, 40-300+ ms) and the phone's audio output all happen
//     outside this process and are UNMEASURED here.
//
// So this is never a glass-to-ear figure and must not be compared against the
// §6.7 / D10 end-to-end budgets as if it were one. Measuring the real number
// needs the Phase 0C loopback rig and a phone fleet (BLOCKERS B3, §11.4); until
// that runs, AudioStreamState.LatencyMeasured stays false.
//
// The estimate got SMALLER when this was corrected, which is worth stating
// plainly: the old number was not conservative, it was wrong, and a latency
// budget padded by a term that does not exist misdirects the effort to fix the
// terms that do (§15 names HI latency as the plan's largest risk).
func EncoderPathLatencyMS(capturePeriodMS, opusFrameMS int) int {
	if opusFrameMS <= 0 {
		opusFrameMS = encode.DefaultOpusParams().FrameMS
	}
	if capturePeriodMS <= 0 {
		// No capture period observed yet: the encoder's own frame is the only
		// accumulation this function can account for.
		return opusFrameMS
	}
	blocks := (opusFrameMS + capturePeriodMS - 1) / capturePeriodMS // ceil
	return blocks * capturePeriodMS
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

// frameUsable reports whether a capture frame describes itself well enough to be
// de-interleaved and measured (§6.1 "per-input channel count detected"). A frame
// that does not is a fault to report, not one to skip.
func frameUsable(f capture.Frame) bool {
	return f.SampleRate > 0 && f.Channels > 0 && len(f.Samples) > 0 && len(f.Samples)%f.Channels == 0
}

// blockDurationMS is how much real time an interleaved analysis block covers. It
// is what the §6.3 duration rules are measured against; 0 means the block cannot
// be timed and the caller falls back to the configured window.
func blockDurationMS(samples, channels, sampleRate int) int {
	if channels <= 0 || sampleRate <= 0 {
		return 0
	}
	return (samples / channels) * 1000 / sampleRate
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
