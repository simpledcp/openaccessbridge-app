package encode

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// A live encode must produce RTP that a WebRTC stack can actually parse and
// forward — §6.7's "Opus, tuned for low latency" is only real if packets come
// out. This is the appliance half of the §6.7 path proven end to end at the
// encoder seam: real gst-launch, real Opus, real RTP, parsed by the same
// pion/rtp the peer connection uses.
func TestGStreamerStartRTPProducesParsableOpusRTP(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("no gst-launch/opusenc on this host: nothing to prove, and nothing is claimed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	p := DefaultOpusParams()
	s, err := g.StartRTP(ctx, PCMFormat{SampleRate: 48000, Channels: 1}, p)
	if err != nil {
		t.Fatalf("StartRTP: %v", err)
	}
	defer s.Close()

	// Feed a real-time 1 kHz tone in 10 ms blocks, exactly as the pipeline feeds
	// the mapped lane.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		const frame = 480
		phase, step := 0.0, 2*math.Pi*1000/48000
		block := make([]float64, frame)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				for i := range block {
					block[i] = 0.3 * math.Sin(phase)
					phase += step
				}
				if err := s.WritePCM(block); err != nil {
					return
				}
			}
		}
	}()

	var (
		buf      = make([]byte, 1500)
		pkts     []rtp.Packet
		deadline = time.After(10 * time.Second)
	)
	done := make(chan error, 1)
	go func() {
		for len(pkts) < 12 {
			n, rerr := s.ReadRTP(buf)
			if rerr != nil {
				done <- rerr
				return
			}
			var pkt rtp.Packet
			if uerr := pkt.Unmarshal(buf[:n]); uerr != nil {
				done <- uerr
				return
			}
			pkts = append(pkts, pkt)
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading RTP: %v", err)
		}
	case <-deadline:
		t.Fatalf("only %d RTP packets in 10s: the encoder is not streaming", len(pkts))
	}

	for i, pkt := range pkts {
		if len(pkt.Payload) == 0 {
			t.Fatalf("packet %d carries no Opus payload", i)
		}
		if i == 0 {
			continue
		}
		if pkt.SequenceNumber != pkts[i-1].SequenceNumber+1 {
			t.Fatalf("packet %d sequence %d does not follow %d", i, pkt.SequenceNumber, pkts[i-1].SequenceNumber)
		}
		if i == 1 {
			// The start-of-stream packet (marker bit) covers whatever partial
			// block the first read of the pipe returned, so the step INTO packet
			// 1 is short by design. Every step after it is a whole frame.
			continue
		}
		// §6.7's 10 ms frame at the 48 kHz clock is 480 ticks per packet. A
		// different step would mean the encoder is not running the frame size
		// the codec settings claim.
		if d := pkt.Timestamp - pkts[i-1].Timestamp; d != 480 {
			t.Fatalf("packet %d advanced the RTP clock by %d ticks, want 480 (a 10 ms Opus frame)", i, d)
		}
	}
	if !pkts[0].Marker {
		t.Fatal("the first packet does not carry the RTP marker bit that starts a talkspurt")
	}
	if st := s.Stats(); st.PacketsOut < uint64(len(pkts)) || st.FramesIn == 0 {
		t.Fatalf("stats %+v do not describe the encode that just ran", st)
	}
}

// Closing a stream must unblock its reader: a departing subscriber that left a
// goroutine parked in ReadRTP would leak one per patron over a show (§6.7 peers
// join and leave mid-show).
func TestCloseUnblocksReadRTP(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("no gst-launch/opusenc on this host")
	}
	s, err := g.StartRTP(context.Background(), PCMFormat{SampleRate: 48000, Channels: 1}, DefaultOpusParams())
	if err != nil {
		t.Fatalf("StartRTP: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 1500)
		_, rerr := s.ReadRTP(buf)
		errc <- rerr
	}()
	time.Sleep(50 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("ReadRTP returned no error after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadRTP still blocked 3s after Close: the reader goroutine leaked")
	}
	if err := s.WritePCM(make([]float64, 480)); !errors.Is(err, errStreamClosed) {
		t.Fatalf("WritePCM after Close = %v, want the closed error", err)
	}
}

// The no-op backend must start NOTHING rather than a stream that emits silence:
// a host with no encoder can never present itself as serving audio.
func TestNullBackendStartsNoStream(t *testing.T) {
	s, err := NullBackend{}.StartRTP(context.Background(), PCMFormat{SampleRate: 48000, Channels: 1}, DefaultOpusParams())
	if !errors.Is(err, ErrNoStreamingEncoder) {
		t.Fatalf("NullBackend.StartRTP = %v, %v; want ErrNoStreamingEncoder", s, err)
	}
	if s != nil {
		t.Fatal("NullBackend returned a stream; nothing can be encoded on it")
	}
}

// A format the encoder cannot honour is refused up front, not approximated.
func TestStartRTPRefusesAnImpossibleFormat(t *testing.T) {
	g := NewGStreamerBackend()
	for _, f := range []PCMFormat{{SampleRate: 0, Channels: 1}, {SampleRate: 48000, Channels: 0}, {SampleRate: 48000, Channels: 3}} {
		if _, err := g.StartRTP(context.Background(), f, DefaultOpusParams()); err == nil {
			t.Fatalf("StartRTP accepted %+v", f)
		}
	}
}

// An unrecognised §6.7 Opus application is an error, never a silent fallback to
// the element default — the same rule SmokeTest already follows.
func TestStartRTPRefusesAnUnknownOpusApplication(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("no gst-launch/opusenc on this host")
	}
	p := DefaultOpusParams()
	p.Application = "loudest"
	if _, err := g.StartRTP(context.Background(), PCMFormat{SampleRate: 48000, Channels: 1}, p); err == nil {
		t.Fatal("StartRTP accepted an unknown Opus application")
	}
}
