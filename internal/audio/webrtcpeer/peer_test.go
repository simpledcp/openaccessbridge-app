package webrtcpeer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/openaccessbridge/internal/audio/encode"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// fakeEncoder stands in for internal/audio/encode's GStreamer stream so these
// tests exercise the TOPOLOGY (one source, N peers) without needing gst-launch.
// It counts reads, which is how the fan-out test proves the encoder is read
// once per packet rather than once per subscriber.
type fakeEncoder struct {
	reads  atomic.Uint64
	seq    uint16
	ts     uint32
	closed chan struct{}
	once   sync.Once
	tick   time.Duration
}

func newFakeEncoder() *fakeEncoder {
	return &fakeEncoder{closed: make(chan struct{}), tick: 10 * time.Millisecond}
}

func (f *fakeEncoder) WritePCM([]float64) error { return nil }

func (f *fakeEncoder) ReadRTP(buf []byte) (int, error) {
	select {
	case <-f.closed:
		return 0, errors.New("closed")
	case <-time.After(f.tick):
	}
	f.seq++
	f.ts += 480
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: f.seq, Timestamp: f.ts, SSRC: 0x1234},
		Payload: []byte{0xfc, 0x01, 0x02, 0x03, 0x04, 0x05},
	}
	b, err := pkt.Marshal()
	if err != nil {
		return 0, err
	}
	n := copy(buf, b)
	f.reads.Add(1)
	return n, nil
}

func (f *fakeEncoder) Stats() encode.StreamStats {
	return encode.StreamStats{PacketsOut: f.reads.Load()}
}

func (f *fakeEncoder) Err() error { return nil }

func (f *fakeEncoder) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// browser is the patron half: a receive-only peer that offers, exactly as
// internal/web/assets/patron.js does.
type browser struct {
	pc     *webrtc.PeerConnection
	got    atomic.Uint64
	closed chan struct{}
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}})
	if err != nil {
		t.Fatal(err)
	}
	b := &browser{pc: pc, closed: make(chan struct{})}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			b.got.Add(1)
		}
	})
	t.Cleanup(func() { _ = pc.Close() })
	return b
}

// connect runs the §6.7 exchange: the browser offers, the appliance answers,
// and the browser trickles its candidates in behind the answer.
func (b *browser) connect(t *testing.T, h *Hub, peerID string, stream model.StreamType, maxPeers int) error {
	t.Helper()
	b.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		// A candidate for a peer the hub has not registered yet is dropped by
		// design; ICE still completes from the answer's host candidates.
		_ = h.AddCandidate(peerID, c.ToJSON())
	})
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	answer, err := h.Answer(context.Background(), peerID, stream, offer.SDP, maxPeers)
	if err != nil {
		return err
	}
	return b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
}

func (b *browser) waitForAudio(t *testing.T, min uint64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b.got.Load() >= min {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("only %d RTP packets reached this subscriber in %s (want %d)", b.got.Load(), within, min)
}

func newHub(t *testing.T) *Hub {
	t.Helper()
	requireRoutableInterface(t)
	h, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// §6.7's topology, which the plan states as a requirement: ONE Opus source per
// named stream distributed to one peer connection per subscriber, and "the
// design must avoid re-running capture/analysis per client". A second
// subscriber must therefore add a peer connection and NOT a second encoder read
// stream.
func TestOneEncoderFansOutToEverySubscriber(t *testing.T) {
	h := newHub(t)
	enc := newFakeEncoder()
	if err := h.Publish(model.StreamHI, enc); err != nil {
		t.Fatal(err)
	}

	a, b := newBrowser(t), newBrowser(t)
	if err := a.connect(t, h, "peer-a", model.StreamHI, 0); err != nil {
		t.Fatalf("first subscriber: %v", err)
	}
	a.waitForAudio(t, 5, 10*time.Second)

	if err := b.connect(t, h, "peer-b", model.StreamHI, 0); err != nil {
		t.Fatalf("second subscriber: %v", err)
	}
	b.waitForAudio(t, 5, 10*time.Second)

	// Measure the encoder and one subscriber over the SAME window.
	reads0, aGot0, bGot0 := enc.reads.Load(), a.got.Load(), b.got.Load()
	time.Sleep(500 * time.Millisecond)
	reads, aGot, bGot := enc.reads.Load()-reads0, a.got.Load()-aGot0, b.got.Load()-bGot0

	// The first subscriber must not have been disturbed by the second joining.
	if aGot == 0 {
		t.Fatal("the first subscriber stopped receiving when a second one joined")
	}
	if bGot == 0 {
		t.Fatal("the second subscriber received nothing")
	}
	// The decisive check. With ONE source per named stream the encoder is read
	// once per packet however many subscribers there are, so reads ≈ each
	// subscriber's packet count. A per-subscriber encoder would read the source
	// about twice per delivered packet, which is the design defect §6.7 rules
	// out ("must avoid re-running capture/analysis per client").
	if reads > bGot*3/2 {
		t.Fatalf("%d encoder reads delivered %d packets to a subscriber: the per-stream source is being duplicated per client", reads, bGot)
	}
	if n := h.Listeners(model.StreamHI); n != 2 {
		t.Fatalf("connected listener count = %d, want 2 (§6.6)", n)
	}
}

// A subscriber leaving mid-show must take everything of its own with it and
// nothing of anyone else's (§6.7 "peers joining and leaving mid-show").
func TestDepartingSubscriberLeavesTheOthersPlaying(t *testing.T) {
	h := newHub(t)
	enc := newFakeEncoder()
	if err := h.Publish(model.StreamHI, enc); err != nil {
		t.Fatal(err)
	}
	stay, leave := newBrowser(t), newBrowser(t)
	if err := stay.connect(t, h, "stay", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	if err := leave.connect(t, h, "leave", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	stay.waitForAudio(t, 5, 10*time.Second)
	leave.waitForAudio(t, 5, 10*time.Second)

	h.ClosePeer("leave")
	if n := h.Peers(); n != 1 {
		t.Fatalf("hub still holds %d peers after one left", n)
	}
	before := stay.got.Load()
	time.Sleep(300 * time.Millisecond)
	if stay.got.Load() <= before {
		t.Fatal("the remaining subscriber stopped receiving when another left")
	}
	waitFor(t, 5*time.Second, func() bool { return h.Listeners(model.StreamHI) == 1 })
}

// Nothing encoding this stream means nothing to subscribe to. The hub must
// refuse rather than answer with a peer that will play silence (guardrail #7).
func TestAnswerWithoutASourceIsRefused(t *testing.T) {
	h := newHub(t)
	b := newBrowser(t)
	err := b.connect(t, h, "peer", model.StreamVIN, 0)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("offer with no encoder = %v, want ErrNoSource", err)
	}
	if h.Peers() != 0 {
		t.Fatal("a refused offer left a peer behind")
	}
}

// D11 / §9.5: the deployment profile's concurrent-client cap is enforced at
// peer admission, not just at the caption fan-out.
func TestSubscriberCapIsEnforced(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	first := newBrowser(t)
	if err := first.connect(t, h, "one", model.StreamHI, 1); err != nil {
		t.Fatal(err)
	}
	second := newBrowser(t)
	if err := second.connect(t, h, "two", model.StreamHI, 1); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("second subscriber past the cap = %v, want ErrAtCapacity", err)
	}
}

// §6.7: "no internet STUN/TURN is expected or permitted in the production
// local-only path." The answer the appliance hands a phone must therefore carry
// host candidates only — a reflexive or relayed candidate would mean this
// appliance had talked to something off the venue LAN.
func TestAnswerCarriesHostCandidatesOnly(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := h.Answer(context.Background(), "peer", model.StreamHI, offer.SDP, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "typ host") {
		t.Fatal("the answer carries no host candidate at all")
	}
	for _, forbidden := range []string{"typ srflx", "typ prflx", "typ relay", "stun:", "turn:"} {
		if strings.Contains(answer, forbidden) {
			t.Fatalf("the answer carries %q: the local-only path reached off-LAN", forbidden)
		}
	}
	// §6.7's codec line: Opus is signalled 48000/2 even for mono content.
	if !strings.Contains(answer, "opus/48000/2") {
		t.Fatalf("the answer does not offer opus/48000/2:\n%s", answer)
	}
}

// Unpublishing a stream (the input failed, or the room was reconfigured) must
// drop its subscribers rather than leave them holding a peer that will never
// carry audio again.
func TestUnpublishDropsThatStreamsSubscribers(t *testing.T) {
	h := newHub(t)
	enc := newFakeEncoder()
	if err := h.Publish(model.StreamHI, enc); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	if err := b.connect(t, h, "peer", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	b.waitForAudio(t, 3, 10*time.Second)
	h.Unpublish(model.StreamHI)
	if n := h.Peers(); n != 0 {
		t.Fatalf("%d peers survived their source being unpublished", n)
	}
	if h.Publishing(model.StreamHI) {
		t.Fatal("the stream still reports a source after Unpublish")
	}
	if enc.Err() == nil {
		// The fake never fails; what matters is that Close was called on it.
		select {
		case <-enc.closed:
		default:
			t.Fatal("the encoder was left running with no subscribers")
		}
	}
}

// requireRoutableInterface skips when this host has no non-loopback address.
// pion gathers host candidates on real interfaces only (as an appliance on a
// patron LAN should), so with nothing but `lo` there is no candidate for two
// local peers to meet on — and a skipped test is the honest outcome, not a
// weakened one.
func requireRoutableInterface(t *testing.T) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := i.Addrs()
		if aerr == nil && len(addrs) > 0 {
			return
		}
	}
	t.Skip("no non-loopback interface: two local WebRTC peers have no host candidate to meet on")
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached within " + within.String())
}

// §6.7's patrons "join and leave mid-show", and the one they do most is switch
// feeds: assets/patron.js re-offers on the SAME control socket, so under the
// same peer id. The peer they switched away from is closed, and pion delivers
// its final state change on a goroutine of its own — so it can land after the
// replacement is already registered.
//
// Keyed by peer id, that stale transition took the peer the patron was actually
// listening to out of the hub and closed it: their audio stopped for the rest of
// the show, having done nothing but change feeds. This asserts the transition is
// keyed by peer IDENTITY, so history cannot reach the present.
func TestAStaleTransitionCannotTouchTheCurrentPeer(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	if err := h.Publish(model.StreamVIN, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	onHI := newBrowser(t)
	if err := onHI.connect(t, h, "one-socket", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	onHI.waitForAudio(t, 3, 10*time.Second)
	h.mu.Lock()
	stale := h.peers["one-socket"]
	h.mu.Unlock()
	if stale == nil {
		t.Fatal("the first peer was never registered")
	}

	// The patron switches HI -> VI-N on the same socket.
	onVIN := newBrowser(t)
	if err := onVIN.connect(t, h, "one-socket", model.StreamVIN, 0); err != nil {
		t.Fatal(err)
	}
	onVIN.waitForAudio(t, 3, 10*time.Second)
	waitFor(t, 5*time.Second, func() bool { return h.Listeners(model.StreamVIN) == 1 })
	if n := h.Peers(); n != 1 {
		t.Fatalf("switching feeds left %d peers, want 1: the peer switched away from was not released", n)
	}

	// Now the transitions the replaced peer still owed us, arriving late.
	h.onPeerState(stale, webrtc.PeerConnectionStateDisconnected)
	h.onPeerState(stale, webrtc.PeerConnectionStateClosed)

	if n := h.Peers(); n != 1 {
		t.Fatalf("a stale transition evicted the live peer: %d peers remain", n)
	}
	if n := h.Listeners(model.StreamVIN); n != 1 {
		t.Fatalf("§6.6 listener count = %d after a stale transition, want 1", n)
	}
	before := onVIN.got.Load()
	time.Sleep(300 * time.Millisecond)
	if onVIN.got.Load() <= before {
		t.Fatal("the patron's audio stopped after a stale transition from the feed they switched away from")
	}
}

// A patron who reloads reaches the hub as a fresh offer. Whether it arrives on a
// new control socket (new peer id) or the same one, the peer that came before it
// must be gone — a peer connection and its SRTP session per reload would
// accumulate for the length of a show.
func TestReofferReleasesThePeerItReplaces(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	var replaced []*peer
	for i := 0; i < 4; i++ {
		b := newBrowser(t)
		if err := b.connect(t, h, "one-socket", model.StreamHI, 0); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		b.waitForAudio(t, 3, 10*time.Second)
		if n := h.Peers(); n != 1 {
			t.Fatalf("after %d reloads the hub holds %d peers, want 1", i+1, n)
		}
		h.mu.Lock()
		replaced = append(replaced, h.peers["one-socket"])
		h.mu.Unlock()
	}
	waitFor(t, 5*time.Second, func() bool { return h.Listeners(model.StreamHI) == 1 })

	// Dropping the old peer from the map is not releasing it. Each superseded
	// peer connection — and the SRTP session, DTLS transport and ICE agent under
	// it — must actually be CLOSED, or a patron whose phone reconnects through a
	// show leaves one running per reload.
	for i, p := range replaced[:len(replaced)-1] {
		waitFor(t, 5*time.Second, func() bool {
			return p.pc.ConnectionState() == webrtc.PeerConnectionStateClosed
		})
		if p.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			t.Fatalf("the peer replaced by reload %d is still open: it was dropped from the hub but never closed", i+1)
		}
	}
}

// D11 / §9.5 again, this time under the arrival pattern a full auditorium
// actually produces: everyone scans the QR at once. The cap has to count peers
// that are mid-negotiation too, or N offers racing each other all see the same
// free slot and the appliance admits more phones than the profile allows.
func TestSubscriberCapHoldsUnderConcurrentOffers(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	const cap = 2
	const offers = 6
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for i := 0; i < offers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := newBrowser(t)
			if err := b.connect(t, h, fmt.Sprintf("peer-%d", i), model.StreamHI, cap); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrAtCapacity) {
				t.Errorf("offer %d failed for a reason other than the cap: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := admitted.Load(); n > cap {
		t.Fatalf("%d of %d concurrent offers were admitted against a cap of %d", n, offers, cap)
	}
	if n := h.Peers(); n > cap {
		t.Fatalf("the hub holds %d peers against a cap of %d", n, cap)
	}
}

// §6.6 asks for the connected listener count and §9.4 says to log state
// transitions and counts. A departure that logs nothing leaves an operator's
// only record of the count monotonically increasing — every phone that ever
// connected still apparently listening. This asserts the count comes back down
// in the log, and that the departure carries no client identifier with it.
func TestADepartingSubscriberIsLoggedWithTheNewCount(t *testing.T) {
	logs := &syncBuffer{}
	h := newHubLogging(t, logs)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	stay, leave := newBrowser(t), newBrowser(t)
	if err := stay.connect(t, h, "stays", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	if err := leave.connect(t, h, "leaves", model.StreamHI, 0); err != nil {
		t.Fatal(err)
	}
	stay.waitForAudio(t, 3, 10*time.Second)
	leave.waitForAudio(t, 3, 10*time.Second)
	waitFor(t, 5*time.Second, func() bool { return h.Listeners(model.StreamHI) == 2 })

	h.ClosePeer("leaves")
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), string(logging.EvWebRTCPeerClosed))
	})
	line := lastEventLine(t, logs.String(), string(logging.EvWebRTCPeerClosed))
	if !strings.Contains(line, `"listeners":1`) {
		t.Fatalf("a subscriber left and the log did not record the count coming down:\n%s", line)
	}
	// §9.4: the peer id is a client identifier and must not travel to the log.
	for _, id := range []string{"leaves", "stays"} {
		if strings.Contains(logs.String(), id) {
			t.Fatalf("the peer id %q reached the log:\n%s", id, logs.String())
		}
	}
}

func newHubLogging(t *testing.T, logs *syncBuffer) *Hub {
	t.Helper()
	requireRoutableInterface(t)
	h, err := New(Options{Log: logging.New(logging.Options{Component: "test", JSON: true, Writer: logs})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// syncBuffer is a concurrency-safe log sink: pion delivers state changes from
// its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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

func lastEventLine(t *testing.T, logs, event string) string {
	t.Helper()
	found := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"event":"`+event+`"`) {
			found = line
		}
	}
	if found == "" {
		t.Fatalf("no %s event in:\n%s", event, logs)
	}
	return found
}
