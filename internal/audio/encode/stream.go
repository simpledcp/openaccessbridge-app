package encode

// Streaming Opus/RTP encode (buildplan.md §6.7). SmokeTest proves the encode
// chain exists; this is the chain a patron actually listens to: one live
// encoder per NAMED STREAM whose RTP is fanned out to N browser peers by
// captionbridge-media (§6.7 topology, "the design must avoid re-running
// capture/analysis per client").
//
// HOW PCM REACHES THE ENCODER. The §6.7 spike ran `audiotestsrc` inside the
// GStreamer pipeline, but the real source is the MAPPED output of
// internal/audio/pipeline — capture → channel mapper, already de-interleaved
// into this stream's lane (§6.5). So the source element is `fdsrc` on the
// child's stdin and the appliance writes that lane into it as S16LE:
//
//   - it keeps ONE mapper as the single place that decides which captured
//     channel feeds which named stream (§6.5). Letting GStreamer re-open the
//     device and split channels again would put a second, untested mapping in
//     the path, and the qualifier's verdict would describe a different lane
//     from the one a patron hears;
//   - it keeps the capture device owned by the pure-Go capture seam (§6.1
//     stable device ids, hot-plug), which is where the real ALSA source plugs
//     in (BLOCKER B3);
//   - fdsrc is a byte stream with no format of its own, so the caps are stated
//     explicitly by `rawaudioparse` rather than guessed. That element also
//     timestamps each buffer from the sample count, which is what makes the RTP
//     timestamps monotonic and exactly frame-spaced.
//
// The RTP leaves GStreamer over UDP on 127.0.0.1 (the transport the §6.7 spike
// proved pion/rtp parses) and is read back here. Nothing is written to disk:
// audio is transient content (§9.4, guardrail #5).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ErrNoStreamingEncoder reports that no live Opus/RTP encode could be started.
// Like ErrNoEncoder it is returned INSTEAD of a working-looking stand-in, so a
// host without GStreamer can never appear to be serving audio.
var ErrNoStreamingEncoder = errors.New("encode: no streaming Opus encoder on this host; nothing is being encoded")

// errStreamClosed is returned by a closed stream's WritePCM.
var errStreamClosed = errors.New("encode: streaming encoder is closed")

// PCMFormat is the shape of the mapped audio a caller feeds the encoder. It is
// the format of the LANE the channel mapper produced (§6.5), not of the capture
// device: a stereo-split input hands each named stream one mono lane.
type PCMFormat struct {
	SampleRate int
	Channels   int
}

func (f PCMFormat) validate() error {
	if f.SampleRate <= 0 {
		return fmt.Errorf("encode: sample rate %d is not a capture format", f.SampleRate)
	}
	if f.Channels < 1 || f.Channels > 2 {
		return fmt.Errorf("encode: %d channels cannot be an Opus stream (§6.7 mono per stream; stereo-pair delivers as one stream)", f.Channels)
	}
	return nil
}

// StreamStats is what a live encode has ACTUALLY done. Every field is a count of
// something observed, so a caller can tell "the encoder is running" from "the
// encoder was started" (guardrail: nothing unmeasured is reported as measured).
type StreamStats struct {
	// FramesIn is the number of PCM blocks accepted from the capture chain.
	FramesIn uint64
	// FramesDropped is the number of PCM blocks discarded because the encoder
	// was not draining them. Non-zero means a patron heard a gap.
	FramesDropped uint64
	// PacketsOut is the number of RTP packets read back out of the encoder.
	// Zero means nothing has been encoded yet, whatever state the process is in.
	PacketsOut uint64
}

// RTPStream is one live Opus encode, packetized as RTP. It is the unit §6.7
// calls a "capture/analysis/Opus source per named stream": exactly one exists
// per named stream and every subscriber's peer connection is fed from it.
//
// Implementations must never block WritePCM on a slow reader — the caller is the
// capture/qualify chain, which cannot stall on delivery (§6.1 independence).
type RTPStream interface {
	// WritePCM hands one block of mapped, interleaved samples in [-1, 1] to the
	// encoder. It does not block; audio that cannot be accepted is dropped and
	// counted in StreamStats.FramesDropped.
	WritePCM(samples []float64) error
	// ReadRTP blocks until the next RTP packet is available, copying it into buf.
	// It returns an error once the encode has stopped, so a reader goroutine ends
	// with the stream rather than leaking.
	ReadRTP(buf []byte) (int, error)
	// Stats reports what the encode has done so far.
	Stats() StreamStats
	// Err reports why the encode stopped, or nil while it is running.
	Err() error
	// Close stops the encoder and releases its process and socket.
	Close() error
}

// Streamer is the optional half of Backend that produces a live stream. It is
// separate from Backend so a backend that can only smoke-test stays honest about
// it rather than returning a stream that emits nothing.
type Streamer interface {
	// StartRTP begins a live Opus/RTP encode of audio in format f.
	StartRTP(ctx context.Context, f PCMFormat, p OpusParams) (RTPStream, error)
}

// StartRTP starts the live encode for one named stream. The UDP socket is bound
// FIRST, on loopback with an ephemeral port, and the port is then handed to
// udpsink: binding first means the port cannot be taken between choosing it and
// using it, and binding on 127.0.0.1 means nothing off this appliance can reach
// the pre-SRTP media (§9.4 local only).
func (g *GStreamerBackend) StartRTP(ctx context.Context, f PCMFormat, p OpusParams) (RTPStream, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	if !g.Available() {
		return nil, ErrNoStreamingEncoder
	}
	// An unusable codec setting fails before anything is bound or spawned.
	if _, err := opusAudioType(p.Application); err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, fmt.Errorf("encode: bind local RTP socket: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	args, err := g.rtpPipelineArgs(f, p, port)
	if err != nil {
		conn.Close()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, g.bin(), args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("encode: encoder stdin: %w", err)
	}
	tail := &tailWriter{max: 512}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("encode: start streaming encoder: %w", err)
	}

	s := &gstRTPStream{
		cancel: cancel, conn: conn, stdin: stdin, tail: tail,
		in:   make(chan []byte, pcmQueueBlocks),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.pump()
	go func() {
		err := cmd.Wait()
		s.stopped(err)
	}()
	return s, nil
}

// pcmQueueBlocks bounds the PCM queued for the encoder. At the §6.7 10 ms frame
// this is a second of audio: enough to ride out a scheduling hiccup, small
// enough that a wedged encoder is reported as dropped frames within a second
// instead of silently growing a buffer of stale audio (§6.7 latency budget).
const pcmQueueBlocks = 100

// gstRTPStream is the GStreamer implementation of RTPStream.
type gstRTPStream struct {
	cancel context.CancelFunc
	conn   *net.UDPConn
	stdin  io.WriteCloser
	tail   *tailWriter

	in        chan []byte
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	framesIn      atomic.Uint64
	framesDropped atomic.Uint64
	packetsOut    atomic.Uint64

	// peer is the encoder's own UDP source address, latched from the first
	// packet. Anything arriving from elsewhere is another local process talking
	// to our socket and is discarded rather than forwarded to patrons.
	peerMu sync.Mutex
	peer   *net.UDPAddr

	errMu sync.Mutex
	err   error
}

// WritePCM converts one mapped block to S16LE and queues it for the encoder.
func (s *gstRTPStream) WritePCM(samples []float64) error {
	select {
	case <-s.done:
		return errStreamClosed
	default:
	}
	if len(samples) == 0 {
		return nil
	}
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		putS16LE(buf[i*2:], v)
	}
	select {
	case s.in <- buf:
		s.framesIn.Add(1)
		return nil
	case <-s.done:
		return errStreamClosed
	default:
		// The encoder is not draining. Dropping is the only non-blocking option
		// and it is COUNTED, never silent: the capture/qualify chain must not
		// stall on delivery (§6.1), and a patron gap must be visible.
		s.framesDropped.Add(1)
		return nil
	}
}

// pump feeds queued PCM into the encoder's stdin.
func (s *gstRTPStream) pump() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case b := <-s.in:
			if _, err := s.stdin.Write(b); err != nil {
				// The encoder is gone (EPIPE) or shutting down. Record it once;
				// the process reaper records the exit status right behind us.
				s.stopped(fmt.Errorf("encode: writing to the streaming encoder failed: %w", err))
				return
			}
		}
	}
}

// ReadRTP returns the next RTP packet the encoder produced.
func (s *gstRTPStream) ReadRTP(buf []byte) (int, error) {
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if e := s.Err(); e != nil {
				return 0, e
			}
			return 0, err
		}
		if !s.fromEncoder(addr) {
			continue
		}
		s.packetsOut.Add(1)
		return n, nil
	}
}

// fromEncoder latches the encoder's source address and rejects anything else.
func (s *gstRTPStream) fromEncoder(addr *net.UDPAddr) bool {
	if addr == nil || !addr.IP.IsLoopback() {
		return false
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if s.peer == nil {
		s.peer = addr
		return true
	}
	return s.peer.Port == addr.Port && s.peer.IP.Equal(addr.IP)
}

// Stats reports the encode's observed counts.
func (s *gstRTPStream) Stats() StreamStats {
	return StreamStats{
		FramesIn:      s.framesIn.Load(),
		FramesDropped: s.framesDropped.Load(),
		PacketsOut:    s.packetsOut.Load(),
	}
}

// Err reports why the encode stopped, or nil while it runs.
func (s *gstRTPStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// stopped records the first reason the encode ended and unblocks every reader.
func (s *gstRTPStream) stopped(cause error) {
	s.errMu.Lock()
	if s.err == nil {
		if cause == nil {
			cause = io.EOF
		}
		if t := s.tail.String(); t != "" {
			cause = fmt.Errorf("%w: %s", cause, t)
		}
		s.err = cause
	}
	s.errMu.Unlock()
	s.shutdown()
}

// Close stops the encoder process and releases the socket. It is idempotent.
func (s *gstRTPStream) Close() error {
	s.errMu.Lock()
	if s.err == nil {
		s.err = errStreamClosed
	}
	s.errMu.Unlock()
	s.shutdown()
	s.wg.Wait()
	return nil
}

// shutdown releases everything exactly once: the child process (so no encoder is
// orphaned when a patron's stream stops), the stdin pipe, and the UDP socket —
// closing the socket is what makes an in-flight ReadRTP return, so a departing
// subscriber cannot leave a reader goroutine parked forever.
func (s *gstRTPStream) shutdown() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		_ = s.stdin.Close()
		_ = s.conn.Close()
	})
}

// rtpPipelineArgs builds the gst-launch pipeline for a live encode. Every §6.7
// codec knob is passed explicitly, exactly as SmokeTest does; nothing is left at
// an element default while the docs claim it was tuned.
func (g *GStreamerBackend) rtpPipelineArgs(f PCMFormat, p OpusParams, port int) ([]string, error) {
	audioType, err := opusAudioType(p.Application)
	if err != nil {
		return nil, err
	}
	frameMS := maxInt(p.FrameMS, 10)
	// One read per Opus frame keeps the encoder's input granularity equal to its
	// frame size, so fdsrc adds no buffering of its own to the §6.7 budget.
	blockBytes := f.SampleRate * frameMS / 1000 * f.Channels * 2
	if blockBytes < 2 {
		blockBytes = 2
	}
	return []string{
		"-q",
		"fdsrc", "fd=0", fmt.Sprintf("blocksize=%d", blockBytes), "!",
		"rawaudioparse", "use-sink-caps=false", "format=pcm", "pcm-format=s16le",
		fmt.Sprintf("sample-rate=%d", f.SampleRate), fmt.Sprintf("num-channels=%d", f.Channels), "!",
		"audioconvert", "!",
		"opusenc", fmt.Sprintf("bitrate=%d", maxInt(p.Bitrate, 32000)),
		fmt.Sprintf("frame-size=%d", frameMS),
		fmt.Sprintf("audio-type=%s", audioType),
		fmt.Sprintf("dtx=%s", boolToTF(p.DTX)), fmt.Sprintf("inband-fec=%s", boolToTF(p.FEC)),
		// Timestamp each Opus frame from the sample count rather than from the
		// arrival time of the bytes that carried it. rtpopuspay derives the RTP
		// timestamp from the buffer, so without this a short read on the stdin
		// pipe becomes an irregular RTP clock — which is exactly the jitter the
		// phone-side buffer (§6.7's dominant latency term) has to absorb.
		"perfect-timestamp=true", "!",
		"rtpopuspay", "!",
		"udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", port), "sync=false", "async=false",
	}, nil
}

// StartRTP on the no-op backend starts NOTHING and says so, so a host with no
// encoder can never present itself as serving audio (kickoff: never fabricate).
func (NullBackend) StartRTP(context.Context, PCMFormat, OpusParams) (RTPStream, error) {
	return nil, ErrNoStreamingEncoder
}

// putS16LE writes one sample as little-endian signed 16-bit, clamping rather
// than wrapping: a wrapped sample turns an over-level input into loud noise in a
// patron's ear instead of the clipping the qualifier already reports (§6.3).
func putS16LE(dst []byte, v float64) {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	n := int32(v * 32767)
	dst[0] = byte(n)
	dst[1] = byte(n >> 8)
}

// tailWriter keeps the last max bytes written to it. The encoder's stderr is
// diagnostics about ELEMENTS, never audio, and it is bounded here so a failing
// pipeline cannot grow memory without limit.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
