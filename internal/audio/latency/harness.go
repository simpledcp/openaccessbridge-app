package latency

// The §11.4 harness itself. It drives the PRODUCTION path rather than a
// re-implementation of it — the real internal/audio/pipeline.Stream, the real
// gst-launch Opus encoder, the real webrtcpeer.Hub — and takes its timestamps
// from a tap wrapped around the encode.RTPStream seam. A harness that measured
// its own copy of the pipeline would report a number no patron's audio ever
// travelled through.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/encode"
	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/pipeline"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/audio/webrtcpeer"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// ErrNoMeasurement reports that the streaming half of the harness could not run,
// so no Opus or transport figure exists. It is returned INSTEAD of a Result with
// zeroed stages: an absent measurement must never be readable as "0 ms".
var ErrNoMeasurement = errors.New("latency: the appliance-side streaming path could not be measured on this host")

// Config parameterises a run. The zero value is not useful; use DefaultConfig.
type Config struct {
	// Duration is the sampling window for the streaming stages, AFTER warm-up.
	// §11.4 wants a distribution, so this has to be long enough that a p95 means
	// something: at a 10 ms frame it is 100 samples per second.
	Duration time.Duration `json:"duration"`
	// Warmup is discarded before sampling starts. The encoder's FIRST packets
	// carry gst-launch process start-up, which is a start-up cost and not the
	// steady state a patron listens through; including them would report a
	// worse floor than the appliance has.
	Warmup time.Duration `json:"warmup"`
	// FrameMS is the §6.7 Opus frame and the capture period fed to it.
	FrameMS int `json:"frame_ms"`
	// SampleRate is §6.1's fixed internal capture format.
	SampleRate int `json:"sample_rate"`
	// Channels is the width of the mapped lane (§6.7 mono per stream).
	Channels int `json:"channels"`
	// AnalysisWindow is the §6.3 qualification block.
	AnalysisWindow time.Duration `json:"analysis_window"`
	// AnalysisBlocks is how many blocks the pure-Go stages are timed over.
	AnalysisBlocks int `json:"analysis_blocks"`
	// AnalysisOnly measures the pure-Go stages and nothing else: no gst-launch
	// encoder is spawned and no peer connection is negotiated. It is what
	// `go test -short` runs, and a Result from it reports the streaming stages
	// as unmeasured rather than as fast.
	AnalysisOnly bool `json:"analysis_only"`
}

// DefaultConfig is the short run the normal test suite carries: long enough for
// a p95 to be a reading rather than an anecdote, short enough to sit in CI.
func DefaultConfig() Config {
	return Config{
		Duration:       3 * time.Second,
		Warmup:         1500 * time.Millisecond,
		FrameMS:        encode.DefaultOpusParams().FrameMS,
		SampleRate:     pipeline.DefaultSampleRate,
		Channels:       1,
		AnalysisWindow: 200 * time.Millisecond,
		AnalysisBlocks: 200,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Duration <= 0 {
		c.Duration = d.Duration
	}
	if c.Warmup < 0 {
		c.Warmup = d.Warmup
	}
	if c.FrameMS <= 0 {
		c.FrameMS = d.FrameMS
	}
	if c.SampleRate <= 0 {
		c.SampleRate = d.SampleRate
	}
	if c.Channels <= 0 {
		c.Channels = d.Channels
	}
	if c.AnalysisWindow <= 0 {
		c.AnalysisWindow = d.AnalysisWindow
	}
	if c.AnalysisBlocks <= 0 {
		c.AnalysisBlocks = d.AnalysisBlocks
	}
	return c
}

// framesPerBlock is how many samples per channel one Opus frame / capture period
// covers.
func (c Config) framesPerBlock() int { return c.SampleRate * c.FrameMS / 1000 }

// Run measures the appliance-side path. The pure-Go stages are always measured;
// the streaming stages need an Opus encoder and a non-loopback host candidate
// for two WebRTC peers to meet on, and their absence is reported as a skip
// reason rather than as a fast result.
func Run(ctx context.Context, cfg Config) (Result, error) {
	cfg = cfg.withDefaults()
	res := Result{
		Config:     cfg,
		FramingMS:  cfg.FrameMS,
		EstimateMS: pipeline.EncoderPathLatencyMS(cfg.FrameMS, cfg.FrameMS),
	}

	mapD, qualD, err := measureAnalysis(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.Stages = append(res.Stages,
		Stage{
			Name:    StageChannelMap,
			Bracket: "one mapper.Extract of one capture frame into this stream's lane (§6.5), on the DELIVERY path",
			Excludes: "the capture period itself — the frame already exists when Extract is called. " +
				"CPU cost only, with no hardware in it (B3).",
			D: mapD,
		},
		Stage{
			Name:    StageLevelAnalysis,
			Bracket: "one mapper.Extract plus one qualifier.Analyze of a full §6.3 analysis block",
			Excludes: "a patron's latency entirely — §6.1's analysis tap runs BESIDE the encode path, " +
				"not in front of it, so this cost is never in the delivery chain. It is measured " +
				"against §6.7's \"<5 ms\" analysis row, not added to the floor.",
			D: qualD,
		},
	)

	if cfg.AnalysisOnly {
		res.SkipReason = "analysis-only run: no encoder was started and no peer connection was negotiated"
		return res, nil
	}
	if err := measureStreaming(ctx, cfg, &res); err != nil {
		res.SkipReason = err.Error()
		return res, fmt.Errorf("%w: %v", ErrNoMeasurement, err)
	}
	res.StreamingMeasured = true
	return res, nil
}

// measureAnalysis times the pure-Go stages. It calls the SAME functions the
// pipeline calls, on frames of the same shape, so a regression in either shows
// up here.
func measureAnalysis(ctx context.Context, cfg Config) (Distribution, Distribution, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	src := &capture.SyntheticTone{
		FreqHz: 1000, Amplitude: 0.3,
		SampleRate: cfg.SampleRate, Channels: cfg.Channels,
		FrameSize: cfg.framesPerBlock(),
		// NOT Realtime: this stage measures CPU cost per call, so the harness
		// wants blocks as fast as it can qualify them. The streaming half below
		// is paced, because there a generator running faster than the clock
		// would describe the generator instead of a booth feed.
	}
	frames, err := src.Start(ctx)
	if err != nil {
		return Distribution{}, Distribution{}, fmt.Errorf("latency: start analysis source: %w", err)
	}
	defer func() { _ = src.Close() }()

	var (
		mapSamples  = make([]time.Duration, 0, cfg.AnalysisBlocks*32)
		qualSamples = make([]time.Duration, 0, cfg.AnalysisBlocks)
		acc         []float64
		target      = cfg.framesPerBlock() * cfg.Channels * int(cfg.AnalysisWindow.Milliseconds()) / cfg.FrameMS
		th          = qualifier.ThresholdsForTier("") // the shipped default tier profile (§6.2)
	)
	if target < cfg.Channels {
		target = cfg.Channels
	}
	for len(qualSamples) < cfg.AnalysisBlocks {
		select {
		case <-ctx.Done():
			return Distribution{}, Distribution{}, ctx.Err()
		case f, ok := <-frames:
			if !ok {
				return Distribution{}, Distribution{}, errors.New("latency: analysis source stopped early")
			}
			// The delivery-path mapping: one Extract per capture frame.
			t0 := time.Now()
			_, err := mapper.Extract(model.ChannelMono, f.Samples, f.Channels)
			mapSamples = append(mapSamples, time.Since(t0))
			if err != nil {
				return Distribution{}, Distribution{}, fmt.Errorf("latency: channel map: %w", err)
			}

			acc = append(acc, f.Samples...)
			if len(acc) < target {
				continue
			}
			// The §6.6 analysis tap: map the block, then qualify it.
			t1 := time.Now()
			blockLane, mErr := mapper.Extract(model.ChannelMono, acc, f.Channels)
			if mErr != nil {
				return Distribution{}, Distribution{}, fmt.Errorf("latency: block channel map: %w", mErr)
			}
			qualifier.Analyze(qualifier.Input{
				Samples: blockLane, SampleRate: f.SampleRate,
				Channels: 1, DeviceChannels: f.Channels,
				Present: true, DeviceConnected: true, DuringPlayback: true,
				InputDeviceID: syntheticDeviceID, StreamType: model.StreamHI,
				SourceChannel: model.ChannelMono,
			}, th)
			qualSamples = append(qualSamples, time.Since(t1))
			acc = acc[:0]
		}
	}
	return newDistribution(mapSamples), newDistribution(qualSamples), nil
}

// syntheticDeviceID is the identity the harness's source reports. It says what
// it is: this codebase has already been caught reporting a synthetic tone under
// a real device's identity, and a latency figure attributed to booth hardware
// that never existed would be the same fabrication in a new place (B3).
const syntheticDeviceID = "latency-harness:synthetic-tone (no capture hardware — B3)"

// measureStreaming runs the real pipeline into the real peer hub and joins the
// per-packet timestamps.
func measureStreaming(ctx context.Context, cfg Config, res *Result) error {
	if err := requireHostCandidate(); err != nil {
		return err
	}
	backend := encode.NewGStreamerBackend()
	if !backend.Available() {
		return errors.New("no gst-launch/opusenc on this host, so no Opus was encoded and nothing is claimed about the encode path")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hub, err := webrtcpeer.New(webrtcpeer.Options{})
	if err != nil {
		return fmt.Errorf("peer hub: %w", err)
	}
	defer hub.Close()

	tp := newTap(cfg)
	src := &capture.SyntheticTone{
		FreqHz: 1000, Amplitude: 0.3,
		SampleRate: cfg.SampleRate, Channels: cfg.Channels,
		FrameSize: cfg.framesPerBlock(),
		// Paced at 1x: 10 ms of audio per 10 ms of clock. Every figure below is
		// a wall-clock delay, so an unpaced generator would make all of them
		// describe the generator's speed rather than a feed (see B3).
		Realtime: true,
	}
	st := pipeline.NewStream(src, &tapBackend{Backend: backend, tap: tp}, pipeline.StreamConfig{
		DeviceID:       syntheticDeviceID,
		Stream:         model.StreamHI,
		Source:         model.ChannelMono,
		AnalysisWindow: cfg.AnalysisWindow,
		Delivery:       hub,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = st.Run(runCtx, true)
	}()
	defer func() {
		cancel()
		<-done
	}()

	if err := waitFor(runCtx, 15*time.Second, func() bool { return hub.Publishing(model.StreamHI) }); err != nil {
		return fmt.Errorf("no encoder was published for %s within the deadline", model.StreamHI)
	}

	sub, err := newSubscriber()
	if err != nil {
		return err
	}
	defer sub.close()
	if err := sub.connect(runCtx, hub, "latency-harness"); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if err := waitFor(runCtx, 20*time.Second, func() bool { return sub.received() > 0 }); err != nil {
		return errors.New("no RTP reached the subscriber, so there is no transport measurement")
	}

	// Warm-up is discarded, then the sampling window runs.
	if err := sleepCtx(runCtx, cfg.Warmup); err != nil {
		return err
	}
	from := time.Now()
	if err := sleepCtx(runCtx, cfg.Duration); err != nil {
		return err
	}
	until := time.Now()

	cancel()
	<-done
	return tp.join(sub.snapshot(), from, until, res)
}

// requireHostCandidate refuses to run rather than reporting a transport figure
// that never crossed a peer connection: two Pion peers with no STUN and no TURN
// (§6.7) need a non-loopback host candidate to meet on.
func requireHostCandidate() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("cannot enumerate interfaces: %w", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		if addrs, aErr := i.Addrs(); aErr == nil && len(addrs) > 0 {
			return nil
		}
	}
	return errors.New("no non-loopback interface: two local WebRTC peers have no host candidate to meet on")
}

// --- the tap -----------------------------------------------------------------

// tapBackend wraps the real encoder backend so the harness can time the seam
// without changing the pipeline.
type tapBackend struct {
	encode.Backend
	tap *tap
}

func (b *tapBackend) StartRTP(ctx context.Context, f encode.PCMFormat, p encode.OpusParams) (encode.RTPStream, error) {
	s, ok := b.Backend.(encode.Streamer)
	if !ok {
		return nil, encode.ErrNoStreamingEncoder
	}
	inner, err := s.StartRTP(ctx, f, p)
	if err != nil {
		return nil, err
	}
	b.tap.attach(inner)
	return b.tap, nil
}

// egressSample is one RTP packet as it left the encoder's own socket.
type egressSample struct {
	at  time.Time
	ord int // the WritePCM ordinal whose audio this packet carries
}

// arrival is one RTP packet as a subscriber parsed it.
type arrival struct {
	ts uint32
	at time.Time
}

// tap sits on the encode.RTPStream seam and records two timestamps per packet:
// when the block was handed to the encoder, and when the packet carrying it came
// back out. It holds no audio — only clock readings and counters (§9.4).
//
// CORRELATION. The encoder is a strict FIFO with DTX off (§6.7), so the Nth
// accepted PCM block becomes the Nth emitted RTP packet. That ordinal
// correspondence is not assumed on faith: the RTP sequence number is checked on
// every packet, and a discontinuity RESYNCHRONISES the ordinal (and is counted)
// rather than letting the correlation drift by a frame for the rest of the run.
// A write ordinal is recorded only when the encoder ACCEPTED the block —
// encode.StreamStats.FramesIn is read across the call — so a dropped block
// cannot shift every later packet onto the wrong write.
type tap struct {
	cfg          Config
	frameSamples uint32

	mu     sync.Mutex
	inner  encode.RTPStream
	writes []time.Time
	egress map[uint32]egressSample

	ord       int
	havePkt   bool
	seq0      uint16
	lastTS    uint32
	stride0   uint32
	strides   int
	strideOK  bool
	resyncs   int
	malformed int
}

func newTap(cfg Config) *tap {
	return &tap{
		cfg:          cfg,
		frameSamples: uint32(cfg.framesPerBlock()),
		egress:       map[uint32]egressSample{},
		strideOK:     true,
	}
}

func (t *tap) attach(inner encode.RTPStream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inner = inner
}

func (t *tap) stream() encode.RTPStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inner
}

// WritePCM records the instant the completed block was handed over — which is
// the instant its LAST sample became available to the encoder — and only if the
// encoder took it.
func (t *tap) WritePCM(samples []float64) error {
	s := t.stream()
	if s == nil {
		return errors.New("latency: tap has no encoder attached")
	}
	before := s.Stats().FramesIn
	at := time.Now()
	err := s.WritePCM(samples)
	if s.Stats().FramesIn > before {
		t.mu.Lock()
		t.writes = append(t.writes, at)
		t.mu.Unlock()
	}
	return err
}

// ReadRTP records the instant the packet left the encoder's socket, keyed by its
// RTP timestamp so the subscriber can join on packet identity rather than on
// arrival order.
func (t *tap) ReadRTP(buf []byte) (int, error) {
	s := t.stream()
	if s == nil {
		return 0, errors.New("latency: tap has no encoder attached")
	}
	n, err := s.ReadRTP(buf)
	if err != nil {
		return n, err
	}
	at := time.Now()
	var h rtp.Header
	if _, uErr := h.Unmarshal(buf[:n]); uErr != nil {
		t.mu.Lock()
		t.malformed++
		t.mu.Unlock()
		return n, nil
	}
	t.mu.Lock()
	t.note(h, at)
	t.mu.Unlock()
	return n, nil
}

// note folds one packet into the correlation state. Caller holds t.mu.
func (t *tap) note(h rtp.Header, at time.Time) {
	if !t.havePkt {
		t.havePkt, t.seq0, t.ord, t.lastTS = true, h.SequenceNumber, 0, h.Timestamp
	} else {
		// Resynchronise the ordinal from the sequence number. In the ordinary
		// case the delta is exactly 1 and this is a no-op.
		expect := t.seq0 + uint16(t.ord) + 1
		delta := int(int16(h.SequenceNumber - expect))
		if delta != 0 {
			t.resyncs++
		}
		t.ord += 1 + delta

		stride := h.Timestamp - t.lastTS
		t.lastTS = h.Timestamp
		t.strides++
		if t.strides == 1 {
			t.stride0 = stride
		} else if stride != t.frameSamples {
			// Some later stride is not exactly one frame, so the run is not the
			// clean ramp the pre-skip reading depends on. Claim nothing.
			t.strideOK = false
		}
	}
	if t.ord < 0 || t.ord >= len(t.writes) {
		// No write is held for this packet — it cannot be attributed, so it is
		// not recorded at all rather than attributed to the nearest one.
		return
	}
	t.egress[h.Timestamp] = egressSample{at: at, ord: t.ord}
}

func (t *tap) Stats() encode.StreamStats {
	if s := t.stream(); s != nil {
		return s.Stats()
	}
	return encode.StreamStats{}
}

func (t *tap) Err() error {
	if s := t.stream(); s != nil {
		return s.Err()
	}
	return nil
}

func (t *tap) Close() error {
	if s := t.stream(); s != nil {
		return s.Close()
	}
	return nil
}

// join turns the two recorded sides into the streaming stages, keeping only
// packets whose encoder egress fell inside the sampling window.
func (t *tap) join(arrivals []arrival, from, until time.Time, res *Result) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	res.PacketsEncoded = len(t.egress)
	res.PacketsReceived = len(arrivals)
	res.SeqResyncs = t.resyncs
	if s := t.inner; s != nil {
		res.EncoderPCMDropped = s.Stats().FramesDropped
	}
	// The pre-skip reading, only when the RTP clock was a clean ramp with a
	// short FIRST stride. Anything else and nothing is reported.
	if t.strideOK && t.strides > 1 && t.stride0 > 0 && t.stride0 < t.frameSamples {
		res.OpusPreSkipSamples = int(t.frameSamples - t.stride0)
	}

	var encodeD, fanoutD, floorD []time.Duration
	for _, a := range arrivals {
		e, ok := t.egress[a.ts]
		if !ok {
			res.Unjoined++
			continue
		}
		if e.at.Before(from) || e.at.After(until) {
			continue
		}
		w := t.writes[e.ord]
		enc, fan, floor := e.at.Sub(w), a.at.Sub(e.at), a.at.Sub(w)
		if enc < 0 || fan < 0 {
			// A packet cannot leave the encoder before its audio went in, nor
			// reach a subscriber before it left. Either means the correlation is
			// wrong, and a wrong correlation must abort the run rather than
			// quietly report a shorter floor than the appliance has.
			return fmt.Errorf("latency: correlation is inconsistent (encode %v, fan-out %v): refusing to report a floor from it", enc, fan)
		}
		encodeD = append(encodeD, enc)
		fanoutD = append(fanoutD, fan)
		floorD = append(floorD, floor)
	}
	if len(floorD) == 0 {
		return errors.New("no packet could be joined across the encoder and the subscriber inside the sampling window")
	}

	res.Stages = append(res.Stages,
		Stage{
			Name:    StageOpusEncode,
			Bracket: "a complete 10 ms block handed to the Opus encoder → the RTP packet carrying that block read back off the encoder's own socket",
			Excludes: "the framing delay of accumulating the block (stated separately below) and " +
				"Opus DECODE, which nothing here performs.",
			D: newDistribution(encodeD),
		},
		Stage{
			Name:    StageWebRTCFanout,
			Bracket: "that packet off the encoder's socket → the same packet (joined by RTP timestamp) parsed by a Pion subscriber, through webrtcpeer.Hub's fan-out, a real SRTP session and an ICE host-candidate hop",
			Excludes: "any network — both peers are processes on this one machine — and any jitter " +
				"buffer: a Pion receiver hands packets up as they arrive, which is the single " +
				"biggest reason this is a floor and not an experience (B2).",
			D: newDistribution(fanoutD),
		},
		Stage{
			Name:    StageApplianceFloor,
			Bracket: "one packet across both stages above: block handed to the encoder → parsed by the subscriber. A measurement on ONE packet, not a sum of percentiles",
			Excludes: "everything in the NOT MEASURED list below — capture hardware, Wi-Fi, the " +
				"phone-side jitter buffer, Opus decode, and phone audio output.",
			D: newDistribution(floorD),
		},
	)
	return nil
}

// --- the subscriber ----------------------------------------------------------

// subscriber is the patron half: a receive-only Pion peer that OFFERS, exactly
// as internal/web/assets/patron.js does, with no ICE servers (§6.7 local only).
type subscriber struct {
	pc  *webrtc.PeerConnection
	got atomic.Uint64

	mu       sync.Mutex
	arrivals []arrival
}

func newSubscriber() (*subscriber, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("latency: subscriber codecs: %w", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{
		// §6.7: no STUN, no TURN, ever.
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return nil, fmt.Errorf("latency: subscriber peer: %w", err)
	}
	s := &subscriber{pc: pc}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("latency: subscriber transceiver: %w", err)
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			at := time.Now()
			s.got.Add(1)
			s.mu.Lock()
			s.arrivals = append(s.arrivals, arrival{ts: p.Timestamp, at: at})
			s.mu.Unlock()
		}
	})
	return s, nil
}

// connect runs the §6.7 exchange: the subscriber offers, the appliance answers
// with gathering complete, and the subscriber trickles its candidates behind it.
func (s *subscriber) connect(ctx context.Context, h *webrtcpeer.Hub, peerID string) error {
	s.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = h.AddCandidate(peerID, c.ToJSON())
	})
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	answer, err := h.Answer(ctx, peerID, model.StreamHI, offer.SDP, 0)
	if err != nil {
		return err
	}
	return s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
}

func (s *subscriber) received() uint64 { return s.got.Load() }

func (s *subscriber) snapshot() []arrival {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]arrival, len(s.arrivals))
	copy(out, s.arrivals)
	return out
}

func (s *subscriber) close() { _ = s.pc.Close() }

// --- small helpers -----------------------------------------------------------

func waitFor(ctx context.Context, within time.Duration, cond func() bool) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	return context.DeadlineExceeded
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
