package web

// Recording the §12.4 / §1.3 Phase 9 operational and legal review.
//
// This appliance cannot verify that a review happened — it is an organisational
// fact about a venue, not a measurement — and the surfaces say so rather than
// presenting a stored string as a check. What recording it DOES achieve is that
// the claim cannot go unmade: §12.4 says the Low-Cost Replacement profile "may
// only ship to a venue after Phase 9 operational/legal review", and without
// somewhere to put that, the profile that replaces a professional receiver
// system could be selected by anyone, silently, with nothing anywhere saying it
// had been considered.
//
// It is deliberately NOT the §11.5 staff-step treatment (D30). A per-show tick
// the appliance cannot verify ages badly and is refused there. A Phase 9 review
// is a one-time organisational event with a reference and a date; it does not go
// stale between shows, and a durable record of it is what a later reviewer
// actually needs.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Phase9Response is the operator-facing state of the review record.
type Phase9Response struct {
	// Recorded: a reference is on file.
	Recorded bool `json:"recorded"`
	// Reference is the operator's own note — a ticket, a date, a signatory.
	Reference string `json:"reference,omitempty"`
	// Required: the active profile is one §12.4 gates behind the review.
	Required bool `json:"required"`
	// Note states, in words, that this is a record and not a verification.
	Note string `json:"note"`
}

func (s *Server) handleAdminPhase9(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		http.Error(w, "no configuration store", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.phase9Response())
	case http.MethodPost:
		var body struct {
			Reference string `json:"reference"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		ref := strings.TrimSpace(body.Reference)
		if len(ref) > 200 {
			ref = ref[:200]
		}
		if err := s.deps.Store.PutSetting(config.SettingPhase9Review, ref); err != nil {
			writeJSONError(w, "the review reference could not be saved", http.StatusInternalServerError)
			return
		}
		// Audited: recording (or clearing) the review that gates a
		// replacement-grade deployment is a decision worth being able to review
		// later. The reference is operator free text and stays out of the log.
		s.deps.Log.Warn(logging.EvStateTransition,
			logging.Field("phase", "phase9_review_recorded"),
			logging.Field("present", ref != ""),
			logging.Field("reference_len", len(ref)))
		writeJSON(w, s.phase9Response())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) phase9Response() Phase9Response {
	ref, _, _ := s.deps.Store.GetSetting(config.SettingPhase9Review)
	ref = strings.TrimSpace(ref)
	out := Phase9Response{
		Recorded:  ref != "",
		Reference: ref,
		Required:  s.cfg.Profile == model.ProfileLowCostReplacement,
	}
	if out.Recorded {
		out.Note = "Recorded. This appliance cannot verify that a review took place — this is the " +
			"reference an operator entered, kept so a later reviewer can find it."
	} else {
		out.Note = "No review is recorded. §12.4 permits the Low-Cost Replacement profile only after " +
			"the Phase 9 operational and legal review, because that is the profile a venue uses in " +
			"place of a professional receiver system."
	}
	return out
}
