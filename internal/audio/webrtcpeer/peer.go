// Package webrtcpeer is the appliance-side WebRTC peer for captionbridge-media
// (buildplan.md §6.7, DECISIONS D20).
//
// TOPOLOGY, which §6.7 states as a requirement rather than an optimization:
// "One capture/analysis/Opus source per named stream (HI, VI-N), distributed to
// one WebRTC peer connection per browser subscriber — not literal multicast …
// The design must avoid re-running capture/analysis per client." So a Hub holds
// at most ONE source per named stream — one encode.RTPStream, one reader
// goroutine, one TrackLocalStaticRTP — and every subscriber's PeerConnection
// binds to that same track. Adding a subscriber adds an SRTP session and
// nothing else: no second capture, no second qualifier, no second encoder.
//
// LOCAL ONLY (§6.7, §9.4). Peers are configured with an EMPTY ICEServer list:
// host candidates only, no STUN and no TURN, because the phone and the
// appliance are one hop apart on the appliance's own patron LAN. There is no
// configuration knob here that could reach the internet. pion's default mDNS
// mode (query-only) is stated explicitly below because §6.7 calls the browser's
// `*.local` candidates out by name: the appliance must RESOLVE them, and must
// not advertise mDNS candidates of its own.
//
// NO SDP, NO CANDIDATES, NO CLIENT IDS IN LOGS (§9.4). This package logs peer
// state transitions and counts, keyed by named stream. The peer id — which is a
// client identifier — never reaches the logger, and neither does an SDP body.
package webrtcpeer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Errors a caller turns into calm patron copy. They are error CLASSES, never
// strings shown to a patron (§7.2).
var (
	// ErrNoSource: nothing is encoding this named stream, so there is nothing to
	// subscribe to. Never answered with an empty peer that plays silence.
	ErrNoSource = errors.New("webrtcpeer: no encoder is running for this stream")
	// ErrAtCapacity: the deployment profile's concurrent-client cap is reached
	// (D11, §9.5). Refusing is better than admitting a peer the RF cannot carry.
	ErrAtCapacity = errors.New("webrtcpeer: this auditorium is at its subscriber cap")
	// ErrUnknownPeer: an ICE candidate or close for a peer that does not exist.
	ErrUnknownPeer = errors.New("webrtcpeer: no such peer")
	// ErrClosed: the hub is shutting down.
	ErrClosed = errors.New("webrtcpeer: hub is closed")
)

// DefaultPortMin and DefaultPortMax bound patron media, and they are not a
// preference — they are one half of an agreement with the firewall.
//
// §9.1's ruleset opens `iif <client> udp dport 10000-20000 accept` on the patron
// interface. Until this was set, pion bound its ICE agent to whatever ephemeral
// ports the kernel handed out (typically 32768-60999), so on a real appliance
// with that ruleset loaded as root the phone's inbound connectivity checks
// arrived on ports the input chain drops. The rule protected a range nothing
// used, and patron audio depended on conntrack happening to have an entry from
// the appliance's own outbound check — which is timing, not design.
//
// These numbers must stay equal to firewall.DefaultWebRTCPortRange. An invariant
// test asserts it, because the two live in different packages and the failure is
// silent on this host (no root, so no ruleset is ever loaded — BLOCKER B5).
const (
	DefaultPortMin uint16 = 10000
	DefaultPortMax uint16 = 20000
)

// defaultGatherTimeout bounds host-candidate gathering. On the local-only path
// gathering is a synchronous enumeration of this host's interfaces and finishes
// in milliseconds; the bound exists so a wedged gatherer surfaces as a failed
// answer instead of holding a patron's request open.
const defaultGatherTimeout = 3 * time.Second

// Options configures a Hub.
type Options struct {
	// Log receives peer state transitions. Never SDP, candidates or peer ids.
	Log *logging.Logger
	// GatherTimeout bounds ICE gathering for one answer (0 = 3 s).
	GatherTimeout time.Duration
	// PortMin / PortMax bound the UDP ports patron media is carried on, and MUST
	// match the range §9.1's ruleset opens on the client interface. Zero uses
	// DefaultPortMin/Max, which are the same numbers the firewall defaults to.
	PortMin, PortMax uint16
	// Scope confines ICE gathering to the patron interface (scope.go). The zero
	// value gathers on every interface — the behaviour D27 recorded as a known
	// limit — and reports itself as unconfined rather than passing for scoped.
	Scope PatronScope
}

// Hub owns the appliance's WebRTC peers and the per-stream RTP sources they are
// fed from. It satisfies internal/audio/pipeline.DeliverySink, which is how a
// running stream publishes its encoder and reads back its listener count.
type Hub struct {
	log    *logging.Logger
	gather time.Duration
	api    *webrtc.API
	// scope is read by the gathering filter, which pion calls on a goroutine of
	// its own during an answer, so it is an atomic value rather than something
	// guarded by h.mu: core can push a new patron interface (media.network)
	// while an answer is being negotiated.
	scope   atomic.Pointer[PatronScope]
	mu      sync.Mutex
	sources map[model.StreamType]*source
	peers   map[string]*peer
	closed  bool
	// lastOffer is the MEASURED result of the most recent answer: which of this
	// appliance's interfaces a patron was really told to try. It is written from
	// the emitted SDP, never from the scope that was requested.
	lastOffer offerMeasurement
}

// offerMeasurement is what was read back out of the last answer's SDP.
type offerMeasurement struct {
	answered   bool
	interfaces []string
	candidates int
}

// source is one named stream's single Opus/RTP encode plus the track every
// subscriber to that stream is bound to.
type source struct {
	stream model.StreamType
	rtp    encode.RTPStream
	track  *webrtc.TrackLocalStaticRTP
	done   chan struct{}
	wg     sync.WaitGroup
}

// peer is one browser subscriber.
type peer struct {
	id     string
	stream model.StreamType
	pc     *webrtc.PeerConnection
	// connected is set from the peer-connection state callback, so Listeners
	// counts subscribers that are REALLY connected rather than ones that were
	// merely answered (§6.6 "connected listener count").
	connected bool
}

// New builds a Hub. The media engine registers exactly one codec: Opus at
// 48000/2. The channel count is an SDP convention, not the audio's channel
// count — §6.7's "mono per stream" is about the CONTENT. Declaring 1 here makes
// the browser's offered codec unmatched and the track never starts (D20).
func New(opts Options) (*Hub, error) {
	log := opts.Log
	if log == nil {
		log = logging.Nop()
	}
	gather := opts.GatherTimeout
	if gather <= 0 {
		gather = defaultGatherTimeout
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
			// §6.7's codec line, restated to the browser: 10 ms frames and
			// in-band FEC for Wi-Fi resilience. The encoder is configured with
			// the same numbers (internal/audio/encode).
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("webrtcpeer: register Opus: %w", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("webrtcpeer: interceptors: %w", err)
	}
	var se webrtc.SettingEngine
	// §6.7's mDNS gotcha, stated rather than inherited: the appliance RESOLVES
	// the browser's `*.local` host candidates (query) and publishes none of its
	// own (no gather), so a phone can always reach it by address on the patron
	// LAN.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)

	// §9.1: media must land inside the range the firewall opens. A failure here
	// is fatal to the hub rather than logged and continued — a peer that gathers
	// outside the opened range produces audio a patron cannot receive on any
	// appliance where the ruleset is actually loaded, and that is worse than no
	// peer at all, because the dashboard would report a stream as available.
	portMin, portMax := opts.PortMin, opts.PortMax
	if portMin == 0 || portMax == 0 {
		portMin, portMax = DefaultPortMin, DefaultPortMax
	}
	if err := se.SetEphemeralUDPPortRange(portMin, portMax); err != nil {
		return nil, fmt.Errorf("webrtcpeer: media port range %d-%d: %w", portMin, portMax, err)
	}

	h := &Hub{
		log:     log,
		gather:  gather,
		sources: map[model.StreamType]*source{},
		peers:   map[string]*peer{},
	}
	scope := opts.Scope
	h.scope.Store(&scope)
	// The filter closes over the HUB, not over the scope it was built with, so a
	// patron interface pushed down later (media.network) takes effect on the very
	// next answer without rebuilding the API — and so a scope can never be
	// reported as in force by one part of this process while gathering still uses
	// another (scope.go).
	se.SetInterfaceFilter(func(name string) bool { return h.patronScope().allowInterface(name) })

	h.api = webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	)
	return h, nil
}

// patronScope reads the scope currently in force.
func (h *Hub) patronScope() PatronScope {
	if s := h.scope.Load(); s != nil {
		return *s
	}
	return PatronScope{}
}

// SetPatronScope confines ICE gathering to one interface, or (with an empty
// scope) removes the confinement. It is how core pushes the §9.1 client
// interface down after the setup wizard records it.
//
// An unusable interface is REJECTED and the previous scope is left in force:
// silently accepting it would mean the next patron's answer had no host
// candidate at all, and the phone would spin on a network fault that was really
// a configuration fault. The error names which of the two it is.
func (h *Hub) SetPatronScope(s PatronScope) error {
	if err := s.usable(); err != nil {
		return err
	}
	// Logged only when the scope actually CHANGES. Core re-pushes the configured
	// interface on every posture pass, so an unconditional line here claimed a
	// state transition every fifteen seconds for the life of the appliance —
	// filling the log a review reads with events that did not happen.
	changed := h.patronScope().name() != s.name()
	h.scope.Store(&s)
	if changed {
		// The last answer described the OLD scope. Keeping it would leave the
		// dashboard telling an operator to fix something they just fixed, until
		// the next patron happens to join.
		h.mu.Lock()
		h.lastOffer = offerMeasurement{}
		h.mu.Unlock()

		h.log.Info(logging.EvStateTransition,
			logging.Field("phase", "webrtc_patron_scope"),
			logging.Field("interface", s.name()),
			logging.Field("confined", s.Configured()))
	}
	return nil
}

// ScopeReport is the dashboard's view of ICE scoping: what was configured, what
// is in force, and — the only measurement of the three — which interfaces the
// last patron was actually offered.
func (h *Hub) ScopeReport() model.ICEScopeReport {
	s := h.patronScope()
	rep := model.ICEScopeReport{Configured: s.Configured(), Interface: s.name()}
	if err := s.usable(); err != nil {
		rep.Unusable = err.Error()
	} else {
		rep.InForce = s.Configured()
	}
	h.mu.Lock()
	m := h.lastOffer
	h.mu.Unlock()
	rep.Answered = m.answered
	rep.OfferedInterfaces = append([]string(nil), m.interfaces...)
	rep.OfferedCandidates = m.candidates
	return rep
}

// recordOffer measures an emitted answer: how many host candidates it carries and
// which of this host's interfaces they belong to. An address that maps to no
// interface is recorded under the empty name rather than dropped, so a candidate
// this appliance cannot account for still shows up as one it offered.
func (h *Hub) recordOffer(sdp string) int {
	seen := map[string]bool{}
	var names []string
	addrs := candidateAddrs(sdp)
	// The interface table is read ONCE per answer rather than once per candidate.
	// A typical answer carries a dozen candidates and each lookup walked every
	// interface and every address on the host, which measured at several
	// milliseconds inside the patron's join path — the one place on this
	// appliance where milliseconds were the whole point (§11.4).
	byAddr := interfaceIndex()
	for _, ip := range addrs {
		name := byAddr[ip.String()]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	h.mu.Lock()
	h.lastOffer = offerMeasurement{answered: true, interfaces: names, candidates: len(addrs)}
	h.mu.Unlock()
	return len(addrs)
}

// Publish installs the single RTP source for a named stream and starts the one
// reader that fans its packets out to every subscriber. Publishing over an
// existing source replaces it and drops that stream's subscribers, because the
// audio they were listening to no longer exists.
func (h *Hub) Publish(stream model.StreamType, rtp encode.RTPStream) error {
	if rtp == nil {
		return ErrNoSource
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}, "audio", "captionbridge-"+string(stream))
	if err != nil {
		return fmt.Errorf("webrtcpeer: track for %s: %w", stream, err)
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	old := h.sources[stream]
	src := &source{stream: stream, rtp: rtp, track: track, done: make(chan struct{})}
	h.sources[stream] = src
	dropped := h.takePeersOfLocked(stream)
	h.mu.Unlock()

	stopSource(old)
	closePeers(dropped)
	src.wg.Add(1)
	go h.fanOut(src)
	h.log.Info(logging.EvStateTransition,
		logging.Field("phase", "webrtc_source_published"),
		logging.Field("stream", string(stream)))
	return nil
}

// Unpublish stops a named stream's source and drops its subscribers. A patron
// whose feed stopped is disconnected rather than left holding a peer that will
// never carry audio again (guardrail #7: no silent degradation).
func (h *Hub) Unpublish(stream model.StreamType) {
	h.mu.Lock()
	src := h.sources[stream]
	delete(h.sources, stream)
	dropped := h.takePeersOfLocked(stream)
	h.mu.Unlock()

	stopSource(src)
	closePeers(dropped)
	if src != nil {
		h.log.Info(logging.EvStateTransition,
			logging.Field("phase", "webrtc_source_unpublished"),
			logging.Field("stream", string(stream)),
			logging.Field("subscribers_dropped", len(dropped)))
	}
}

// Listeners reports how many subscribers of a named stream are CONNECTED
// (§6.6 "connected listener count"). A peer that has been answered but whose
// ICE has not completed is not counted: it is not listening yet.
func (h *Hub) Listeners(stream model.StreamType) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.peers {
		if p.stream == stream && p.connected {
			n++
		}
	}
	return n
}

// Publishing reports whether a named stream currently has a source.
func (h *Hub) Publishing(stream model.StreamType) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sources[stream]
	return ok
}

// Answer completes the browser-offers/appliance-answers exchange §6.7 specifies
// and assets/patron.js already implements. maxPeers is the deployment profile's
// concurrent-client cap (D11); 0 means unlimited.
//
// The answer is returned with ICE gathering COMPLETE, so the whole exchange is
// one request and one reply. That suits the local path exactly: with no STUN or
// TURN there is nothing to wait on but this host's own interfaces, and the
// signaling seam between core and media is a request/response socket rather
// than a push channel. The browser still trickles ITS candidates in, and
// AddCandidate takes them.
func (h *Hub) Answer(ctx context.Context, peerID string, stream model.StreamType, offerSDP string, maxPeers int) (string, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return "", ErrClosed
	}
	src := h.sources[stream]
	h.mu.Unlock()
	if src == nil {
		return "", ErrNoSource
	}
	// Checked per answer, not only when the scope was set: the patron interface
	// is a physical thing that can be unplugged between one patron joining and
	// the next. Gathering with the filter on a missing interface yields an answer
	// with no host candidate, which a phone experiences as an unexplained
	// failure to connect — so the fault is named here instead (scope.go).
	if err := h.patronScope().usable(); err != nil {
		return "", err
	}

	pc, err := h.api.NewPeerConnection(webrtc.Configuration{
		// §6.7: "no internet STUN/TURN is expected or permitted in the
		// production local-only path." An empty list is that rule in code.
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return "", fmt.Errorf("webrtcpeer: new peer: %w", err)
	}
	p := &peer{id: peerID, stream: stream, pc: pc}

	sender, err := pc.AddTrack(src.track)
	if err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("webrtcpeer: add track: %w", err)
	}
	// Drain this sender's RTCP. Without a reader the receiver's reports pile up
	// in the interceptor's buffer; the reader ends by itself when the peer
	// closes, so it cannot outlive its subscriber.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, rerr := sender.Read(buf); rerr != nil {
				return
			}
		}
	}()
	// The callback closes over THIS peer, never over its id. pion dispatches
	// state changes on a goroutine of its own, so a peer that has already been
	// replaced — a patron switching HI to VI-N re-offers on the same control
	// socket, and so under the same peer id — can deliver its final "closed"
	// AFTER its replacement is registered. Keyed by id, that stale callback
	// evicted and closed the peer the patron was actually listening to, taking
	// their audio down mid-show. Keyed by identity, it can only reach its own.
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { h.onPeerState(p, s) })

	// Admit the peer BEFORE negotiating, under one lock with the capacity check,
	// so D11's concurrent-client cap counts peers that are mid-negotiation as
	// well as peers already answered: two offers arriving together must not both
	// see the last free slot (§9.5's per-profile client target). Admission also
	// replaces whatever peer this id already held — a browser that reloaded or
	// switched feeds must not leave an orphan peer and SRTP session behind it.
	h.mu.Lock()
	if h.closed || h.sources[stream] != src {
		h.mu.Unlock()
		_ = pc.Close()
		return "", ErrNoSource
	}
	prior := h.takePeerLocked(peerID)
	if maxPeers > 0 && len(h.peers) >= maxPeers {
		h.mu.Unlock()
		_ = pc.Close()
		closePeers([]*peer{prior})
		return "", ErrAtCapacity
	}
	h.peers[peerID] = p
	h.mu.Unlock()
	if prior != nil {
		closePeers([]*peer{prior})
		h.log.Info(logging.EvWebRTCPeerReconnect, logging.Field("stream", string(stream)))
	}

	// The peer is in the map from here on, so every failure path has to take it
	// back out: a peer left behind for an offer that never got an answer would
	// hold a slot against the cap for the rest of the show.
	fail := func(err error) (string, error) {
		h.removePeer(p)
		_ = pc.Close()
		return "", err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return fail(fmt.Errorf("webrtcpeer: offer rejected: %w", err))
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(fmt.Errorf("webrtcpeer: create answer: %w", err))
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(fmt.Errorf("webrtcpeer: set local description: %w", err))
	}
	gctx, cancel := context.WithTimeout(ctx, h.gather)
	defer cancel()
	select {
	case <-gathered:
	case <-gctx.Done():
		return fail(fmt.Errorf("webrtcpeer: host candidate gathering did not complete: %w", gctx.Err()))
	}
	local := pc.LocalDescription()
	if local == nil {
		return fail(errors.New("webrtcpeer: no local description after gathering"))
	}

	h.mu.Lock()
	stillOurs := !h.closed && h.sources[stream] == src && h.peers[peerID] == p
	h.mu.Unlock()
	if !stillOurs {
		// The source went away while we were negotiating, or a later offer from
		// this patron replaced us: do not hand back an answer for a peer bound to
		// a track that no longer produces audio.
		return fail(ErrNoSource)
	}
	// Measured from the SDP that is about to leave this process — the only
	// artifact that says what the phone was really told to try.
	offered := h.recordOffer(local.SDP)
	if offered == 0 {
		// Gathering produced nothing. The pre-check passed, so the interface was
		// usable a moment ago; it can still happen (a NIC bounced mid-gather, or
		// held only addresses that cannot become host candidates). An answer with
		// no host candidate is indistinguishable from a dead network at the
		// phone's end: it spins until ICE gives up and nobody is told why. Name
		// the fault instead — the same refusal the pre-check would have given.
		return fail(fmt.Errorf("%w: gathering on %q produced no host candidate",
			ErrPatronInterfaceUnusable, h.patronScope().name()))
	}
	return local.SDP, nil
}

// removePeer takes one peer out of the map BY IDENTITY, leaving a different peer
// that has since claimed the same id alone. Every removal goes through here or
// through takePeerLocked's callers for exactly that reason.
func (h *Hub) removePeer(p *peer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peers[p.id] != p {
		return false
	}
	delete(h.peers, p.id)
	return true
}

// AddCandidate applies one trickled ICE candidate from a browser.
func (h *Hub) AddCandidate(peerID string, c webrtc.ICECandidateInit) error {
	h.mu.Lock()
	p := h.peers[peerID]
	h.mu.Unlock()
	if p == nil {
		return ErrUnknownPeer
	}
	return p.pc.AddICECandidate(c)
}

// ClosePeer drops one subscriber. §6.7's peers "join and leave mid-show", so
// this runs on every patron close and must leave nothing behind: the peer
// connection, its SRTP session, its RTCP reader and its track binding all go
// with it, while every other subscriber to the same source keeps playing.
func (h *Hub) ClosePeer(peerID string) {
	h.mu.Lock()
	p := h.takePeerLocked(peerID)
	var listeners int
	if p != nil {
		listeners = h.listenersLocked(p.stream)
	}
	h.mu.Unlock()
	if p == nil {
		return
	}
	closePeers([]*peer{p})
	// A patron leaving is a state transition with a count attached, and without
	// this line the log's §6.6 listener count only ever goes UP: an operator
	// reading it after a show would count everyone who ever connected as still
	// listening. The peer id is NOT logged — it is a client identifier (§9.4).
	h.log.Info(logging.EvWebRTCPeerClosed,
		logging.Field("stream", string(p.stream)),
		logging.Field("reason", "subscriber_left"),
		logging.Field("listeners", listeners))
}

// Peers reports the number of peer connections the hub is holding, connected or
// not. It is the leak check: a subscriber that left must not be in this count.
func (h *Hub) Peers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.peers)
}

// Close stops every source and drops every subscriber.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	srcs := make([]*source, 0, len(h.sources))
	for _, s := range h.sources {
		srcs = append(srcs, s)
	}
	h.sources = map[model.StreamType]*source{}
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.peers = map[string]*peer{}
	h.mu.Unlock()

	for _, s := range srcs {
		stopSource(s)
	}
	closePeers(peers)
}

// fanOut is the ONE reader per named stream. It copies each RTP packet the
// encoder produced onto the shared track, which pion re-stamps per binding
// (SSRC and payload type) and encrypts separately for each subscriber. A write
// error is per-subscriber — a phone that walked out of range must not stop the
// audio for everyone else — so it is counted, not fatal.
func (h *Hub) fanOut(src *source) {
	defer src.wg.Done()
	buf := make([]byte, 1500)
	writeErrs := 0
	for {
		n, err := src.rtp.ReadRTP(buf)
		if err != nil {
			select {
			case <-src.done:
				return // an ordinary stop
			default:
			}
			h.log.Warn(logging.EvWebRTCPeerClosed,
				logging.Field("stream", string(src.stream)),
				logging.Field("reason", "encoder_stopped"))
			return
		}
		if _, err := src.track.Write(buf[:n]); err != nil {
			writeErrs++
			if writeErrs%rtpWriteErrorLogEvery == 1 {
				h.log.Warn(logging.EvWebRTCPeerClosed,
					logging.Field("stream", string(src.stream)),
					logging.Field("reason", "subscriber_write_failed"),
					logging.Field("count", writeErrs))
			}
		}
	}
}

// rtpWriteErrorLogEvery rate-limits the per-subscriber write-failure log: at 100
// packets a second an unreachable phone would otherwise fill the log with one
// line per 10 ms while telling the operator nothing new.
const rtpWriteErrorLogEvery = 500

// onPeerState records connect/disconnect transitions and reaps a peer that the
// ICE agent gave up on, so a phone that left the auditorium does not stay in the
// listener count or hold a peer connection open.
//
// It takes the peer itself rather than its id: pion delivers these on its own
// goroutine, so a transition can land after the hub has already replaced or
// dropped this peer. `current` is the guard — a state change from a peer the hub
// no longer holds is history, and must not touch whichever peer holds its id now
// (see the callback registration in Answer).
func (h *Hub) onPeerState(p *peer, s webrtc.PeerConnectionState) {
	stream := p.stream
	switch s {
	case webrtc.PeerConnectionStateConnected:
		h.mu.Lock()
		current := h.peers[p.id] == p
		if current {
			p.connected = true
		}
		listeners := h.listenersLocked(stream)
		h.mu.Unlock()
		if !current {
			return
		}
		h.log.Info(logging.EvWebRTCPeerConnected,
			logging.Field("stream", string(stream)),
			logging.Field("listeners", listeners))
	case webrtc.PeerConnectionStateDisconnected:
		h.mu.Lock()
		current := h.peers[p.id] == p
		if current {
			p.connected = false
		}
		h.mu.Unlock()
		if !current {
			return
		}
		h.log.Warn(logging.EvWebRTCPeerReconnect,
			logging.Field("stream", string(stream)),
			logging.Field("reason", "ice_disconnected"))
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		if !h.removePeer(p) {
			// Already gone — a stale transition, or a peer the hub tore down
			// itself. Its connection is closed either way; there is nothing to
			// report and nothing to release.
			return
		}
		h.mu.Lock()
		listeners := h.listenersLocked(stream)
		h.mu.Unlock()
		go closePeers([]*peer{p})
		h.log.Info(logging.EvWebRTCPeerClosed,
			logging.Field("stream", string(stream)),
			logging.Field("listeners", listeners))
	}
}

func (h *Hub) listenersLocked(stream model.StreamType) int {
	n := 0
	for _, p := range h.peers {
		if p.stream == stream && p.connected {
			n++
		}
	}
	return n
}

// takePeerLocked removes a peer from the map and returns it. Closing happens
// outside the lock: pion's Close waits on its own goroutines, one of which is
// the state callback that wants this mutex.
func (h *Hub) takePeerLocked(peerID string) *peer {
	p := h.peers[peerID]
	if p != nil {
		delete(h.peers, peerID)
	}
	return p
}

func (h *Hub) takePeersOfLocked(stream model.StreamType) []*peer {
	var out []*peer
	for id, p := range h.peers {
		if p.stream == stream {
			out = append(out, p)
			delete(h.peers, id)
		}
	}
	return out
}

func closePeers(ps []*peer) {
	for _, p := range ps {
		if p != nil {
			_ = p.pc.Close()
		}
	}
}

// stopSource ends a source's reader and closes its encoder, in that order, so
// the reader sees an ordinary stop rather than logging an encoder failure.
func stopSource(s *source) {
	if s == nil {
		return
	}
	close(s.done)
	_ = s.rtp.Close()
	s.wg.Wait()
}
