package pipeline

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

const testRate = 48000

// --- test doubles ---------------------------------------------------------

// scriptedSource is a capture.Source whose frames, pacing and lifetime the test
// controls: it can pace frames apart (dropouts), change capture format
// mid-stream, stall (stale-frame watchdog), or stop entirely (disconnect). It
// deliberately does NOT implement capture.HealthReporter, so it also exercises
// the pipeline's fallback presence handling.
type scriptedSource struct {
	frames []capture.Frame
	gap    time.Duration // pacing between frames
	hold   bool          // keep the channel open after the last frame (stall)

	once sync.Once
	done chan struct{}
}

// scripted builds a source over a frame script. Options are set on the result.
func scripted(fs []capture.Frame) *scriptedSource {
	return &scriptedSource{frames: fs, done: make(chan struct{})}
}

func (s *scriptedSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame)
	go func() {
		defer close(out)
		for i, f := range s.frames {
			if i > 0 && s.gap > 0 {
				select {
				case <-time.After(s.gap):
				case <-ctx.Done():
					return
				case <-s.done:
					return
				}
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			case <-s.done:
				return
			}
		}
		if s.hold {
			select {
			case <-ctx.Done():
			case <-s.done:
			}
		}
	}()
	return out, nil
}

func (s *scriptedSource) Describe() string { return "scripted test source" }

func (s *scriptedSource) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// frames builds n identical capture frames.
func frames(n int, samples []float64, rate, channels int) []capture.Frame {
	out := make([]capture.Frame, n)
	for i := range out {
		out[i] = capture.Frame{Samples: samples, SampleRate: rate, Channels: channels}
	}
	return out
}

// tone/silence are one 10 ms frame's worth of interleaved samples.
func tone(channels int, amp float64) []float64 {
	return qualifier.SineWave(1000, amp, testRate, channels, testRate/100)
}

func silence(channels int) []float64 { return make([]float64, channels*testRate/100) }

// saturated is a full-scale square: a sustained run of truly clipped samples.
func saturated(channels int) []float64 {
	out := make([]float64, channels*testRate/100)
	for i := range out {
		if (i/(channels*24))%2 == 0 {
			out[i] = 1
		} else {
			out[i] = -1
		}
	}
	return out
}

// syncBuffer is a concurrency-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// settled reports whether the stream has produced a real verdict yet — the
// initial state is Failed/"not yet started", which must not satisfy a test
// waiting for a failure the pipeline is supposed to detect.
func settled(s model.AudioStreamState) bool {
	return len(s.Reasons) > 0 && s.Reasons[0] != "not yet started"
}

// waitFor polls the stream state until cond holds or the deadline passes.
func waitFor(t *testing.T, s *Stream, what string, cond func(model.AudioStreamState) bool) model.AudioStreamState {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		st := s.Status()
		if cond(st) {
			return st
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last state: %+v", what, st)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func run(t *testing.T, s *Stream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Stop()
	})
	go s.Run(ctx, true)
}

// --- tests ----------------------------------------------------------------

// TestPipelineSyntheticToneQualifies runs the full capture→map→qualify chain on a
// synthetic tone with no hardware and asserts a usable verdict is produced.
func TestPipelineSyntheticToneQualifies(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, encode.NullBackend{}, StreamConfig{
		DeviceID: "synthetic-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond,
	})
	run(t, st)
	waitFor(t, st, "a usable verdict", func(s model.AudioStreamState) bool {
		return s.Status.Usable() && s.LevelDBFS < 0
	})
}

// TestPipelineImpossibleMappingFails asserts a stereo-split mapping on a mono
// input is surfaced as Failed (pre-show, not mid-show — §6.5).
func TestPipelineImpossibleMappingFails(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "mono-0", Stream: model.StreamHI, Source: model.ChannelLeft, RequiredChannels: 2,
		AnalysisWindow: 50 * time.Millisecond,
	})
	run(t, st)
	got := waitFor(t, st, "a failed verdict", func(s model.AudioStreamState) bool {
		return settled(s) && s.Status == model.QualFailed
	})
	if !strings.Contains(strings.Join(got.Reasons, " "), "requires") {
		t.Errorf("reasons %v should explain the impossible mapping", got.Reasons)
	}
}

// TestPipelineStereoModesQualify is the regression test for the falsely-failed
// stereo mappings: every §6.5 stereo mode on a two-channel device must qualify,
// because Failed is reserved for mappings the hardware CANNOT satisfy. The
// pipeline used to hand the qualifier a hardcoded Channels:1 alongside
// RequiredChannels:2, so all four modes reported "input appears mono".
func TestPipelineStereoModesQualify(t *testing.T) {
	tests := []struct {
		name   string
		source model.SourceChannel
		stream model.StreamType
	}{
		{name: "left = HI", source: model.ChannelLeft, stream: model.StreamHI},
		{name: "right = VI-N", source: model.ChannelRight, stream: model.StreamVIN},
		{name: "summed mono = HI", source: model.ChannelSummed, stream: model.StreamHI},
		{name: "stereo pair = VI-N", source: model.ChannelStereo, stream: model.StreamVIN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 2, FrameSize: 480}
			st := NewStream(src, nil, StreamConfig{
				DeviceID: "stereo-0", Stream: tc.stream, Source: tc.source, RequiredChannels: 2,
				AnalysisWindow: 50 * time.Millisecond,
			})
			run(t, st)
			got := waitFor(t, st, "a verdict", settled)
			if got.Status == model.QualFailed {
				t.Fatalf("legal stereo mapping %s reported Failed: %v", tc.source, got.Reasons)
			}
			if got.Channels != 2 {
				t.Errorf("Channels = %d, want 2 — §6.3 reports the DEVICE's mono/stereo capability", got.Channels)
			}
			if got.LevelDBFS >= 0 || got.LevelDBFS <= qualifier.MinDBFS {
				t.Errorf("LevelDBFS = %.2f, want a real measurement", got.LevelDBFS)
			}
		})
	}
}

// TestPipelineQuietBlockIsNotAMidShowFailure is the regression test for the
// single-block silence alarm: one quiet analysis window must not demote a stream
// to Failed (which readiness escalates to a CRITICAL patron-visible warning).
// §6.3 requires duration plus noise-floor context.
func TestPipelineQuietBlockIsNotAMidShowFailure(t *testing.T) {
	// 5 × 10 ms silent frames = exactly one 50 ms analysis window, then stall.
	src := scripted(frames(5, silence(1), testRate, 1))
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "quiet-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
		Thresholds:        qualifier.Thresholds{SustainedSilenceMS: 1000},
	})
	run(t, st)
	got := waitFor(t, st, "the first qualified block", func(s model.AudioStreamState) bool {
		return s.Silence
	})
	if got.Status == model.QualFailed {
		t.Fatalf("one quiet 50 ms block must not fail the stream: %v", got.Reasons)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "watching for sustained silence") {
		t.Errorf("the quiet condition should still be reported out loud, got %v", got.Reasons)
	}
}

// TestPipelineSustainedSilenceFails is the other half: silence that LASTS is a
// real fault and must surface (§6.6 "a configured stream that goes silent …
// raises a loud admin warning").
func TestPipelineSustainedSilenceFails(t *testing.T) {
	// 150 × 10 ms silent frames = 1.5 s, past the 1 s threshold set below.
	src := scripted(frames(150, silence(1), testRate, 1))
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "dead-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
		Thresholds:        qualifier.Thresholds{SustainedSilenceMS: 1000},
	})
	run(t, st)
	got := waitFor(t, st, "a sustained-silence failure", func(s model.AudioStreamState) bool {
		return settled(s) && s.Status == model.QualFailed
	})
	if !strings.Contains(strings.Join(got.Reasons, " "), "no signal at all") {
		t.Errorf("a floor at digital zero should be reported as no signal, got %v", got.Reasons)
	}
}

// TestPipelineSilenceAccumulatorResets proves a quiet passage that ends does not
// leave the stream primed to fail: the accumulated silence is cleared once real
// audio returns.
func TestPipelineSilenceAccumulatorResets(t *testing.T) {
	script := append(frames(50, silence(1), testRate, 1), frames(50, tone(1, 0.3), testRate, 1)...)
	src := scripted(script)
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "recovering-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
		Thresholds:        qualifier.Thresholds{SustainedSilenceMS: 1000},
	})
	run(t, st)
	got := waitFor(t, st, "the signal to return", func(s model.AudioStreamState) bool {
		return settled(s) && !s.Silence
	})
	if got.Status == model.QualFailed {
		t.Fatalf("a recovered input must not stay failed: %v", got.Reasons)
	}
}

// TestPipelineDisconnectOnSourceClose covers §1.2 guardrail 7: when the capture
// source stops, the stream must report a dead device rather than freeze its last
// healthy verdict — including clearing the measurements, so the dashboard cannot
// show the level the input had when it was working.
func TestPipelineDisconnectOnSourceClose(t *testing.T) {
	src := scripted(frames(20, tone(1, 0.3), testRate, 1)) // channel closes at the end
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "unplugged-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond,
	})
	run(t, st)
	got := waitFor(t, st, "a disconnect", func(s model.AudioStreamState) bool {
		return s.Status == model.QualFailed && strings.Contains(strings.Join(s.Reasons, " "), "disconnected")
	})
	if got.Present || got.DeviceConnected {
		t.Errorf("a disconnected device must report present=false, connected=false: %+v", got)
	}
	if got.LevelDBFS != qualifier.MinDBFS || got.NoiseFloorDBFS != qualifier.MinDBFS {
		t.Errorf("measurements must be cleared on failure, got level=%.2f floor=%.2f", got.LevelDBFS, got.NoiseFloorDBFS)
	}
}

// TestPipelineStaleFrameWatchdog covers the other death mode: a source that
// keeps its channel open but stops delivering audio. Without a deadline the
// stream would serve its last good verdict forever.
func TestPipelineStaleFrameWatchdog(t *testing.T) {
	src := scripted(frames(10, tone(1, 0.3), testRate, 1))
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "stalled-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 100 * time.Millisecond,
	})
	run(t, st)
	got := waitFor(t, st, "the stale-frame watchdog", func(s model.AudioStreamState) bool {
		return settled(s) && s.Status == model.QualFailed
	})
	if !strings.Contains(strings.Join(got.Reasons, " "), "stopped delivering audio") {
		t.Errorf("reasons %v should name the stalled device", got.Reasons)
	}
	if got.DeviceConnected {
		t.Errorf("a stalled device must not still be reported as connected")
	}
}

// TestPipelineRejectsNon48kCapture pins the §6.1 fixed internal capture format.
func TestPipelineRejectsNon48kCapture(t *testing.T) {
	src := scripted(frames(20, tone(1, 0.3), 44100, 1))
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "wrong-rate-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
	})
	run(t, st)
	got := waitFor(t, st, "a format rejection", func(s model.AudioStreamState) bool {
		return settled(s) && s.Status == model.QualFailed
	})
	if !strings.Contains(strings.Join(got.Reasons, " "), "uses a fixed 48000 Hz internal capture format") {
		t.Errorf("reasons %v should name the required capture format", got.Reasons)
	}
}

// TestPipelineFormatChangeIsCounted covers the §6.3 "stability (device
// renumbering, dropouts, sample-rate glitches)" measurement: a device that
// re-enumerates with a different channel count mid-stream is reported, not
// silently de-interleaved with the stale width.
func TestPipelineFormatChangeIsCounted(t *testing.T) {
	script := append(frames(30, tone(2, 0.3), testRate, 2), frames(60, tone(3, 0.3), testRate, 3)...)
	src := scripted(script)
	src.hold = true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "renumbering-0", Stream: model.StreamHI, Source: model.ChannelSummed,
		AnalysisWindow:    50 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
	})
	run(t, st)
	// Wait for the per-SESSION stability reason, not the immediate one. Two
	// different states mention the format change: the live publish at the moment
	// the odd frame arrives, which still describes the window it measured (the
	// old width — honestly, since that window really was 2-channel), and the next
	// completed analysis block, which carries the new width. Matching on
	// "changed capture format" alone caught whichever came first and made this
	// test flaky; the session reason is only ever emitted from a completed block.
	got := waitFor(t, st, "the format change to be reported on a completed block", func(s model.AudioStreamState) bool {
		return strings.Contains(strings.Join(s.Reasons, " "), "this session")
	})
	if got.Status.Rank() < model.QualMarginal.Rank() {
		t.Errorf("a device that changed format mid-stream must be demoted, got %q", got.Status)
	}
	if got.Channels != 3 {
		t.Errorf("Channels = %d, want the NEW device width 3", got.Channels)
	}
}

// TestPipelineCountsDropouts asserts §6.6's dropout metric is real: frames that
// arrive far later than a capture period are counted as audio the device never
// delivered, and a recent dropout demotes the verdict.
func TestPipelineCountsDropouts(t *testing.T) {
	src := scripted(frames(8, tone(1, 0.3), testRate, 1))
	src.gap, src.hold = 60*time.Millisecond, true
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "glitchy-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow:    20 * time.Millisecond,
		DropoutAfter:      30 * time.Millisecond,
		StaleFrameTimeout: 10 * time.Second,
	})
	run(t, st)
	got := waitFor(t, st, "dropouts to be counted", func(s model.AudioStreamState) bool {
		return s.Dropouts > 0
	})
	if got.Status.Rank() < model.QualMarginal.Rank() {
		t.Errorf("a stream with recent dropouts must be demoted, got %q (%v)", got.Status, got.Reasons)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "dropouts") {
		t.Errorf("reasons %v should name the dropouts", got.Reasons)
	}
}

// TestPipelineEncoderStateAndLatency pins the honesty rule for §6.6's encoder
// state and estimated latency: a backend that encodes NOTHING reports
// "unavailable" and contributes no latency figure, and no state ever claims a
// measured end-to-end latency (that needs the Phase 0C rig, BLOCKERS B3).
func TestPipelineEncoderStateAndLatency(t *testing.T) {
	t.Run("no encoder reports no latency", func(t *testing.T) {
		src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
		st := NewStream(src, encode.NullBackend{}, StreamConfig{
			DeviceID: "synthetic-0", Stream: model.StreamHI, Source: model.ChannelMono,
			AnalysisWindow: 50 * time.Millisecond,
		})
		run(t, st)
		got := waitFor(t, st, "a verdict", func(s model.AudioStreamState) bool { return s.Status.Usable() })
		if got.EncoderState != EncoderUnavailable {
			t.Errorf("EncoderState = %q, want %q", got.EncoderState, EncoderUnavailable)
		}
		if got.LatencyMSEst != 0 {
			t.Errorf("LatencyMSEst = %d, want 0 when nothing was encoded", got.LatencyMSEst)
		}
		if got.LatencyMeasured {
			t.Errorf("LatencyMeasured must be false: no end-to-end latency has been measured (B3)")
		}
		if got.ConnectedClients != 0 {
			t.Errorf("ConnectedClients = %d, want 0 until a subscriber transport exists", got.ConnectedClients)
		}
	})

	t.Run("a real encoder reports the encode-path estimate", func(t *testing.T) {
		g := encode.NewGStreamerBackend()
		if !g.Available() {
			t.Skip("gst-launch-1.0 / opusenc not available on this host")
		}
		src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
		st := NewStream(src, g, StreamConfig{
			DeviceID: "synthetic-0", Stream: model.StreamHI, Source: model.ChannelMono,
			AnalysisWindow: 50 * time.Millisecond,
		})
		run(t, st)
		got := waitFor(t, st, "an encode-path estimate", func(s model.AudioStreamState) bool {
			return s.EncoderState == EncoderReadyNoTransport && s.LatencyMSEst > 0
		})
		// 10 ms capture period + 10 ms Opus frame; no invented jitter term.
		if want := EncoderPathLatencyMS(10, encode.DefaultOpusParams().FrameMS); got.LatencyMSEst != want {
			t.Errorf("LatencyMSEst = %d, want %d (capture period + Opus frame)", got.LatencyMSEst, want)
		}
		if got.LatencyMeasured {
			t.Errorf("an estimate must never be reported as a measurement")
		}
	})
}

// TestEncoderPathLatencyMS pins the framing model: ONE accumulation, being the
// smallest whole number of capture periods that holds at least one Opus frame.
//
// The first case is the correction. A 10 ms capture period feeding 10 ms Opus
// frames is one wait, not two: the block is handed straight to the encoder and
// becomes exactly one frame. The old estimate said 20 ms; §11.4 measured the
// whole appliance path at ~10.7 ms.
func TestEncoderPathLatencyMS(t *testing.T) {
	tests := []struct {
		name          string
		period, frame int
		want          int
		why           string
	}{
		{name: "the shipped settings are ONE accumulation", period: 10, frame: 10, want: 10,
			why: "one captured block IS one Opus frame"},
		{name: "several blocks make a frame", period: 5, frame: 10, want: 10,
			why: "the encoder waits for the second block"},
		{name: "a block longer than a frame waits for the block", period: 20, frame: 10, want: 20,
			why: "the block yields two frames at once; the wait is the block"},
		{name: "a period that does not divide the frame costs a whole extra block", period: 7, frame: 10, want: 14,
			why: "two 7 ms blocks, because one is not enough for a 10 ms frame"},
		{name: "the §6.7 stability fallback frame", period: 10, frame: 20, want: 20,
			why: "two 10 ms blocks make one 20 ms frame"},
		{name: "unknown frame size falls back to the default", period: 5, frame: 0,
			want: encode.DefaultOpusParams().FrameMS, why: "default frame, made of whole 5 ms blocks"},
		{name: "no capture period observed yet", period: 0, frame: 10, want: 10,
			why: "only the encoder's own frame can be accounted for"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncoderPathLatencyMS(tc.period, tc.frame); got != tc.want {
				t.Errorf("EncoderPathLatencyMS(%d, %d) = %d, want %d — %s",
					tc.period, tc.frame, got, tc.want, tc.why)
			}
		})
	}
}

// The estimate must never exceed what §11.4 MEASURED the whole appliance-side
// path to be. The old value did: it claimed 20 ms where the harness measured
// ~10.7 ms end to end, which is the shape of a budget padded by a term that does
// not exist.
func TestTheEstimateDoesNotExceedTheMeasuredApplianceFloor(t *testing.T) {
	// §11.4 on this host: 10 ms of structural framing + 651 µs measured p95.
	const measuredApplianceFloorMS = 11
	got := EncoderPathLatencyMS(10, encode.DefaultOpusParams().FrameMS)
	if got > measuredApplianceFloorMS {
		t.Fatalf("the structural estimate is %d ms; §11.4 measured the whole appliance path at ~10.7 ms",
			got)
	}
}

// TestPipelineTierSelectsThresholds confirms the §6.2 tier reaches the qualifier
// through the stream config, and that an override layers on top of it.
func TestPipelineTierSelectsThresholds(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "jack-0", Stream: model.StreamHI, Source: model.ChannelMono,
		Tier:       qualifier.Tier0BuiltInJack,
		Thresholds: qualifier.Thresholds{SustainedSilenceMS: 1234},
	})
	th := st.Thresholds()
	if want := qualifier.ThresholdsForTier(qualifier.Tier0BuiltInJack).DCOffsetAbs; th.DCOffsetAbs != want {
		t.Errorf("DCOffsetAbs = %v, want the tier-0 value %v", th.DCOffsetAbs, want)
	}
	if th.SustainedSilenceMS != 1234 {
		t.Errorf("SustainedSilenceMS = %d, want the caller override 1234", th.SustainedSilenceMS)
	}

	unset := NewStream(src, nil, StreamConfig{DeviceID: "default-0", Stream: model.StreamHI, Source: model.ChannelMono})
	if unset.Thresholds() != qualifier.DefaultThresholds() {
		t.Errorf("an unset tier must use the shipped tier-2 defaults")
	}
}

// TestPipelineEmitsAudioEvents asserts the §9.4 audio event taxonomy is actually
// emitted on transitions — the events existed with no emitters — and that no
// audio content reaches the log (guardrail: nothing sensitive in logs).
func TestPipelineEmitsAudioEvents(t *testing.T) {
	var sink syncBuffer
	log := logging.New(logging.Options{Component: "media-test", Writer: &sink, JSON: true})

	src := scripted(frames(20, tone(1, 0.3), testRate, 1)) // closes at the end
	st := NewStream(src, nil, StreamConfig{
		DeviceID: "events-0", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond,
		Logger:         log,
	})
	run(t, st)
	waitFor(t, st, "a disconnect", func(s model.AudioStreamState) bool {
		return s.Status == model.QualFailed && strings.Contains(strings.Join(s.Reasons, " "), "disconnected")
	})

	out := sink.String()
	for _, ev := range []logging.Event{
		logging.EvAudioInputConnected,
		logging.EvAudioQualifierVerdict,
		logging.EvAudioInputDisconnected,
	} {
		if !strings.Contains(out, string(ev)) {
			t.Errorf("event %q was never emitted; log:\n%s", ev, out)
		}
	}
	for _, forbidden := range []string{"samples", "\"pcm\"", "audio_data"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("log contains %q — audio content must never be logged (§9.4)", forbidden)
		}
	}
}

// TestPipelineEmitsClippingAndSilenceEvents covers the two remaining §9.4 audio
// events, which had no emitters at all: clipping and sustained silence are
// recorded ON ENTRY, so a booth log shows when a fault started rather than only
// that a later poll found it.
func TestPipelineEmitsClippingAndSilenceEvents(t *testing.T) {
	t.Run("clipping", func(t *testing.T) {
		var sink syncBuffer
		src := scripted(frames(20, saturated(1), testRate, 1))
		src.hold = true
		st := NewStream(src, nil, StreamConfig{
			DeviceID: "hot-0", Stream: model.StreamHI, Source: model.ChannelMono,
			AnalysisWindow:    50 * time.Millisecond,
			StaleFrameTimeout: 10 * time.Second,
			Logger:            logging.New(logging.Options{Component: "media-test", Writer: &sink, JSON: true}),
		})
		run(t, st)
		waitFor(t, st, "a clipping verdict", func(s model.AudioStreamState) bool { return s.Clipping })
		if out := sink.String(); !strings.Contains(out, string(logging.EvAudioLevelClipping)) {
			t.Errorf("clipping event was never emitted; log:\n%s", out)
		}
	})

	t.Run("sustained silence", func(t *testing.T) {
		var sink syncBuffer
		src := scripted(frames(150, silence(1), testRate, 1))
		src.hold = true
		st := NewStream(src, nil, StreamConfig{
			DeviceID: "dead-1", Stream: model.StreamHI, Source: model.ChannelMono,
			AnalysisWindow:    50 * time.Millisecond,
			StaleFrameTimeout: 10 * time.Second,
			Thresholds:        qualifier.Thresholds{SustainedSilenceMS: 1000},
			Logger:            logging.New(logging.Options{Component: "media-test", Writer: &sink, JSON: true}),
		})
		run(t, st)
		waitFor(t, st, "a sustained-silence failure", func(s model.AudioStreamState) bool {
			return settled(s) && s.Status == model.QualFailed
		})
		if out := sink.String(); !strings.Contains(out, string(logging.EvAudioSilenceSustained)) {
			t.Errorf("sustained-silence event was never emitted; log:\n%s", out)
		}
	})
}
