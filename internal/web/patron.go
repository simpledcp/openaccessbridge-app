package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// patronMux builds the public patron routes (§7.1, §8.4). It contains NO admin or
// diagnostic routes — that separation is structural, not just authorization.
func (s *Server) patronMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/accessibility", s.servePatronPage)
	mux.HandleFunc("/caption", s.servePatronPage)
	mux.HandleFunc("/listen", s.servePatronPage)
	mux.Handle("/static/", noStore(s.static))
	mux.HandleFunc("/api/caption/stream", s.handleCaptionSSE)
	mux.HandleFunc("/api/caption/ws", s.handleCaptionWS)
	mux.HandleFunc("/api/caption/languages", s.handleLanguages)
	mux.HandleFunc("/healthz", handleHealth)
	return securityHeaders(mux)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/accessibility", http.StatusFound)
}

func (s *Server) servePatronPage(w http.ResponseWriter, r *http.Request) {
	b, err := assetsFS.ReadFile("assets/patron.html")
	if err != nil {
		http.Error(w, "not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
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

func (s *Server) langOrDefault(r *http.Request) string {
	if l := r.URL.Query().Get("lang"); l != "" {
		return l
	}
	return s.cfg.DefaultLanguage
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
	sub, err := s.deps.Hub.Subscribe(s.langOrDefault(r))
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
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		b, _ := json.Marshal(msg)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		flusher.Flush()
	}
}

// handleCaptionWS streams caption state over WebSocket (the primary transport,
// §5.6) and accepts {type:"setLanguage"} control messages for in-place language
// switching. Origin is unrestricted: this is a local-only, receive-oriented
// caption feed on the appliance's own patron LAN, not a CSRF-sensitive surface.
func (s *Server) handleCaptionWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()

	sub, err := s.deps.Hub.Subscribe(s.langOrDefault(r))
	if err != nil {
		_ = c.Close(websocket.StatusTryAgainLater, "caption stream at capacity")
		return
	}
	defer s.deps.Hub.Unsubscribe(sub)

	ctx := r.Context()
	// Reader goroutine: handle language-switch control messages.
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var ctl struct {
				Type     string `json:"type"`
				Language string `json:"language"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "setLanguage" && ctl.Language != "" {
				s.deps.Hub.SetLanguage(sub, ctl.Language)
			}
		}
	}()

	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		b, _ := json.Marshal(msg)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			return
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
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
