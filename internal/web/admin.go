package web

import (
	"net/http"

	"github.com/simpledcp/captionbridge/internal/model"
)

// adminMux builds the admin routes (§8.4). It is served on a separate listener
// from the patron mux; in production profiles that listener is loopback/isolated
// (enforced in validateBindSeparation). Admin endpoints are authenticated
// (requireAdmin); the first-boot setup wizard is open only until a credential
// exists.
func (s *Server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin", s.requireAdmin(s.serveAdminPage))
	mux.Handle("/static/", noStore(s.static))
	mux.HandleFunc("/api/admin/status", s.requireAdmin(s.handleAdminStatus))
	mux.HandleFunc("/healthz", handleHealth)

	if s.setup != nil {
		mux.HandleFunc("/admin/setup", s.requireAdmin(s.serveSetupPage))
		mux.HandleFunc("/api/admin/setup/state", s.requireAdmin(s.handleSetupState))
		mux.HandleFunc("/api/admin/setup/credentials", s.requireAdmin(postOnly(s.handleSetupCredentials)))
		mux.HandleFunc("/api/admin/setup/network", s.requireAdmin(postOnly(s.handleSetupNetwork)))
		mux.HandleFunc("/api/admin/setup/server", s.requireAdmin(postOnly(s.handleSetupServer)))
		mux.HandleFunc("/api/admin/setup/profile", s.requireAdmin(postOnly(s.handleSetupProfile)))
		mux.HandleFunc("/api/admin/setup/complete", s.requireAdmin(postOnly(s.handleSetupComplete)))
	}
	return securityHeaders(mux)
}

func (s *Server) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, "assets/admin.html")
}

func (s *Server) serveSetupPage(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, "assets/setup.html")
}

func (s *Server) serveAsset(w http.ResponseWriter, name string) {
	b, err := assetsFS.ReadFile(name)
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

func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}
