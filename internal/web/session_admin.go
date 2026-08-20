package web

// Operator surface for per-show patron sessions (buildplan.md §7.2 onboarding,
// §9.4 per-show access, §10.4 session object).
//
// These routes are composed IN FRONT of adminMux rather than registered inside it:
// internal/web/admin.go belongs to the admin/setup lane, and adding routes there
// would collide. The composition is behaviourally identical — same listener, same
// requireAdmin, same security headers — and keeps the patron-session surface in
// one file with the store that backs it.
//
// The QR IMAGE is deliberately not generated here. Rendering a QR that no offline
// tool on this build host can decode would be an artifact nobody has verified
// scans, which is exactly the fabricated result the house rules forbid (and a QR
// that fails at the auditorium door fails silently, from the patron's side). What
// this endpoint returns instead is the exact payload a QR must carry — the join
// URL — plus a short typed code that works today. Rendering the payload as a QR is
// a seam for whoever owns the admin page, and validating a real phone camera scan
// is BLOCKER B2 regardless.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// adminSurface is the admin listener's handler: the session/QR routes plus
// everything adminMux serves.
func (s *Server) adminSurface() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/session", s.requireAdmin(s.handleAdminSession))
	mux.HandleFunc("/api/admin/session/end", s.requireAdmin(postOnly(s.handleAdminSessionEnd)))
	mux.Handle("/", s.adminMux())
	return securityHeaders(mux)
}

// SessionResponse is the operator-facing view of a per-show session (§10.4) plus
// the onboarding artifacts.
type SessionResponse struct {
	Active bool `json:"active"`
	model.PatronSession
	// JoinCode is the short code a patron can type when they cannot scan.
	JoinCode string `json:"join_code,omitempty"`
	// JoinURL is the exact payload a per-show QR must encode (§7.2: "a per-show QR
	// encodes the local URL + session token").
	JoinURL string `json:"join_url,omitempty"`
	// PerShowTokenRequired mirrors the active profile, so the operator can see
	// whether patrons actually need this code at all.
	PerShowTokenRequired bool `json:"per_show_token_required"`
}

func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cur, ok := s.sessions.Current()
		if !ok {
			writeJSON(w, SessionResponse{PerShowTokenRequired: s.cfg.PerShowToken})
			return
		}
		writeJSON(w, s.sessionResponse(r, cur))
	case http.MethodPost:
		var body struct {
			ShowID     string   `json:"show_id"`
			Modes      []string `json:"modes"`
			LifetimeMS int64    `json:"lifetime_ms"`
		}
		if r.Body != nil {
			// An empty body is a valid "mint the default session" request.
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		var modes []model.PatronMode
		for _, m := range body.Modes {
			pm, ok := model.ParsePatronMode(strings.TrimSpace(m))
			if !ok {
				http.Error(w, "unknown patron mode "+m, http.StatusBadRequest)
				return
			}
			modes = append(modes, pm)
		}
		issued, err := s.sessions.Issue(IssueOptions{
			ShowID:   body.ShowID,
			Modes:    modes,
			Lifetime: time.Duration(body.LifetimeMS) * time.Millisecond,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Audited without content: the show id is opaque and the token is not
		// logged (§9.4 — no client identifiers, no secrets in logs).
		s.deps.Log.Info(logging.EvStateTransition,
			logging.Field("phase", "patron_session_issued"),
			logging.Field("auditorium_id", issued.AuditoriumID))
		writeJSON(w, s.sessionResponse(r, issued))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminSessionEnd(w http.ResponseWriter, r *http.Request) {
	s.sessions.EndShow()
	s.deps.Log.Info(logging.EvStateTransition, logging.Field("phase", "patron_session_show_end"))
	writeJSON(w, SessionResponse{PerShowTokenRequired: s.cfg.PerShowToken})
}

func (s *Server) sessionResponse(r *http.Request, issued IssuedSession) SessionResponse {
	return SessionResponse{
		Active:               true,
		PatronSession:        issued.PatronSession,
		JoinCode:             issued.JoinCode,
		JoinURL:              s.joinURL(r, issued.Token),
		PerShowTokenRequired: s.cfg.PerShowToken,
	}
}

// joinURL builds the address a patron's phone should open. It points at the
// PATRON bind address, never the admin one the operator is currently talking to.
func (s *Server) joinURL(r *http.Request, token string) string {
	host := patronHostHint(s.cfg.PatronBind, r)
	u := url.URL{
		Scheme:   "http", // DECISIONS D12: plain HTTP on the patron LAN by default
		Host:     host,
		Path:     "/accessibility",
		RawQuery: url.Values{"t": []string{token}}.Encode(),
	}
	return u.String()
}

// patronHostHint renders the patron listener's host:port. A wildcard bind has no
// single address to advertise; rather than invent one, it falls back to the host
// the operator reached the appliance on, which at least resolves for them.
func patronHostHint(bind string, r *http.Request) string {
	h, p, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if isWildcardHost(h) {
		if r != nil && r.Host != "" {
			if rh, _, rerr := net.SplitHostPort(r.Host); rerr == nil {
				return net.JoinHostPort(rh, p)
			}
			return net.JoinHostPort(r.Host, p)
		}
	}
	return net.JoinHostPort(h, p)
}
