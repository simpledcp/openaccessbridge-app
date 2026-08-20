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
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"strings"
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
	Profile    model.FirewallProfile
	PatronBind string // e.g. "0.0.0.0:8080"
	AdminBind  string // e.g. "127.0.0.1:8443" — must be isolated in prod
	// Auditorium is the room name shown to patrons (§7.2 "You are connected to
	// Auditorium 3"). Room identity only — never plot or title detail.
	Auditorium string
	// AuditoriumID is the stable room identifier bound into per-show tokens so a
	// code from another room is refused (§8.5). Defaults to a slug of Auditorium.
	AuditoriumID    string
	DefaultLanguage string
	// PerShowToken requires a valid per-show session on the patron surface. It
	// mirrors deploy.Profile.PerShowToken (true for Patron-Facing and Low-Cost
	// Replacement, false for Lab and Booth Diagnostic).
	PerShowToken bool
}

// Deps are the runtime dependencies injected by the core process.
type Deps struct {
	Hub       *fanout.Hub
	Log       *logging.Logger
	Status    func() model.SystemReadiness // admin readiness snapshot
	Languages func() []LangOption          // available caption languages
	Store     config.Store                 // config store (setup wizard, admin auth)
	// Audio is the live per-stream audio state, projected down to the patron-safe
	// availability model by internal/web/audio.go (§6.6 → §7.2). Nil means no
	// audio is configured, which the patron page reports honestly.
	Audio func() []model.AudioStreamState
	// Signaler relays WebRTC SDP/ICE to captionbridge-media (§6.7). Nil in the
	// shipped binary — see the SEAM note on AudioSignaler (BLOCKERS B4/B2).
	Signaler AudioSignaler
	// Calibration drives the §6.4 audio calibration wizard: the device list and
	// the live input meter both come from captionbridge-media, which owns every
	// capture device. Nil builds the loopback driver for MediaSocketDir, so the
	// wizard works on a default deployment with no wiring; injecting one is how
	// tests (and any non-default transport) substitute for it.
	Calibration CalibrationDriver
	// MediaSocketDir is the loopback socket directory captionbridge-media listens
	// in (§3.5). Empty uses ipc.DefaultSocketDir(); the core process should pass
	// the --socket-dir it actually supervises so the two cannot drift apart.
	MediaSocketDir string
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
	// patronStatic serves ONLY the patron assets, and adminStatic only the admin
	// ones. §7.2 forbids any admin/diagnostic leakage onto the patron surface and
	// says it is "enforced by route separation and bind-address separation" — a
	// single shared asset handler quietly defeats both, because the patron
	// listener then serves the dashboard's markup and its booth diagnostics to
	// the untrusted BYOD network.
	patronStatic http.Handler
	adminStatic  http.Handler
	setup        *SetupManager
	sessions     *SessionStore
	patronTmpl   *template.Template
}

// NewServer validates the configuration and builds the patron + admin servers.
func NewServer(cfg Config, deps Deps) (*Server, error) {
	if deps.Log == nil {
		deps.Log = logging.Nop()
	}
	if cfg.DefaultLanguage == "" {
		cfg.DefaultLanguage = "en"
	}
	if cfg.Auditorium == "" {
		cfg.Auditorium = "this auditorium"
	}
	if cfg.AuditoriumID == "" {
		// §8.5 needs a stable room id inside every token. Deriving one from the
		// displayed room name is better than an empty binding, which would make
		// every code valid in every room.
		cfg.AuditoriumID = slugAuditorium(cfg.Auditorium)
	}
	if deps.Calibration == nil {
		// §6.4's wizard needs the process that owns the audio devices. Building
		// the loopback driver here (it dials lazily, per request) means a default
		// deployment needs no extra wiring; a caller that supervises a media
		// helper on a non-default socket dir passes MediaSocketDir.
		deps.Calibration = NewMediaCalibrationDriver(defaultMediaSocket(deps.MediaSocketDir))
	}
	if err := validateBindSeparation(cfg); err != nil {
		return nil, err
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}
	patronStatic, err := surfaceFS(sub, patronAssets)
	if err != nil {
		return nil, err
	}
	adminStatic, err := surfaceFS(sub, adminAssets)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.ParseFS(assetsFS, "assets/patron.html")
	if err != nil {
		return nil, fmt.Errorf("parse patron page: %w", err)
	}
	s := &Server{
		cfg:          cfg,
		deps:         deps,
		patronStatic: http.StripPrefix("/static/", http.FileServer(http.FS(patronStatic))),
		adminStatic:  http.StripPrefix("/static/", http.FileServer(http.FS(adminStatic))),
		patronTmpl:   tmpl.Lookup("patron.html"),
	}
	if s.patronTmpl == nil {
		return nil, errors.New("patron page template missing")
	}
	sessions, err := NewSessionStore(cfg.AuditoriumID, deps.Store)
	if err != nil {
		return nil, err
	}
	s.sessions = sessions
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
			Handler:           s.calibrationSurface(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return s, nil
}

// slugAuditorium reduces a display name to a stable token-binding id.
func slugAuditorium(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Sessions exposes the per-show session store so the core process can mint a
// session at show start and end it at show end (§10.4).
func (s *Server) Sessions() *SessionStore { return s.sessions }

// EndShow terminates the current show for every connected patron: tokens are
// revoked and live caption/audio connections receive the polite "this show has
// ended" state instead of dangling (§7.2, §9.4).
//
// SEAM: nothing in cmd/captionbridge-core calls this yet. The automatic trigger
// (ACSP Terminate Lease / end of the final reel) lives in the caption controller,
// which this lane does not own; the admin endpoint POST /api/admin/session/end is
// the operator path that exists today.
func (s *Server) EndShow() { s.sessions.EndShow() }

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

// AdminHandler exposes the admin surface (used by tests via httptest).
func (s *Server) AdminHandler() http.Handler { return s.calibrationSurface() }

// patronAssets and adminAssets partition the embedded assets by SURFACE. A file
// must be listed to be served, so a new admin screen cannot reach the patron
// network by being added to the same directory (§7.2, §8.4).
var (
	patronAssets = []string{"patron.html", "patron.css", "patron.js"}
	adminAssets  = []string{
		"admin.html", "admin.css", "admin.js",
		"setup.html", "setup.js",
		"calibrate.html", "calibrate.css", "calibrate.js",
	}
)

// allowFS wraps an fs.FS and serves only an explicit allowlist of names.
// Anything else reports fs.ErrNotExist, so a file cannot reach a surface merely
// by sharing a directory with one that belongs there.
type allowFS struct {
	src     fs.FS
	allowed map[string]bool
}

func (a allowFS) Open(name string) (fs.File, error) {
	if !a.allowed[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return a.src.Open(name)
}

// surfaceFS builds the allowlisted view for one surface. It verifies every named
// asset exists NOW, so a renamed or deleted file breaks startup instead of
// silently 404ing in a booth before a show.
func surfaceFS(src fs.FS, names []string) (fs.FS, error) {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		f, err := src.Open(n)
		if err != nil {
			return nil, fmt.Errorf("embedded asset %s: %w", n, err)
		}
		f.Close()
		allowed[n] = true
	}
	return allowFS{src: src, allowed: allowed}, nil
}
