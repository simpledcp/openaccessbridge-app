package web

// The §9.4 support session on the operator surface.
//
// This is the only page in the product that opens a way INTO the appliance, so
// it is written to make the state of that way in unmistakable. Three things it
// deliberately does:
//
//   - It refuses to open a session without a reason. §9.4 asks for an audited
//     feature, and an audit entry with no reason is a timestamp.
//   - It never reports a window as a connection. This appliance permits an
//     outbound destination; it has no tunnel client and dials nothing, and the
//     response carries that distinction as a field rather than as prose.
//   - It shows the RECENT TRAIL beside the switch. A support mode whose history
//     is only visible in a database is one nobody reviews.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/support"
)

// SupportResponse is the operator-facing view.
type SupportResponse struct {
	support.Report
	// Recent is the audit trail, newest first, so the switch and its history are
	// on the same screen.
	Recent []model.SupportSession `json:"recent,omitempty"`
	// MaxMinutes is the ceiling the appliance will grant whatever is asked for.
	MaxMinutes int `json:"max_minutes"`
}

func (s *Server) handleAdminSupport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Support == nil {
		http.Error(w, "support sessions are not available on this appliance", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.supportResponse())
	case http.MethodPost:
		var body struct {
			Reason  string `json:"reason"`
			Minutes int    `json:"minutes"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		// The authenticated operator, for the audit trail. Only a hash of it is
		// stored, and the name never reaches a log (§9.4).
		user, _, _ := r.BasicAuth()
		_, err := s.deps.Support.Open(support.OpenRequest{
			Reason:    body.Reason,
			AdminUser: user,
			Duration:  time.Duration(body.Minutes) * time.Minute,
		})
		if err != nil {
			// The reason is operator-facing here — this is the admin surface, and
			// "no endpoint is configured" is exactly what a booth needs to read.
			// It is returned as JSON so the page never has to read a response as
			// text, which is a habit the admin surface deliberately does not have
			// (the dashboard must not learn to render bodies it did not parse).
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.supportResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminSupportClose(w http.ResponseWriter, r *http.Request) {
	if s.deps.Support == nil {
		http.Error(w, "support sessions are not available on this appliance", http.StatusServiceUnavailable)
		return
	}
	if err := s.deps.Support.Close(); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.supportResponse())
}

func (s *Server) supportResponse() SupportResponse {
	out := SupportResponse{
		Report:     s.deps.Support.Status(),
		MaxMinutes: int(model.MaxSupportSession / time.Minute),
	}
	if s.deps.SupportTrail != nil {
		if recent, err := s.deps.SupportTrail(10); err == nil {
			out.Recent = recent
		}
	}
	return out
}

// writeJSONError returns an operator-facing reason as JSON.
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
