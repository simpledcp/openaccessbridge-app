package web

// §9.4's debug capture, on the operator surface.
//
// WHY THIS EXISTS AT ALL, given that this appliance's whole posture is that
// caption text and audio never touch disk. §9.4 does not forbid capturing them;
// it forbids capturing them CASUALLY. It offers one sanctioned way — "explicit,
// admin-only, time-limited, size-limited (audio), and visibly indicated on the
// dashboard while active, writing only to a bounded tmpfs, auto-deleting on a
// timer and on reboot" — as the trade against the no-content rule.
//
// `internal/logging/debugcapture` implemented every one of those constraints and
// had no production caller, so the sanctioned way did not exist on any surface.
// That is worse than it sounds: a support engineer who cannot capture evidence
// the sanctioned way does not stop needing evidence. They add a log line, or ask
// a booth to film the screen with a phone — and neither is bounded, admin-only,
// time-limited, visible on the dashboard, or deleted afterwards.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging/debugcapture"
)

// DebugCaptureResponse is the operator-facing state.
type DebugCaptureResponse struct {
	// Available: this appliance has a capture sink at all.
	Available bool `json:"available"`
	// Active captures, content-free (no text, no samples — Status carries counts
	// and the operator's own reason).
	Active []debugcapture.Status `json:"active,omitempty"`
	// MaxSeconds and MaxBytes are the ceilings a grant is clamped to, so the
	// page can say what it will actually get rather than what was asked for.
	MaxSeconds int   `json:"max_seconds"`
	MaxBytes   int64 `json:"max_bytes"`
	// Note states the trade in words for the surface that renders it.
	Note string `json:"note,omitempty"`
}

func (s *Server) handleAdminDebugCapture(w http.ResponseWriter, r *http.Request) {
	if s.deps.DebugCapture == nil {
		http.Error(w, "this appliance has no debug-capture sink", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.debugCaptureResponse())
	case http.MethodPost:
		var body struct {
			Kind    string `json:"kind"`
			Seconds int    `json:"seconds"`
			Reason  string `json:"reason"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		kind := debugcapture.Kind(body.Kind)
		if kind != debugcapture.KindCaption && kind != debugcapture.KindAudio {
			writeJSONError(w, `kind must be "caption" or "audio"`, http.StatusBadRequest)
			return
		}
		// §9.4's "admin-only" is an IDENTIFIED admin: the grant is recorded
		// against whoever authorised it, hashed before it reaches a log.
		user, _, _ := r.BasicAuth()
		if _, err := s.deps.DebugCapture.Start(debugcapture.Grant{
			Kind:      kind,
			TTL:       time.Duration(body.Seconds) * time.Second,
			GrantedBy: user,
			Reason:    body.Reason,
		}); err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.debugCaptureResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminDebugCaptureStop(w http.ResponseWriter, r *http.Request) {
	if s.deps.DebugCapture == nil {
		http.Error(w, "this appliance has no debug-capture sink", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Kind string `json:"kind"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	}
	if err := s.deps.DebugCapture.Stop(debugcapture.Kind(body.Kind)); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.debugCaptureResponse())
}

func (s *Server) debugCaptureResponse() DebugCaptureResponse {
	out := DebugCaptureResponse{
		Available:  true,
		Active:     s.deps.DebugCapture.Sessions(),
		MaxSeconds: int(debugcapture.DefaultMaxTTL / time.Second),
		MaxBytes:   debugcapture.DefaultAudioMaxByte,
	}
	if len(out.Active) > 0 {
		out.Note = "A capture is running. It is writing caption text or audio samples to a bounded " +
			"tmpfs, it deletes itself when the window closes and on reboot, and it is shown on the " +
			"dashboard for as long as it runs."
	} else {
		out.Note = "Nothing is being captured. A capture is explicit, admin-only, time-limited and " +
			"size-limited, and it is the only sanctioned way to record caption text or audio for a " +
			"support case (§9.4)."
	}
	return out
}
