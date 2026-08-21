package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/encode"
	"github.com/simpledcp/openaccessbridge/internal/ipc"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// syncBuffer is a concurrency-safe log sink: the IPC server handles each
// connection on its own goroutine, and the peer hub logs from pion's.
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

func newLoggedManager(t *testing.T, logs *syncBuffer) *manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := logging.New(logging.Options{Component: "media-test", JSON: true, Writer: logs})
	m := newManagerFor(ctx, log, encode.Select(), "", true)
	t.Cleanup(func() {
		m.stopAll()
		cancel()
	})
	return m
}

func relaySignal(t *testing.T, m *manager, req ipc.SignalRequest) ipc.SignalResponse {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := m.handle(ipc.Message{Type: ipc.TypeMediaSignal, ID: "sig", Payload: payload})
	if msg == nil {
		t.Fatal("the media process did not answer a signaling frame")
	}
	var resp ipc.SignalResponse
	if err := json.Unmarshal(msg.Payload, &resp); err != nil {
		t.Fatalf("malformed signaling reply: %v", err)
	}
	return resp
}

// §9.4: "must not record WebRTC SDP bodies or client identifiers." The relay
// carries both and must observe neither. This is the fabrication-adjacent risk
// on this path: an SDP body names the patron's device addresses and DTLS
// fingerprint, and a peer id identifies one phone across a show.
func TestSignalingNeverLogsSDPOrPeerIdentity(t *testing.T) {
	logs := &syncBuffer{}
	m := newLoggedManager(t, logs)
	const (
		secretSDP  = "v=0\r\no=- 1 1 IN IP4 10.9.9.9\r\na=fingerprint:sha-256 AA:BB:CC\r\n"
		secretPeer = "peer-identity-e2f9c1"
		secretICE  = "candidate:9 1 UDP 1 10.9.9.9 40404 typ host"
	)
	// An offer for a stream with no encoder is refused; the refusal path is the
	// one that logs, so it is the strictest place to check.
	relaySignal(t, m, ipc.SignalRequest{
		PeerID: secretPeer, Type: ipc.SignalOffer, Stream: model.StreamHI, SDP: secretSDP,
	})
	relaySignal(t, m, ipc.SignalRequest{
		PeerID: secretPeer, Type: ipc.SignalICE, Stream: model.StreamHI, Candidate: secretICE,
	})
	relaySignal(t, m, ipc.SignalRequest{PeerID: secretPeer, Type: ipc.SignalClose, Stream: model.StreamHI})

	out := logs.String()
	for _, forbidden := range []string{secretPeer, "fingerprint", "10.9.9.9", "candidate:", "v=0"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("the signaling relay logged %q (§9.4):\n%s", forbidden, out)
		}
	}
}

// An offer for a stream nothing is encoding must come back as a named class, so
// the patron surface can say something calm and true. Answering it would build a
// peer that plays silence.
func TestOfferForAnUnencodedStreamIsRefusedWithAClass(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	resp := relaySignal(t, m, ipc.SignalRequest{
		PeerID: "p1", Type: ipc.SignalOffer, Stream: model.StreamVIN, SDP: "v=0\r\n",
	})
	if resp.Type != ipc.SignalUnavailable {
		t.Fatalf("offer for an unencoded stream = %+v, want unavailable", resp)
	}
	if resp.Reason != ipc.ReasonNoEncoder {
		t.Fatalf("refusal class = %q, want %q", resp.Reason, ipc.ReasonNoEncoder)
	}
	if resp.SDP != "" {
		t.Fatal("a refusal carried an answer")
	}
}

// A stream name this appliance does not serve is its own class, not a generic
// failure — the two lead to different patron copy.
func TestOfferForAnUnknownStreamIsNamedAsSuch(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	resp := relaySignal(t, m, ipc.SignalRequest{
		PeerID: "p1", Type: ipc.SignalOffer, Stream: model.StreamType("KARAOKE"), SDP: "v=0\r\n",
	})
	if resp.Reason != ipc.ReasonUnknownStream {
		t.Fatalf("unknown stream refusal class = %q", resp.Reason)
	}
}

// A malformed relay payload must not take the media process down or be answered
// as if it were a valid offer.
func TestMalformedSignalingPayloadIsRefused(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	msg := m.handle(ipc.Message{Type: ipc.TypeMediaSignal, ID: "sig", Payload: []byte(`{"type":`)})
	if msg == nil {
		t.Fatal("no reply to a malformed signaling frame")
	}
	var resp ipc.SignalResponse
	if err := json.Unmarshal(msg.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != ipc.SignalUnavailable {
		t.Fatalf("malformed payload = %+v, want unavailable", resp)
	}
}

// With gst-launch present the synthetic streams must reach the measured
// "streaming" state — which is what flips the patron surface to available. On a
// host without it they must reach "unavailable", never a state that would offer
// a patron audio nothing is encoding.
func TestSyntheticStreamsReachAMeasuredEncoderState(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	m.applyDefaultSynthetic()

	want := model.EncoderStateUnavailable
	if encode.NewGStreamerBackend().Available() {
		want = model.EncoderStateStreaming
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		states := m.states()
		if len(states) == 2 && states[0].EncoderState == want && states[1].EncoderState == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("streams never reached encoder state %q: %+v", want, m.states())
}

// The stand-in source now feeds a LIVE encode a patron listens to, so it must
// run at 1x. capture.SyntheticTone generates as fast as its consumer reads
// unless it is paced, and an unpaced generator would hand the Opus encoder hours
// of audio in minutes: the phone would play a chipmunk tone, the queue in
// internal/audio/encode would overflow and count dropped blocks that never
// happened at the input, and every wall-clock figure measured around this stream
// would describe the generator rather than a booth feed (B3).
//
// capture.TestRealtimeToneIsPacedAtOneTimesSpeed proves Realtime means 1x; this
// asserts the media process actually asks for it, which is the half that was
// only a comment.
func TestTheStandInSourceFeedingALiveEncodeIsPaced(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	m.applyDefaultSynthetic()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.slots) == 0 {
		t.Fatal("no streams were configured")
	}
	for name, s := range m.slots {
		if s.src == nil {
			t.Fatalf("%s has no capture source running", name)
		}
		tone, ok := s.src.(*capture.SyntheticTone)
		if !ok {
			// A real capture device paces itself; only the stand-in can run away.
			continue
		}
		if !tone.Realtime {
			t.Fatalf("%s feeds a live Opus encode from an UNPACED generator: a patron would hear it at many times speed", name)
		}
	}
}
