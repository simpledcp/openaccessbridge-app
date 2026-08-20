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
	"sync"
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
}

// Hub owns the appliance's WebRTC peers and the per-stream RTP sources they are
// fed from. It satisfies internal/audio/pipeline.DeliverySink, which is how a
// running stream publishes its encoder and reads back its listener count.
type Hub struct {
	log     *logging.Logger
	gather  time.Duration
	api     *webrtc.API
	mu      sync.Mutex
	sources map[model.StreamType]*source
	peers   map[string]*peer
	closed  bool
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

	return &Hub{
		log:    log,
		gather: gather,
		api: webrtc.NewAPI(
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(ir),
			webrtc.WithSettingEngine(se),
		),
		sources: map[model.StreamType]*source{},
		peers:   map[string]*peer{},
	}, nil
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
	if src == nil {
		h.mu.Unlock()
		return "", ErrNoSource
	}
	// A re-offer from a peer id we already hold is a reconnect: the old peer is
	// torn down first so a browser that reloaded cannot leave an orphan peer
	// (and an orphan SRTP session) behind it.
	prior := h.takePeerLocked(peerID)
	if maxPeers > 0 && len(h.peers) >= maxPeers {
		h.mu.Unlock()
		closePeers([]*peer{prior})
		return "", ErrAtCapacity
	}
	h.mu.Unlock()
	if prior != nil {
		closePeers([]*peer{prior})
		h.log.Info(logging.EvWebRTCPeerReconnect, logging.Field("stream", string(stream)))
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
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { h.onPeerState(peerID, stream, s) })

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("webrtcpeer: offer rejected: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("webrtcpeer: create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("webrtcpeer: set local description: %w", err)
	}
	gctx, cancel := context.WithTimeout(ctx, h.gather)
	defer cancel()
	select {
	case <-gathered:
	case <-gctx.Done():
		_ = pc.Close()
		return "", fmt.Errorf("webrtcpeer: host candidate gathering did not complete: %w", gctx.Err())
	}
	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return "", errors.New("webrtcpeer: no local description after gathering")
	}

	h.mu.Lock()
	if h.closed || h.sources[stream] != src {
		// The source went away while we were negotiating: do not hand back an
		// answer for a peer bound to a track that no longer produces audio.
		h.mu.Unlock()
		_ = pc.Close()
		return "", ErrNoSource
	}
	h.peers[peerID] = p
	h.mu.Unlock()
	return local.SDP, nil
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
	h.mu.Unlock()
	closePeers([]*peer{p})
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
func (h *Hub) onPeerState(peerID string, stream model.StreamType, s webrtc.PeerConnectionState) {
	switch s {
	case webrtc.PeerConnectionStateConnected:
		h.mu.Lock()
		if p := h.peers[peerID]; p != nil {
			p.connected = true
		}
		listeners := h.listenersLocked(stream)
		h.mu.Unlock()
		h.log.Info(logging.EvWebRTCPeerConnected,
			logging.Field("stream", string(stream)),
			logging.Field("listeners", listeners))
	case webrtc.PeerConnectionStateDisconnected:
		h.mu.Lock()
		if p := h.peers[peerID]; p != nil {
			p.connected = false
		}
		h.mu.Unlock()
		h.log.Warn(logging.EvWebRTCPeerReconnect,
			logging.Field("stream", string(stream)),
			logging.Field("reason", "ice_disconnected"))
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		h.mu.Lock()
		p := h.takePeerLocked(peerID)
		listeners := h.listenersLocked(stream)
		h.mu.Unlock()
		if p == nil {
			return
		}
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
