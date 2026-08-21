package web

// Patron audio surface: the stream-availability model, the /listen-mode picker's
// server contract, and the WebRTC signaling relay (buildplan.md §6.7, §7.1, §7.2,
// §8.4).
//
// §7.1 makes audio a first-class patron experience ("HI only; VI-N only; captions
// + HI; captions + VI-N … selectable without reload"), and §7.2 requires a stream
// picker, volume, and the "add a little buffer if choppy" control. §8.4 scopes
// /api/audio/* to "authorized local audio streams only".
//
// WHAT IS REAL HERE AND WHAT IS STILL GATED. The availability model, the
// patron-safe projection, the picker's server contract and the signaling relay
// are real software and are tested, and the appliance-side WebRTC peer now
// exists: openaccessbridge-media answers offers with pion (DECISIONS D20), fed by a
// live Opus encode per named stream. What is NOT here is a real audio source or
// a real phone: on a host with no capture hardware the audio a patron receives
// is a generated tone (BLOCKER B3), which this surface says out loud, and the
// jitter buffer that dominates §6.7's latency budget cannot be measured without
// a phone fleet (BLOCKER B2).
//
// Availability is therefore MEASURED rather than assumed: a stream is offered
// only when the media process reports a live encoder for it, so the page never
// draws a player for audio that cannot start.
//
// NO DIAGNOSTIC LEAKAGE (§7.2). Nothing on this surface carries a device name, a
// dBFS figure, a qualifier reason string, an encoder state or a server address —
// only "available: yes/no", one calm sentence, and a plain statement when what
// is playing is a test tone. SDP bodies, ICE candidates and the per-connection
// peer id are relayed but never logged (§9.4).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/simpledcp/openaccessbridge/internal/ipc"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Patron-facing labels for the two named streams. Patrons are not expected to know
// the ISDCF abbreviations, so the abbreviation is a hint, not the name.
const (
	labelHI  = "Hearing assistance"
	labelVIN = "Audio description"
)

// PatronStream is the patron-safe projection of one named audio stream.
type PatronStream struct {
	Stream    model.StreamType `json:"stream"` // HI | VI-N
	Label     string           `json:"label"`
	Available bool             `json:"available"`
	// Reason is calm, non-technical patron copy (§7.2). Empty when available and
	// healthy; a caution when available but uneven.
	Reason string `json:"reason,omitempty"`
	// Caution marks a stream that is offered but degraded (QualifierState
	// Marginal). It is offered WITH the warning, never silently (guardrail #7).
	Caution bool `json:"caution,omitempty"`
	// TestTone marks a stream carrying a GENERATED TONE rather than this
	// auditorium's audio feed (BLOCKER B3: no capture hardware). Now that a
	// patron can really receive this stream, they have to be told what they are
	// hearing — a listener who is handed a sine wave and told nothing would
	// reasonably conclude the accessibility feed is broken. It is a statement
	// about the CONTENT, not a diagnostic: no device, level or reason travels
	// with it (§7.2).
	TestTone bool `json:"test_tone,omitempty"`
}

// PatronAudio is the whole /api/audio/streams document.
type PatronAudio struct {
	Streams []PatronStream `json:"streams"`
	// PlaybackAvailable reports whether this appliance can actually negotiate a
	// listening session right now. False with no signaling peer (B4/B2).
	PlaybackAvailable bool   `json:"playback_available"`
	PlaybackReason    string `json:"playback_reason,omitempty"`
}

// unavailableCopy is §7.2's "VI audio is not currently available…" shape, in words
// a patron understands.
func unavailableCopy(label string) string {
	return label + " audio is not currently available. Please contact theater staff."
}

// tooLowCopy is §7.2's "HI audio signal is too low…" state.
func tooLowCopy(label string) string {
	return label + " audio signal is too low. Please contact theater staff."
}

func streamLabel(t model.StreamType) string {
	if t == model.StreamVIN {
		return labelVIN
	}
	return labelHI
}

// projectStreams turns the media helper's per-stream measurements into the
// patron-safe availability model. It reads only structured fields — never the
// qualifier's staff-facing Reasons, which name devices, levels and remedies.
func projectStreams(states []model.AudioStreamState, granted func(model.StreamType) bool) []PatronStream {
	byType := make(map[model.StreamType]model.AudioStreamState, len(states))
	for _, st := range states {
		byType[st.StreamType] = st
	}
	order := []model.StreamType{model.StreamHI, model.StreamVIN}
	out := make([]PatronStream, 0, len(order))
	for _, t := range order {
		if granted != nil && !granted(t) {
			continue // this session was not granted this mode (§10.4)
		}
		ps := PatronStream{Stream: t, Label: streamLabel(t)}
		st, configured := byType[t]
		ps.TestTone = configured && model.SyntheticDevice(st.InputDeviceID)
		switch {
		case !configured:
			ps.Reason = unavailableCopy(ps.Label)
		case !st.DeviceConnected, !st.Present, st.Status == model.QualFailed:
			ps.Reason = unavailableCopy(ps.Label)
		case st.EncoderState != model.EncoderStateStreaming:
			// The input may be perfect, but nothing is encoding it, so nothing
			// can reach this patron. Offering the feed here would draw a player
			// that cannot start (§6.6 encoder state → §7.2 patron copy).
			ps.Reason = unavailableCopy(ps.Label)
		case st.Silence:
			// Sustained silence on a configured input is §7.2's "signal is too
			// low" state: the feed exists but there is nothing on it.
			ps.Reason = tooLowCopy(ps.Label)
		case st.Status == model.QualMarginal:
			ps.Available = true
			ps.Caution = true
			ps.Reason = "This feed may be uneven right now."
		default:
			ps.Available = true
		}
		out = append(out, ps)
	}
	return out
}

// audioSnapshot builds the whole patron audio document for one request/session.
func (s *Server) audioSnapshot(sess *model.PatronSession) PatronAudio {
	var states []model.AudioStreamState
	if s.deps.Audio != nil {
		states = s.deps.Audio()
	}
	var granted func(model.StreamType) bool
	if sess != nil {
		snapshot := *sess
		granted = func(t model.StreamType) bool {
			m, ok := model.ModeForStream(t)
			return ok && snapshot.Grants(m)
		}
	}
	doc := PatronAudio{Streams: projectStreams(states, granted)}
	// Playback is available when this appliance can REALLY serve a listening
	// session: there is a relay to the media process, and at least one stream
	// this session may pick is being encoded right now. Either half missing is
	// reported as unavailable with calm copy rather than a player that cannot
	// start (guardrail #7).
	if s.deps.Signaler == nil {
		doc.PlaybackReason = playbackUnavailableCopy
		return doc
	}
	for _, ps := range doc.Streams {
		if ps.Available {
			doc.PlaybackAvailable = true
			break
		}
	}
	if !doc.PlaybackAvailable {
		doc.PlaybackReason = playbackUnavailableCopy
	}
	return doc
}

// playbackUnavailableCopy is the whole-surface "you cannot listen here right
// now" state in §7.2's plain language. It names nothing about why.
const playbackUnavailableCopy = "Listening on your phone is not available in this auditorium right now. Please contact theater staff."

// handleAudioStreams serves the patron-safe availability document (§8.4
// "/api/audio/* authorized local audio streams only").
func (s *Server) handleAudioStreams(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authorize(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": patronAccessCopy(err)})
		return
	}
	writeJSON(w, s.audioSnapshot(sess))
}

// --- WebRTC signaling relay (§6.7) ---

// SignalFrame is one opaque signaling message between a patron browser and the
// appliance's media process. SDP and ICE bodies pass through this layer and are
// NEVER logged (§9.4 "must not record WebRTC SDP bodies or client identifiers").
type SignalFrame struct {
	Type          string           `json:"type"` // select | offer | answer | ice | close | unavailable
	Stream        model.StreamType `json:"stream,omitempty"`
	SDP           string           `json:"sdp,omitempty"`
	Candidate     string           `json:"candidate,omitempty"`
	SDPMid        string           `json:"sdp_mid,omitempty"`
	SDPMLineIndex int              `json:"sdp_m_line_index,omitempty"`
	// Reason is calm patron copy for an unavailable frame.
	Reason string `json:"reason,omitempty"`
}

// AudioSignaler relays WebRTC signaling to openaccessbridge-media. §6.7: "SDP
// offer/answer + ICE are exchanged over the existing local web server's WebSocket
// — no third-party signaling", and the local path is host-ICE-candidate only (no
// STUN/TURN), so an implementation must never reach the internet.
//
// peerID identifies ONE patron's control socket for the life of that connection.
// The media process holds one peer connection per id, which is what lets a
// browser's trickled candidates find their peer and — more importantly — what
// lets a patron who closed the page have their peer torn down instead of leaked.
// It is minted per connection here, is meaningless outside this appliance, and
// must never be logged: it is a client identifier (§9.4).
//
// The production implementation lives in cmd/openaccessbridge-core
// (signaler.go); tests inject a loopback one to exercise this relay without a
// media process.
type AudioSignaler interface {
	// Signal relays one frame from a patron and returns the frames to send back.
	// Implementations must not log SDP or candidate bodies.
	Signal(ctx context.Context, peerID string, in SignalFrame) ([]SignalFrame, error)
}

// newPeerID mints the opaque per-connection identifier described above.
func newPeerID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Without randomness two patrons could collide on one peer and take each
		// other's audio down. Falling back to a guessable id is worse than
		// having none: an empty id makes the media process refuse the offer.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// ErrPlaybackRefused is how an AudioSignaler reports that the media process
// declined to start a listening session, carrying the refusal's CLASS (one of
// ipc.Reason*). The class travels; the wording does not. §7.2's patron copy is
// composed here, in the one place that owns what a patron is shown, so a helper
// process can never put its own diagnostic sentence on a phone.
type ErrPlaybackRefused struct{ Class string }

func (e ErrPlaybackRefused) Error() string { return "playback refused: " + e.Class }

// refusalCopy turns a refusal class into calm patron copy (§7.2).
func refusalCopy(class string, label string) string {
	switch class {
	case ipc.ReasonAtCapacity:
		// The one refusal a patron can act on: it is temporary and not their
		// device's fault.
		return "Too many phones are listening in this auditorium right now. Please try again in a moment, or contact theater staff."
	case ipc.ReasonNoEncoder, ipc.ReasonUnknownStream:
		return unavailableCopy(label)
	default:
		return "Listening could not be started. Please contact theater staff."
	}
}

// audioPollInterval is how often a connected patron's availability view refreshes.
// Availability comes from the media helper's own polled status, so a tighter
// interval would add nothing.
const audioPollInterval = 2 * time.Second

// handleAudioWS is the patron audio control + signaling socket. It carries the
// availability model, the stream selection state machine, and the offer/answer
// relay. It is the only place a patron can reach the media process, and it can
// only reach it for a stream the session was granted.
func (s *Server) handleAudioWS(w http.ResponseWriter, r *http.Request) {
	sess, aerr := s.authorize(r)
	if aerr != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": patronAccessCopy(aerr)})
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx := r.Context()
	showEnded := s.sessions.ShowEnded()
	s.sessions.addAudioClient(1)
	defer s.sessions.addAudioClient(-1)

	// One peer per control socket, released when the socket goes (§6.7 "peers
	// joining and leaving mid-show"). Without this a patron who locks their
	// phone and walks out leaves a peer connection, an SRTP session and a track
	// binding behind for the rest of the show.
	peerID := newPeerID()
	defer s.releasePeer(peerID)

	send := func(v any) error {
		b, merr := json.Marshal(v)
		if merr != nil {
			return merr
		}
		return c.Write(ctx, websocket.MessageText, b)
	}

	snap := s.audioSnapshot(sess)
	if err := send(audioStateMsg(snap)); err != nil {
		return
	}

	// Inbound frames from the patron.
	in := make(chan SignalFrame, 4)
	go func() {
		defer close(in)
		for {
			_, data, rerr := c.Read(ctx)
			if rerr != nil {
				return
			}
			var f SignalFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			select {
			case in <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	tick := time.NewTicker(audioPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-showEnded:
			// §7.2 show-end behavior: terminate rather than dangle.
			_ = send(map[string]string{"type": msgShowEnded})
			_ = c.Close(websocket.StatusNormalClosure, "show ended")
			return
		case <-tick.C:
			next := s.audioSnapshot(sess)
			if audioChanged(snap, next) {
				snap = next
				if err := send(audioStateMsg(snap)); err != nil {
					return
				}
			}
		case f, ok := <-in:
			if !ok {
				return
			}
			for _, out := range s.handleSignalFrame(ctx, peerID, sess, f) {
				if err := send(out); err != nil {
					return
				}
			}
		}
	}
}

// releasePeer tells the media process this patron's peer is finished. It runs on
// a context of its own because the request context is already cancelled by the
// time a socket closes, and a cancelled relay would leave the peer alive.
func (s *Server) releasePeer(peerID string) {
	if s.deps.Signaler == nil || peerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerReleaseTimeout)
	defer cancel()
	_, _ = s.deps.Signaler.Signal(ctx, peerID, SignalFrame{Type: "close"})
}

// peerReleaseTimeout bounds the teardown relay. It is short: a media process
// that is not answering has already lost the peer we are trying to release.
const peerReleaseTimeout = 3 * time.Second

// handleSignalFrame runs the picker/signaling state machine for one inbound frame.
// Every refusal carries calm patron copy; none carries a diagnostic.
func (s *Server) handleSignalFrame(ctx context.Context, peerID string, sess *model.PatronSession, f SignalFrame) []SignalFrame {
	switch f.Type {
	case "select", "offer":
		snap := s.audioSnapshot(sess)
		if !streamOffered(snap, f.Stream) {
			return []SignalFrame{{
				Type:   "unavailable",
				Stream: f.Stream,
				Reason: reasonFor(snap, f.Stream),
			}}
		}
		if !snap.PlaybackAvailable || s.deps.Signaler == nil {
			return []SignalFrame{{
				Type:   "unavailable",
				Stream: f.Stream,
				Reason: snap.PlaybackReason,
			}}
		}
		if f.Type == "select" {
			return nil // availability confirmed; the browser sends its offer next
		}
	case "ice", "close":
		if s.deps.Signaler == nil {
			return nil
		}
	default:
		return nil
	}

	out, err := s.deps.Signaler.Signal(ctx, peerID, f)
	if err != nil {
		// No silent degradation: a relay failure is a loud (but calm-worded)
		// patron state, and the event is logged WITHOUT the SDP body (§9.4).
		var refused ErrPlaybackRefused
		class := "relay_failed"
		if errors.As(err, &refused) {
			class = refused.Class
		}
		s.deps.Log.Warn(logging.EvWebRTCPeerClosed,
			logging.Field("stream", string(f.Stream)),
			logging.Field("reason", class))
		return []SignalFrame{{
			Type:   "unavailable",
			Stream: f.Stream,
			Reason: refusalCopy(class, streamLabel(f.Stream)),
		}}
	}
	for _, o := range out {
		if o.Type == "answer" {
			// An answer was RELAYED. Whether a peer then connected is something
			// only the media process observes, and it logs
			// webrtc.peer.connected from the peer's own state change — this
			// relay must not claim a connection it cannot see (guardrail: never
			// report what was not measured).
			s.deps.Log.Info(logging.EvStateTransition,
				logging.Field("phase", "webrtc_answer_relayed"),
				logging.Field("stream", string(o.Stream)))
		}
	}
	return out
}

func streamOffered(a PatronAudio, t model.StreamType) bool {
	for _, st := range a.Streams {
		if st.Stream == t {
			return st.Available
		}
	}
	return false
}

func reasonFor(a PatronAudio, t model.StreamType) string {
	for _, st := range a.Streams {
		if st.Stream == t {
			if st.Reason != "" {
				return st.Reason
			}
		}
	}
	// Not in the offered set at all: either not a known stream or not granted.
	return unavailableCopy(streamLabel(t))
}

func audioChanged(a, b PatronAudio) bool {
	if a.PlaybackAvailable != b.PlaybackAvailable || a.PlaybackReason != b.PlaybackReason {
		return true
	}
	if len(a.Streams) != len(b.Streams) {
		return true
	}
	for i := range a.Streams {
		if a.Streams[i] != b.Streams[i] {
			return true
		}
	}
	return false
}

// audioStateMsg wraps a snapshot as a typed socket frame.
func audioStateMsg(a PatronAudio) map[string]any {
	return map[string]any{
		"type":               "audio_state",
		"streams":            a.Streams,
		"playback_available": a.PlaybackAvailable,
		"playback_reason":    a.PlaybackReason,
	}
}
