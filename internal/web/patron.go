package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// msgShowEnded is the terminal patron frame for §7.2's show-end behavior:
// "streams terminate and the session expires at show end; the page shows a polite
// 'this show has ended' state rather than dangling."
const msgShowEnded = "show_ended"

// sessionCookie carries the per-show token after a scan or a typed join code, so
// the token stops travelling in URLs (and out of the phone's history).
const sessionCookie = "cb_session"

// patronMux builds the public patron routes (§7.1, §8.4). It contains NO admin or
// diagnostic routes — that separation is structural, not just authorization.
func (s *Server) patronMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	// §7.1's three routes are three different experiences, not three names for
	// one page: /caption never renders audio controls and /listen never renders
	// caption controls, decided server-side in the template.
	mux.HandleFunc("/accessibility", s.servePatronPage)
	mux.HandleFunc("/caption", s.servePatronPage)
	mux.HandleFunc("/listen", s.servePatronPage)
	mux.Handle("/static/", noStore(s.patronStatic))
	mux.HandleFunc("/api/patron/join", postOnly(s.handlePatronJoin))
	mux.HandleFunc("/api/caption/stream", s.handleCaptionSSE)
	mux.HandleFunc("/api/caption/ws", s.handleCaptionWS)
	mux.HandleFunc("/api/caption/languages", s.handleLanguages)
	mux.HandleFunc("/api/audio/streams", s.handleAudioStreams)
	mux.HandleFunc("/api/audio/ws", s.handleAudioWS)
	mux.HandleFunc("/healthz", handleHealth)
	return securityHeaders(mux)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	target := "/accessibility"
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q // carry a scanned token through the redirect
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// --- page rendering (§7.1 routes and modes) ---

// patronPageData is what the patron template renders from. It carries room
// identity and mode only — never a server address, interface, device or log line
// (§7.2 no admin/diagnostic leakage).
type patronPageData struct {
	Mode       string // accessibility | caption | listen
	Captions   bool
	Audio      bool
	Title      string
	Auditorium string
	NeedCode   bool
	// ShowEnded suppresses the join form: §7.2's show-end state is "a polite
	// 'this show has ended'", not an invitation to type a code that cannot work.
	ShowEnded     bool
	AccessMessage string
	// ModeMessage explains a control the session was NOT granted. Drawing the
	// control anyway would leave the patron on "Reconnecting…" forever while its
	// transport answered 403 — a silent degradation (guardrail #7).
	ModeMessage string
}

func modeForPath(path string) (mode string, captions, audio bool) {
	switch path {
	case "/caption":
		return "caption", true, false
	case "/listen":
		return "listen", false, true
	default:
		return "accessibility", true, true
	}
}

func (s *Server) servePatronPage(w http.ResponseWriter, r *http.Request) {
	// A scanned QR lands here with ?t=<token>. Convert it to a cookie and bounce
	// to a clean URL so the token does not linger in the address bar.
	if tok := r.URL.Query().Get("t"); tok != "" {
		if sess, err := s.sessions.Validate(tok); err == nil {
			s.setSessionCookie(w, sess)
			q := r.URL.Query()
			q.Del("t")
			target := r.URL.Path
			if enc := q.Encode(); enc != "" {
				target += "?" + enc
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
	}

	mode, captions, audio := modeForPath(r.URL.Path)
	data := patronPageData{
		Mode:       mode,
		Captions:   captions,
		Audio:      audio,
		Title:      patronTitle(mode),
		Auditorium: s.auditoriumName(),
	}
	sess, err := s.authorize(r)
	if err != nil {
		data.NeedCode = true
		data.ShowEnded = errors.Is(err, ErrShowEnded) || errors.Is(err, ErrSessionExpired)
		data.AccessMessage = patronAccessCopy(err)
	} else {
		// §10.4: render only what this session was actually granted. The caption
		// and audio transports enforce the same grants, so a control drawn
		// without its grant is a control that can never connect.
		data.Captions = captions && grantsCaption(sess)
		data.Audio = audio && grantsAnyAudio(sess)
		data.ModeMessage = ungrantedCopy(captions && !data.Captions, audio && !data.Audio)
	}
	s.renderPatron(w, data)
}

// renderPatron writes the patron page. It renders into memory first so a template
// failure cannot leave a half-drawn page on a patron's phone with a 200 already
// committed — a broken page is indistinguishable from a broken appliance from the
// seat, so the failure has to be whole (guardrail #7).
func (s *Server) renderPatron(w http.ResponseWriter, data patronPageData) {
	var buf bytes.Buffer
	if err := s.patronTmpl.Execute(&buf, data); err != nil {
		s.deps.Log.Error(logging.EvStateTransition, logging.Field("phase", "patron_page_render_failed"))
		http.Error(w, "This room's system is not ready. Please contact theater staff.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func patronTitle(mode string) string {
	switch mode {
	case "caption":
		return "CaptionBridge — Captions"
	case "listen":
		return "CaptionBridge — Listen"
	default:
		return "CaptionBridge — Captions and Listening"
	}
}

// --- per-show session gate (§9.4, §10.4) ---

// authorize resolves the request's per-show session. When the active deployment
// profile does not require a per-show token (Lab, Booth Diagnostic — deploy
// Profile.PerShowToken=false) it returns (nil, nil): the surface is open and every
// mode is offered. Otherwise a valid, unexpired, this-room token is required.
func (s *Server) authorize(r *http.Request) (*model.PatronSession, error) {
	if !s.cfg.PerShowToken || s.sessions == nil {
		return nil, nil
	}
	tok := r.URL.Query().Get("t")
	if tok == "" {
		if c, err := r.Cookie(sessionCookie); err == nil {
			tok = c.Value
		}
	}
	sess, err := s.sessions.Validate(tok)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// grantsCaption reports whether the resolved session may read captions. A nil
// session means the token gate is off, which grants everything.
func grantsCaption(sess *model.PatronSession) bool {
	return sess == nil || sess.Grants(model.ModeCaption)
}

// grantsAnyAudio reports whether the session may listen to either named stream.
func grantsAnyAudio(sess *model.PatronSession) bool {
	return sess == nil || sess.Grants(model.ModeHI) || sess.Grants(model.ModeVIN)
}

// ungrantedCopy is the calm sentence shown in place of a control this room's code
// does not include (§7.2 clear, non-technical error states).
func ungrantedCopy(lostCaptions, lostAudio bool) string {
	switch {
	case lostCaptions && lostAudio:
		return "This room's code does not include captions or assistive audio. Please contact theater staff."
	case lostCaptions:
		return "Captions are not included in this room's code. Please contact theater staff."
	case lostAudio:
		return "Assistive audio is not included in this room's code. Please contact theater staff."
	}
	return ""
}

// patronAccessCopy turns a session error into calm, non-technical patron copy
// (§7.2). It never names a token, a signature, an auditorium id or a code path.
func patronAccessCopy(err error) string {
	switch {
	case errors.Is(err, ErrWrongAuditorium):
		// §8.5: a stale or wrong-room code must be caught, and the patron told
		// plainly, rather than silently admitted to the wrong screen.
		return "This code is for a different auditorium. Please ask staff for this room's code."
	case errors.Is(err, ErrSessionExpired), errors.Is(err, ErrShowEnded):
		return "This show has ended. Thank you for joining us."
	case errors.Is(err, ErrNoSession):
		return "Scan the code at the door, or ask staff for this room's join code."
	default:
		return "That code was not recognized. Please ask theater staff for a new one."
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, sess model.PatronSession) {
	maxAge := int(sess.RemainingAt(time.Now()).Seconds())
	if maxAge <= 0 {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.Token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure is deliberately not set: DECISIONS D12 defaults the patron path
		// to plain HTTP on the appliance's own LAN, and a Secure cookie would
		// simply never be stored there.
	})
}

// handlePatronJoin exchanges the short join code for a session (§9.4 "per-show
// access code or signed QR token") — the path for a patron who cannot scan.
func (s *Server) handlePatronJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "That code was not recognized. Please ask theater staff for a new one."})
		return
	}
	sess, err := s.sessions.ExchangeCode(body.Code)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": patronAccessCopy(err)})
		return
	}
	s.setSessionCookie(w, sess)
	writeJSON(w, map[string]any{"ok": true, "granted_modes": sess.GrantedModes})
}

// --- language negotiation (§7.1 "language selection where multiple caption
// tracks exist", DECISIONS D8) ---

func (s *Server) availableLanguages() []string {
	if s.deps.Languages == nil {
		return nil
	}
	opts := s.deps.Languages()
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		if o.Code != "" {
			out = append(out, o.Code)
		}
	}
	return out
}

// resolveLanguage negotiates the language to serve for a request and reports
// whether an explicitly requested language had to be substituted.
//
// A phone reporting en-US must not be handed a blank screen while en cues play,
// and a patron who explicitly chose a language that the show does not carry must
// be TOLD, not quietly served another one (guardrail #7).
func (s *Server) resolveLanguage(r *http.Request) (serving string, requested string, substituted bool) {
	explicit := strings.TrimSpace(r.URL.Query().Get("lang"))
	accept := parseAcceptLanguage(r.Header.Get("Accept-Language"))
	return negotiateLanguage(explicit, accept, s.cfg.DefaultLanguage, s.availableLanguages())
}

// NegotiateLanguageForTest exposes negotiateLanguage so a test in the caption
// packages can span the seam between the machine's D8 ranking and the web
// layer's use of it. Neither half is meaningful alone: this is the only place
// the two agree or fail to.
func NegotiateLanguageForTest(explicit string, accept []string, venue string, available []string) (serving, requested string, substituted bool) {
	return negotiateLanguage(explicit, accept, venue, available)
}

// negotiateLanguage applies DECISIONS D8's priority order. An explicit patron
// choice outranks the order (D8 governs the DEFAULT, not an active selection);
// otherwise: (1) the venue-configured default, (2) a browser-language match,
// (3) the first available track.
func negotiateLanguage(explicit string, accept []string, venue string, available []string) (serving, requested string, substituted bool) {
	if len(available) == 0 {
		// No show loaded yet: honour the request verbatim so the hub reports
		// "waiting for the show to start" rather than a false "unavailable".
		if explicit != "" {
			return explicit, explicit, false
		}
		for _, a := range accept {
			if a != "" {
				return a, "", false
			}
		}
		return venue, "", false
	}
	if explicit != "" {
		if m, ok := matchLanguage(explicit, available); ok {
			return m, explicit, false
		}
		// Substituting is allowed (a blank page helps nobody) but never silent:
		// the caller tells the patron which language is actually being served.
		return defaultLanguage(accept, venue, available), explicit, true
	}
	return defaultLanguage(accept, venue, available), "", false
}

func defaultLanguage(accept []string, venue string, available []string) string {
	if m, ok := matchLanguage(venue, available); ok {
		return m // D8 (1) venue/show-configured default
	}
	for _, a := range accept {
		if m, ok := matchLanguage(a, available); ok {
			return m // D8 (2) browser-language match
		}
	}
	// D8 (3)/(4): the most accessible track. `available` arrives in D8 priority
	// order from the caption machine (model.CaptionLinkState.AvailableLanguages),
	// so the head is the closed-caption track where the show carries one. This
	// line used to say the same thing while receiving raw RPL order, which made
	// it the first track the PLAYLIST happened to list instead.
	return available[0]
}

// matchLanguage resolves a BCP-47 tag against the available tracks: exact match
// first, then a primary-subtag match (en-US → en, es-MX → es-419).
func matchLanguage(tag string, available []string) (string, bool) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", false
	}
	for _, a := range available {
		if strings.EqualFold(a, tag) {
			return a, true
		}
	}
	prim := primarySubtag(tag)
	if prim == "" {
		return "", false
	}
	var first string
	for _, a := range available {
		if primarySubtag(a) != prim {
			continue
		}
		if strings.EqualFold(a, prim) {
			return a, true // the generic track ("en") is the best fit for "en-US"
		}
		if first == "" {
			first = a
		}
	}
	if first != "" {
		return first, true
	}
	return "", false
}

func primarySubtag(tag string) string {
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		return strings.ToLower(tag[:i])
	}
	return strings.ToLower(tag)
}

// parseAcceptLanguage returns the header's tags ordered by q-value, highest
// first. No such parser existed anywhere in the repo, so D8's step (2) had no
// input on the patron path.
func parseAcceptLanguage(h string) []string {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	type entry struct {
		tag string
		q   float64
		ord int
	}
	var entries []entry
	for i, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag := part
		q := 1.0
		if semi := strings.Index(part, ";"); semi >= 0 {
			tag = strings.TrimSpace(part[:semi])
			for _, p := range strings.Split(part[semi+1:], ";") {
				p = strings.TrimSpace(p)
				if v, ok := strings.CutPrefix(p, "q="); ok {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = f
					}
				}
			}
		}
		if tag == "" || tag == "*" || q <= 0 {
			continue
		}
		entries = append(entries, entry{tag: tag, q: q, ord: i})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].q != entries[j].q {
			return entries[i].q > entries[j].q
		}
		return entries[i].ord < entries[j].ord
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.tag)
	}
	return out
}

func (s *Server) handleLanguages(w http.ResponseWriter, r *http.Request) {
	var langs []LangOption
	if s.deps.Languages != nil {
		langs = s.deps.Languages()
	}
	if len(langs) == 0 {
		langs = []LangOption{{Code: s.cfg.DefaultLanguage, Label: s.cfg.DefaultLanguage}}
	}
	writeJSON(w, langs)
}

// languageFrame is the out-of-band frame that tells the page which language is
// actually being served. It is generated by the web layer (the fan-out Message is
// a shared per-language value and cannot carry a per-subscriber negotiation
// result), and it is what keeps a substitution from being silent.
func languageFrame(serving, requested string, substituted bool, available []string) map[string]any {
	f := map[string]any{
		"type":      "language",
		"serving":   serving,
		"available": available,
	}
	if substituted {
		f["requested"] = requested
		f["substituted"] = true
	}
	return f
}

// --- caption transports (§5.6) ---

// captionSession authorizes a caption transport request, returning the negotiated
// language. A patron whose session does not grant captions is refused with calm
// copy rather than handed an empty stream.
func (s *Server) captionSession(w http.ResponseWriter, r *http.Request) (lang string, requested string, substituted bool, ok bool) {
	sess, err := s.authorize(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": patronAccessCopy(err)})
		return "", "", false, false
	}
	if !grantsCaption(sess) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "Captions are not included in this room's code. Please contact theater staff."})
		return "", "", false, false
	}
	lang, requested, substituted = s.resolveLanguage(r)
	return lang, requested, substituted, true
}

// handleCaptionSSE streams caption state over Server-Sent Events (the simpler
// fallback transport, §5.6). EventSource auto-reconnects; on reconnect the hub
// immediately re-sends the current state.
func (s *Server) handleCaptionSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lang, requested, substituted, ok := s.captionSession(w, r)
	if !ok {
		return
	}
	sub, err := s.deps.Hub.Subscribe(lang)
	if err != nil {
		http.Error(w, "caption stream at capacity", http.StatusServiceUnavailable)
		return
	}
	defer s.deps.Hub.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	showEnded := s.sessions.ShowEnded()
	writeEvent := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeEvent(languageFrame(lang, requested, substituted, s.availableLanguages())) {
		return
	}

	msgs := subscriberChannel(ctx, sub)
	for {
		select {
		case <-ctx.Done():
			return
		case <-showEnded:
			// §7.2: terminate politely instead of dangling on "waiting for the
			// show to start" forever.
			writeEvent(map[string]string{"type": msgShowEnded})
			return
		case msg, alive := <-msgs:
			if !alive {
				return
			}
			if !writeEvent(msg) {
				return
			}
		}
	}
}

// handleCaptionWS streams caption state over WebSocket (the primary transport,
// §5.6) and accepts {type:"setLanguage"} control messages for in-place language
// switching. Origin is unrestricted: this is a local-only, receive-oriented
// caption feed on the appliance's own patron LAN, not a CSRF-sensitive surface.
func (s *Server) handleCaptionWS(w http.ResponseWriter, r *http.Request) {
	lang, requested, substituted, ok := s.captionSession(w, r)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()

	sub, err := s.deps.Hub.Subscribe(lang)
	if err != nil {
		_ = c.Close(websocket.StatusTryAgainLater, "caption stream at capacity")
		return
	}
	defer s.deps.Hub.Unsubscribe(sub)

	ctx := r.Context()
	showEnded := s.sessions.ShowEnded()
	write := func(v any) error {
		b, merr := json.Marshal(v)
		if merr != nil {
			return merr
		}
		return c.Write(ctx, websocket.MessageText, b)
	}
	if err := write(languageFrame(lang, requested, substituted, s.availableLanguages())); err != nil {
		return
	}

	// Reader goroutine: handle language-switch control messages.
	ctl := make(chan string, 4)
	go func() {
		defer close(ctl)
		for {
			_, data, rerr := c.Read(ctx)
			if rerr != nil {
				return
			}
			var m struct {
				Type     string `json:"type"`
				Language string `json:"language"`
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "setLanguage" && m.Language != "" {
				select {
				case ctl <- m.Language:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	msgs := subscriberChannel(ctx, sub)
	for {
		select {
		case <-ctx.Done():
			return
		case <-showEnded:
			_ = write(map[string]string{"type": msgShowEnded})
			_ = c.Close(websocket.StatusNormalClosure, "show ended")
			return
		case want, alive := <-ctl:
			if !alive {
				return
			}
			avail := s.availableLanguages()
			serving, sub2 := want, false
			if len(avail) > 0 {
				if m, matched := matchLanguage(want, avail); matched {
					serving = m
				} else {
					serving, sub2 = defaultLanguage(nil, s.cfg.DefaultLanguage, avail), true
				}
			}
			if err := write(languageFrame(serving, want, sub2, avail)); err != nil {
				return
			}
			s.deps.Hub.SetLanguage(sub, serving)
		case msg, alive := <-msgs:
			if !alive {
				return
			}
			if err := write(msg); err != nil {
				return
			}
		}
	}
}

// subscriberChannel adapts a blocking fan-out subscriber to a channel so a
// transport can also select on show-end and cancellation.
func subscriberChannel(ctx context.Context, sub *fanout.Subscriber) <-chan fanout.Message {
	out := make(chan fanout.Message, 1)
	go func() {
		defer close(out)
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				return
			}
			select {
			case out <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// securityHeaders applies a tight, offline-friendly Content-Security-Policy and
// related headers. No external origins are permitted (local-only appliance).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; "+
				"style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// auditoriumName is the room name shown to a patron (§7.2) and relied on by
// §8.5 as the last human check against a wrong-room join.
//
// An unnamed room renders EMPTY rather than falling back to a build-time
// default. "Auditorium 1" on every screen in a multiplex is worse than no name
// at all: it is a wrong name that looks like a right one, and the whole reason
// the page carries a room name is so a patron or passing staff member can catch
// exactly that. The admin surface raises the unnamed state loudly instead.
func (s *Server) auditoriumName() string {
	if s.deps.AuditoriumName != nil {
		if name := strings.TrimSpace(s.deps.AuditoriumName()); name != "" {
			return name
		}
		return ""
	}
	return strings.TrimSpace(s.cfg.Auditorium)
}
