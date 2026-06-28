package web

import (
	"net/http"

	"github.com/simpledcp/captionbridge/internal/model"
)

// adminMux builds the admin routes (§8.4). It is served on a separate listener
// from the patron mux; in production profiles that listener is loopback/isolated
// (enforced in validateBindSeparation). The full dashboard + setup wizard are
// built in the admin milestone; this provides the authenticated entry point and
// the readiness JSON the dashboard renders.
func (s *Server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin", s.serveAdminPage)
	mux.Handle("/static/", noStore(s.static))
	mux.HandleFunc("/api/admin/status", s.handleAdminStatus)
	mux.HandleFunc("/healthz", handleHealth)
	return securityHeaders(mux)
}

func (s *Server) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	b, err := assetsFS.ReadFile("assets/admin.html")
	if err != nil {
		http.Error(w, "not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	var st model.SystemReadiness
	if s.deps.Status != nil {
		st = s.deps.Status()
	}
	writeJSON(w, st)
}
