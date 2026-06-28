// Package web serves the patron and admin HTTP surfaces (buildplan.md §7, §8).
//
// Public (patron) and admin surfaces are separated in routes AND bind address
// (§8.4). They are served by two independent muxes on two independent listeners,
// so the patron mux can never reach an admin route. In production profiles,
// binding the admin surface on the patron interface is refused (§8.4: "an admin
// surface reachable from the patron side refuses to leave setup").
package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// LangOption is a selectable caption language for the patron UI.
type LangOption struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// Config configures the web server.
type Config struct {
	Profile         model.FirewallProfile
	PatronBind      string // e.g. "0.0.0.0:8080"
	AdminBind       string // e.g. "127.0.0.1:8443" — must be isolated in prod
	Auditorium      string
	DefaultLanguage string
}

// Deps are the runtime dependencies injected by the core process.
type Deps struct {
	Hub       *fanout.Hub
	Log       *logging.Logger
	Status    func() model.SystemReadiness // admin readiness snapshot
	Languages func() []LangOption          // available caption languages
	Store     config.Store                 // config store (setup wizard, admin auth)
	// SetupWindow is how long first-boot setup mode stays open before auto-expiry
	// (§8.1 step 14). Zero uses a default.
	SetupWindow time.Duration
}

// Server owns the patron and admin HTTP servers.
type Server struct {
	cfg    Config
	deps   Deps
	patron *http.Server
	admin  *http.Server
	static http.Handler
	setup  *SetupManager
}

// NewServer validates the configuration and builds the patron + admin servers.
func NewServer(cfg Config, deps Deps) (*Server, error) {
	if deps.Log == nil {
		deps.Log = logging.Nop()
	}
	if cfg.DefaultLanguage == "" {
		cfg.DefaultLanguage = "en"
	}
	if err := validateBindSeparation(cfg); err != nil {
		return nil, err
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, deps: deps, static: http.StripPrefix("/static/", http.FileServer(http.FS(sub)))}
	if deps.Store != nil {
		s.setup = NewSetupManager(deps.Store, deps.Log, cfg.Profile, deps.SetupWindow)
	}

	s.patron = &http.Server{
		Addr:              cfg.PatronBind,
		Handler:           s.patronMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.AdminBind != "" {
		s.admin = &http.Server{
			Addr:              cfg.AdminBind,
			Handler:           s.adminMux(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return s, nil
}

// validateBindSeparation enforces §8.4: in production profiles, the admin surface
// must NOT be reachable on the patron interface.
func validateBindSeparation(cfg Config) error {
	if cfg.PatronBind == "" {
		return errors.New("patron bind address required")
	}
	if cfg.Profile.Production() {
		if cfg.AdminBind == "" {
			return errors.New("production profile requires a separate admin bind address")
		}
		if AdminReachableFromPatron(cfg.PatronBind, cfg.AdminBind) {
			return fmt.Errorf("admin surface %q would be reachable on the patron interface %q; "+
				"production profiles require admin on a separate/loopback interface (§8.4)",
				cfg.AdminBind, cfg.PatronBind)
		}
	}
	return nil
}

// AdminReachableFromPatron reports whether an admin listener at adminBind is
// reachable from the patron network at patronBind. Loopback admin is isolated;
// admin on the same interface or on all-interfaces (0.0.0.0/::) is reachable.
func AdminReachableFromPatron(patronBind, adminBind string) bool {
	ah := hostOf(adminBind)
	if isLoopbackHost(ah) {
		return false
	}
	if isWildcardHost(ah) {
		return true // admin on all interfaces is reachable from the patron side
	}
	ph := hostOf(patronBind)
	if isWildcardHost(ph) {
		return true // patron on all interfaces means admin's interface is included
	}
	return ah == ph
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isWildcardHost(h string) bool {
	if h == "" || h == "*" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsUnspecified() // 0.0.0.0 or ::
}

// Start begins serving. It returns when both servers have stopped; errors other
// than http.ErrServerClosed are reported on errCh.
func (s *Server) Start(errCh chan<- error) {
	go func() {
		if err := s.patron.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("patron server: %w", err)
		}
	}()
	if s.admin != nil {
		go func() {
			if err := s.admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}
}

// Shutdown gracefully stops both servers.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	if e := s.patron.Shutdown(ctx); e != nil {
		err = e
	}
	if s.admin != nil {
		if e := s.admin.Shutdown(ctx); e != nil {
			err = e
		}
	}
	return err
}

// PatronHandler exposes the patron mux (used by tests via httptest).
func (s *Server) PatronHandler() http.Handler { return s.patronMux() }

// AdminHandler exposes the admin mux (used by tests via httptest).
func (s *Server) AdminHandler() http.Handler { return s.adminMux() }
