package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// --- delivery test doubles ------------------------------------------------

// fakeRTP is a live encode whose behaviour the test dictates: how much PCM it
// accepted, whether it ever produced a packet, and whether it died.
type fakeRTP struct {
	mu        sync.Mutex
	written   int
	packets   uint64
	dropped   uint64
	err       error
	closed    bool
	emit      bool // produce a packet per PCM block
	lastBlock []float64
}

func (f *fakeRTP) WritePCM(samples []float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	f.written++
	f.lastBlock = append([]float64(nil), samples...)
	if f.emit {
		f.packets++
	}
	return nil
}

func (f *fakeRTP) ReadRTP([]byte) (int, error) { return 0, errors.New("not used") }

func (f *fakeRTP) Stats() encode.StreamStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return encode.StreamStats{FramesIn: uint64(f.written), PacketsOut: f.packets, FramesDropped: f.dropped}
}

func (f *fakeRTP) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeRTP) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeRTP) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeRTP) blocks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

func (f *fakeRTP) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// streamingBackend is an encode.Backend that also implements encode.Streamer.
type streamingBackend struct {
	mu      sync.Mutex
	starts  int
	format  encode.PCMFormat
	stream  *fakeRTP
	failing bool
}

func (b *streamingBackend) Name() string    { return "fake-streaming" }
func (b *streamingBackend) Available() bool { return true }
func (b *streamingBackend) SmokeTest(context.Context, time.Duration, encode.OpusParams) error {
	return nil
}

func (b *streamingBackend) StartRTP(_ context.Context, f encode.PCMFormat, _ encode.OpusParams) (encode.RTPStream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	b.format = f
	if b.failing {
		return nil, encode.ErrNoStreamingEncoder
	}
	return b.stream, nil
}

func (b *streamingBackend) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}

func (b *streamingBackend) startFormat() encode.PCMFormat {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.format
}

// fakeSink records what the pipeline published and reports a listener count.
type fakeSink struct {
	mu          sync.Mutex
	published   map[model.StreamType]encode.RTPStream
	unpublished []model.StreamType
	listeners   int
	publishErr  error
}

func newSink() *fakeSink {
	return &fakeSink{published: map[model.StreamType]encode.RTPStream{}}
}

func (s *fakeSink) Publish(st model.StreamType, rtp encode.RTPStream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishErr != nil {
		return s.publishErr
	}
	s.published[st] = rtp
	return nil
}

func (s *fakeSink) Unpublish(st model.StreamType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.published, st)
	s.unpublished = append(s.unpublished, st)
}

func (s *fakeSink) Listeners(model.StreamType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listeners
}

func (s *fakeSink) setListeners(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = n
}

func (s *fakeSink) publishCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published)
}

func (s *fakeSink) unpublishedStreams() []model.StreamType {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.StreamType(nil), s.unpublished...)
}

// --- tests ----------------------------------------------------------------

// §6.7: the mapped lane of a running stream is what the Opus encoder encodes,
// and the resulting RTP source is published to the fan-out exactly once — not
// once per subscriber, and not once per analysis block.
func TestStreamPublishesOneLiveEncodeOfTheMappedLane(t *testing.T) {
	rtp := &fakeRTP{emit: true}
	backend := &streamingBackend{stream: rtp}
	sink := newSink()
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		DeviceID: "synthetic-HI", Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
	})
	run(t, st)

	state := waitFor(t, st, "the encoder to report streaming", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderStreaming
	})
	if state.LatencyMSEst <= 0 {
		t.Fatalf("a streaming encoder contributes no encode-path estimate: %+v", state)
	}
	if n := backend.startCount(); n != 1 {
		t.Fatalf("the backend was asked for %d live encodes; §6.7 wants exactly one per named stream", n)
	}
	if got := backend.startFormat(); got.SampleRate != testRate || got.Channels != 1 {
		t.Fatalf("the encoder was opened at %+v, not the mapped lane's format", got)
	}
	if sink.publishCount() != 1 {
		t.Fatalf("the fan-out holds %d sources for one stream", sink.publishCount())
	}
	if rtp.blocks() == 0 {
		t.Fatal("no mapped audio reached the encoder")
	}
}

// §6.6 wants the connected listener count per stream, and it must come from the
// transport that actually has the subscribers.
func TestListenerCountComesFromTheFanOut(t *testing.T) {
	sink := newSink()
	sink.setListeners(3)
	backend := &streamingBackend{stream: &fakeRTP{emit: true}}
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
	})
	run(t, st)
	waitFor(t, st, "the listener count to be reported", func(s model.AudioStreamState) bool {
		return s.ConnectedClients == 3
	})
}

// An encoder that starts and then produces NOTHING must be reported as
// unavailable, never as streaming. This is the fabrication guard on the encoder
// state: "the process launched" is not "audio is being encoded".
func TestAnEncoderThatProducesNoRTPIsReportedUnavailable(t *testing.T) {
	rtp := &fakeRTP{emit: false} // accepts PCM, emits nothing
	backend := &streamingBackend{stream: rtp}
	sink := newSink()
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
	})
	run(t, st)
	state := waitForLong(t, st, "the silent encoder to be reported unavailable", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderUnavailable
	})
	if state.LatencyMSEst != 0 {
		t.Fatalf("an unavailable encoder reported a latency estimate: %+v", state)
	}
	if !rtp.isClosed() {
		t.Fatal("the silent encoder was left running")
	}
	if len(sink.unpublishedStreams()) == 0 {
		t.Fatal("the silent encoder was left published to the fan-out")
	}
}

// A live encode that dies mid-show must take the published source down with it
// and be reported, not frozen at its last healthy state (guardrail #7).
func TestAnEncoderThatDiesIsUnpublishedAndReported(t *testing.T) {
	rtp := &fakeRTP{emit: true}
	backend := &streamingBackend{stream: rtp}
	sink := newSink()
	logs := &syncBuffer{}
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
		Logger: logging.New(logging.Options{Component: "test", JSON: true, Writer: logs}),
	})
	run(t, st)
	waitFor(t, st, "the encoder to report streaming", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderStreaming
	})
	rtp.setErr(errors.New("gst-launch exited"))
	waitForLong(t, st, "the dead encoder to be reported", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderUnavailable
	})
	if len(sink.unpublishedStreams()) == 0 {
		t.Fatal("a dead encoder was left published: subscribers would sit on a silent track")
	}
	if !strings.Contains(logs.String(), string(logging.EvAudioEncoderStopped)) {
		t.Fatalf("the encoder's death was not logged as a state transition:\n%s", logs.String())
	}
}

// A host with no streaming encoder must report no encoder rather than a stream
// nobody can hear — and must not respawn the missing encoder per capture frame.
func TestNoStreamingEncoderIsReportedNotRetriedPerFrame(t *testing.T) {
	backend := &streamingBackend{failing: true}
	sink := newSink()
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
	})
	run(t, st)
	waitFor(t, st, "the missing encoder to be reported", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderUnavailable
	})
	time.Sleep(300 * time.Millisecond)
	if n := backend.startCount(); n > 2 {
		t.Fatalf("a missing encoder was retried %d times in 300ms; the retry interval is not being honoured", n)
	}
	if sink.publishCount() != 0 {
		t.Fatal("a failed encode was published to the fan-out")
	}
}

// Stopping a stream must close its encoder and unpublish it: §6.7's peers come
// and go, but so do STREAMS, and a leaked gst-launch child per reconfiguration
// would accumulate across a show.
func TestStoppingAStreamReleasesItsEncoder(t *testing.T) {
	rtp := &fakeRTP{emit: true}
	backend := &streamingBackend{stream: rtp}
	sink := newSink()
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, backend, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono,
		AnalysisWindow: 50 * time.Millisecond, Delivery: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = st.Run(ctx, true) }()
	waitFor(t, st, "the encoder to report streaming", func(s model.AudioStreamState) bool {
		return s.EncoderState == EncoderStreaming
	})
	cancel()
	_ = st.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not stop")
	}
	if !rtp.isClosed() {
		t.Fatal("the live encoder outlived its stream")
	}
	if sink.publishCount() != 0 {
		t.Fatal("the stopped stream is still published to the fan-out")
	}
}

// A stream with no fan-out attached keeps the existing behaviour exactly: the
// one-shot smoke proves the encoder, nothing is delivered, and the listener
// count stays an honest 0.
func TestNoDeliverySinkKeepsTheSmokeTestPath(t *testing.T) {
	src := &capture.SyntheticTone{FreqHz: 440, Amplitude: 0.3, SampleRate: testRate, Channels: 1, FrameSize: 480}
	st := NewStream(src, encode.NullBackend{}, StreamConfig{
		Stream: model.StreamHI, Source: model.ChannelMono, AnalysisWindow: 50 * time.Millisecond,
	})
	run(t, st)
	state := waitFor(t, st, "a verdict", func(s model.AudioStreamState) bool { return settled(s) })
	if state.EncoderState != EncoderUnavailable {
		t.Fatalf("the no-op backend reported encoder state %q", state.EncoderState)
	}
	if state.ConnectedClients != 0 {
		t.Fatalf("a stream with no transport reported %d listeners", state.ConnectedClients)
	}
}

// waitForLong is waitFor with room for the delivery proof deadline.
func waitForLong(t *testing.T, s *Stream, what string, cond func(model.AudioStreamState) bool) model.AudioStreamState {
	t.Helper()
	deadline := time.After(6 * time.Second)
	for {
		st := s.Status()
		if cond(st) {
			return st
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last state: %+v", what, st)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
