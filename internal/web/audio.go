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
// WHAT IS REAL HERE AND WHAT IS A SEAM. The availability model, the patron-safe
// projection, the picker's server contract and the signaling relay are real
// software and are tested. The appliance-side WebRTC PEER is not: it is GStreamer
// webrtcbin, whose development libraries are absent on this host (BLOCKER B4), and
// there are no real phones to negotiate with (BLOCKER B2). So Deps.Signaler is nil
// in the shipped binary, and when it is nil this layer tells the patron the audio
// feed is unavailable in plain language. It never renders a player that cannot
// play, and it never reports a peer that was not established.
//
// NO DIAGNOSTIC LEAKAGE (§7.2). Nothing on this surface carries a device name, a
// dBFS figure, a qualifier reason string, an encoder state or a server address —
// only "available: yes/no" and one calm sentence. SDP bodies and ICE candidates
// are relayed but never logged (§9.4).

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
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
		switch {
		case !configured:
			ps.Reason = unavailableCopy(ps.Label)
		case !st.DeviceConnected, !st.Present, st.Status == model.QualFailed:
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
	if s.deps.Signaler == nil {
		// SEAM (B4/B2): no webrtcbin peer exists, so there is nothing to listen
		// to. Say so plainly rather than showing a player that cannot start.
		doc.PlaybackReason = "Listening on your phone is not available in this auditorium right now. Please contact theater staff."
		return doc
	}
	doc.PlaybackAvailable = true
	return doc
}

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

// AudioSignaler relays WebRTC signaling to captionbridge-media. §6.7: "SDP
// offer/answer + ICE are exchanged over the existing local web server's WebSocket
// — no third-party signaling", and the local path is host-ICE-candidate only (no
// STUN/TURN), so an implementation must never reach the internet.
//
// SEAM: no production implementation exists. The appliance-side peer is webrtcbin
// (BLOCKER B4) and there are no real phones to negotiate with (BLOCKER B2). Tests
// inject a loopback implementation to exercise this relay end to end.
type AudioSignaler interface {
	// Signal relays one frame from a patron and returns the frames to send back.
	// Implementations must not log SDP or candidate bodies.
	Signal(ctx context.Context, in SignalFrame) ([]SignalFrame, error)
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
			for _, out := range s.handleSignalFrame(ctx, sess, f) {
				if err := send(out); err != nil {
					return
				}
			}
		}
	}
}

// handleSignalFrame runs the picker/signaling state machine for one inbound frame.
// Every refusal carries calm patron copy; none carries a diagnostic.
func (s *Server) handleSignalFrame(ctx context.Context, sess *model.PatronSession, f SignalFrame) []SignalFrame {
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

	out, err := s.deps.Signaler.Signal(ctx, f)
	if err != nil {
		// No silent degradation: a relay failure is a loud (but calm-worded)
		// patron state, and the event is logged WITHOUT the SDP body (§9.4).
		s.deps.Log.Warn(logging.EvWebRTCPeerClosed, logging.Field("stream", string(f.Stream)))
		return []SignalFrame{{
			Type:   "unavailable",
			Stream: f.Stream,
			Reason: "Listening could not be started. Please contact theater staff.",
		}}
	}
	for _, o := range out {
		if o.Type == "answer" {
			s.deps.Log.Info(logging.EvWebRTCPeerConnected, logging.Field("stream", string(o.Stream)))
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
