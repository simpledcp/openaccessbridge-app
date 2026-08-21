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
// The QR IMAGE was deliberately not generated here for two passes, and the reason
// was right at the time: rendering a QR that nothing on this build host could
// decode would have been an artifact nobody had verified scans, and a QR that
// fails at the auditorium door fails silently, from the patron's side.
//
// That objection is now answered rather than waived. internal/qr is checked
// module-for-module against an unrelated encoder across all 160 version/level
// combinations, every reference symbol was read back by zxing-cpp — the decoder
// engine behind many phone scanner apps — and `make qr-verify` decodes the actual
// PNGs this endpoint serves. What is STILL not proven, and is not claimed
// anywhere on this surface, is a real phone camera in real venue lighting: that
// is BLOCKER B2 and no amount of local decoding clears it.

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/qr"
)

// adminSurface is the admin listener's handler: the session/QR routes plus
// everything adminMux serves.
func (s *Server) adminSurface() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/session", s.requireAdmin(s.handleAdminSession))
	mux.HandleFunc("/api/admin/session/end", s.requireAdmin(postOnly(s.handleAdminSessionEnd)))
	mux.HandleFunc("/api/admin/session/qr.png", s.requireAdmin(s.handleAdminSessionQR))
	// §11.5's staff pre-show gate (checklist.go).
	mux.HandleFunc("/api/admin/preshow", s.requireAdmin(s.handleAdminPreShow))
	// §9.4 support sessions (support.go): the only way back in once SSH is off.
	mux.HandleFunc("/api/admin/support", s.requireAdmin(s.handleAdminSupport))
	mux.HandleFunc("/api/admin/support/close", s.requireAdmin(postOnly(s.handleAdminSupportClose)))
	// §9.4 signed offline updates (update.go). Verify only on this image: there
	// are no A/B slots to install into (BLOCKERS B8).
	mux.HandleFunc("/api/admin/update", s.requireAdmin(s.handleAdminUpdate))
	mux.HandleFunc("/api/admin/update/verify", s.requireAdmin(postOnly(s.handleAdminUpdateVerify)))
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
	// QR describes the scannable form of JoinURL (§10.4, §7.2). The image itself
	// is served separately, because a PNG does not belong inside a JSON status
	// document that the admin page polls.
	QR *QRInfo `json:"qr,omitempty"`
}

// QRInfo is what the operator surface needs to render and, more importantly, to
// PRINT a per-show QR.
type QRInfo struct {
	// URL is the admin route serving the image for the CURRENT session.
	URL string `json:"url"`
	// Version and Modules describe the symbol: 4*Version+17 modules plus the
	// quiet zone on each side.
	Version int `json:"version"`
	Modules int `json:"modules"`
	// ECC is the correction level the symbol carries.
	ECC string `json:"ecc"`
	// PrintWidthMM is the smallest width this symbol should be printed at to
	// stay scannable by a phone at arm's length in poor light. It is derived
	// from THIS symbol, because a longer join URL means a larger version, more
	// modules, and a card that must be physically bigger — a relationship that
	// is invisible on a screen and is exactly where a booth would get it wrong.
	PrintWidthMM int `json:"print_width_mm"`
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
	sess := issued.PatronSession
	// §10.4's client_count_caption. It is a real count of live fan-out
	// subscribers, not an estimate; the hub is already on Deps, so leaving this
	// at zero understated a full house to the booth.
	if s.deps.Hub != nil {
		sess.ClientCountCaption = s.deps.Hub.ClientCount()
	}
	joinURL := s.joinURL(r, issued.Token)
	return SessionResponse{
		Active:               true,
		PatronSession:        sess,
		JoinCode:             issued.JoinCode,
		JoinURL:              joinURL,
		PerShowTokenRequired: s.cfg.PerShowToken,
		QR:                   qrInfo(joinURL),
	}
}

// qrInfo describes the symbol for a join URL, or nil if it cannot be encoded at
// all. Nil rather than a zero struct: an operator must see "no QR" and reach for
// the typed code, not a set of zeroes that reads like a symbol.
func qrInfo(joinURL string) *QRInfo {
	sym, err := qr.Encode([]byte(joinURL), qr.DefaultECC)
	if err != nil {
		return nil
	}
	return &QRInfo{
		URL:          "/api/admin/session/qr.png",
		Version:      sym.Version,
		Modules:      sym.Modules(),
		ECC:          sym.ECC.String(),
		PrintWidthMM: sym.RecommendedPrintWidthMM(),
	}
}

// handleAdminSessionQR serves the current show's join URL as a PNG (§10.4,
// §7.2 "a per-show QR encodes the local URL + session token").
//
// Three properties this handler exists to hold:
//
//   - It is on the ADMIN listener only. The image is a bearer credential for the
//     show — anything that can read it can join — so it is served where an
//     operator authenticates, never on the patron network (§8.4).
//   - It is never cached. A per-show QR that a browser or proxy kept would let
//     yesterday's code admit patrons to today's wrong room, which is the §8.5
//     failure mode this whole session model exists to prevent.
//   - With no live show it is a 404, not a blank image. An operator holding a
//     card up to a phone must be able to tell "no session" from "a QR that will
//     not scan".
func (s *Server) handleAdminSessionQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cur, ok := s.sessions.Current()
	if !ok {
		http.Error(w, "no show session is live: issue one first", http.StatusNotFound)
		return
	}
	scale := qrDefaultScale
	if v := strings.TrimSpace(r.URL.Query().Get("scale")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > qr.MaxScale {
			http.Error(w, "scale must be 1-"+strconv.Itoa(qr.MaxScale), http.StatusBadRequest)
			return
		}
		scale = n
	}
	sym, err := qr.Encode([]byte(s.joinURL(r, cur.Token)), qr.DefaultECC)
	if err != nil {
		// Refused rather than approximated: a truncated join URL would produce a
		// QR that scans perfectly and sends a patron nowhere.
		http.Error(w, "this join URL cannot be encoded as a QR", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := sym.PNG(&buf, scale); err != nil {
		http.Error(w, "could not render the QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// qrDefaultScale is the pixels-per-module the admin page gets without asking.
// At a typical join URL (version 9, 61 modules with its quiet zone) that is a
// 488 px image: large enough to scan straight off a booth monitor, small enough
// to sit in a dashboard tile.
const qrDefaultScale = 8

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
