package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// DefaultSetupWindow is how long first-boot setup mode stays open (§8.1 step 14).
const DefaultSetupWindow = 30 * time.Minute

// SetupManager drives the first-boot setup workflow (§8.1). It enforces "no
// default credentials past setup" and the auto-exit on completion/timeout.
type SetupManager struct {
	store   config.Store
	log     *logging.Logger
	profile model.FirewallProfile

	mu       sync.Mutex
	openedAt time.Time
	window   time.Duration
	closed   bool // latched once the window elapses or setup completes
	now      func() time.Time
}

// NewSetupManager builds a SetupManager. The setup window starts now.
func NewSetupManager(store config.Store, log *logging.Logger, profile model.FirewallProfile, window time.Duration) *SetupManager {
	if log == nil {
		log = logging.Nop()
	}
	if window <= 0 {
		window = DefaultSetupWindow
	}
	return &SetupManager{
		store: store, log: log, profile: profile,
		openedAt: time.Now(), window: window, now: time.Now,
	}
}

// SetClock injects a clock for tests. The setup window is a security boundary,
// so it must be testable without sleeping for the real 30 minutes.
func (s *SetupManager) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
	s.openedAt = now()
}

// Open reports whether setup mode is still accepting an unauthenticated
// operator, and latches it closed the first time it is not (§8.1 step 14: exit
// setup mode automatically on completion OR timeout — no lingering open setup
// network). Once closed it stays closed for the life of the process: reopening
// requires a restart, which on the appliance means physical or console access.
func (s *SetupManager) Open() bool {
	complete := false
	if v, _, _ := s.store.GetSetting(config.SettingSetupComplete); v == "true" {
		complete = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	reason := ""
	switch {
	case complete:
		reason = "completed"
	case s.now().Sub(s.openedAt) >= s.window:
		reason = "timeout"
	}
	if reason == "" {
		return true
	}
	s.closed = true
	// Log the close exactly once — it is the moment the admin surface stops
	// being reachable without credentials.
	s.log.Info(logging.EvStateTransition,
		logging.Field("phase", "setup_mode_closed"),
		logging.Field("reason", reason))
	return false
}

// SetupState is the wizard's progress, rendered by the setup page.
type SetupState struct {
	Complete             bool   `json:"complete"`
	HasCredentials       bool   `json:"has_credentials"`
	HasDefaultCredential bool   `json:"has_default_credential"`
	NetworkConfigured    bool   `json:"network_configured"`
	ServerConfigured     bool   `json:"server_configured"`
	ProfileSelected      bool   `json:"profile_selected"`
	SetupModeActive      bool   `json:"setup_mode_active"`
	ExpiresInSec         int    `json:"expires_in_sec"`
	Profile              string `json:"profile"`
}

// State computes the current setup state.
func (s *SetupManager) State() SetupState {
	st := SetupState{Profile: string(s.profile)}
	if v, _, _ := s.store.GetSetting(config.SettingSetupComplete); v == "true" {
		st.Complete = true
	}
	st.HasCredentials, _ = s.store.HasAnyAdminCredential()
	st.HasDefaultCredential, _ = s.store.HasDefaultCredential()
	if v, ok, _ := s.store.GetSetting(config.SettingTheaterInterface); ok && v != "" {
		if v2, ok2, _ := s.store.GetSetting(config.SettingClientInterface); ok2 && v2 != "" {
			st.NetworkConfigured = true
		}
	}
	if profiles, _ := s.store.ListAuditoriumProfiles(); len(profiles) > 0 {
		for _, p := range profiles {
			if p.CinemaServerIP != "" {
				st.ServerConfigured = true
				break
			}
		}
	}
	if v, ok, _ := s.store.GetSetting(config.SettingActiveProfile); ok && v != "" {
		st.ProfileSelected = true
	}

	// Ask Open() rather than recomputing the deadline, so the countdown the page
	// renders and the boundary requireAdmin enforces can never disagree.
	if st.SetupModeActive = s.Open(); st.SetupModeActive {
		s.mu.Lock()
		remaining := s.window - s.now().Sub(s.openedAt)
		s.mu.Unlock()
		st.ExpiresInSec = int(remaining.Seconds())
	}
	return st
}

// canComplete reports whether the wizard's prerequisites are met (§8.1).
func (st SetupState) canComplete() (bool, string) {
	switch {
	case !st.HasCredentials:
		return false, "admin credentials not created"
	case st.HasDefaultCredential:
		return false, "a default credential must be replaced before leaving setup"
	case !st.NetworkConfigured:
		return false, "theater/client interfaces not assigned"
	case !st.ServerConfigured:
		return false, "cinema server allowlist not configured"
	case !st.ProfileSelected:
		return false, "firewall profile not selected"
	default:
		return true, ""
	}
}

// --- HTTP handlers (mounted on the admin mux) ---

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.setup.State())
}

func (s *Server) handleSetupCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || len(body.Password) < 8 {
		http.Error(w, "username required and password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	hash, err := config.HashPassword(body.Password)
	if err != nil {
		http.Error(w, "could not set password", http.StatusInternalServerError)
		return
	}
	// Force a real credential — never a lingering default (§8.1).
	if err := s.deps.Store.PutAdminCredential(config.AdminCredential{Username: body.Username, PWHash: hash, MustChange: false}); err != nil {
		http.Error(w, "could not save credential", http.StatusInternalServerError)
		return
	}
	s.deps.Log.Info(logging.EvStateTransition, logging.Field("phase", "admin_credential_created"))
	writeJSON(w, s.setup.State())
}

func (s *Server) handleSetupNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct{ TheaterIface, ClientIface string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.TheaterIface == "" || body.ClientIface == "" {
		http.Error(w, "both interfaces required", http.StatusBadRequest)
		return
	}
	if body.TheaterIface == body.ClientIface {
		http.Error(w, "theater and client interfaces must differ", http.StatusBadRequest)
		return
	}
	_ = s.deps.Store.PutSetting(config.SettingTheaterInterface, body.TheaterIface)
	_ = s.deps.Store.PutSetting(config.SettingClientInterface, body.ClientIface)
	writeJSON(w, s.setup.State())
}

func (s *Server) handleSetupServer(w http.ResponseWriter, r *http.Request) {
	var p model.AuditoriumProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.AuditoriumID == "" || p.CinemaServerIP == "" {
		http.Error(w, "auditorium id and cinema server ip required", http.StatusBadRequest)
		return
	}
	if p.ACSPInitiation == "" {
		p.ACSPInitiation = model.ACSInitiated // Variant A default (D6)
	}
	if p.IdentityStrength == "" {
		p.IdentityStrength = model.IdentityComposite
	}
	if err := s.deps.Store.PutAuditoriumProfile(p); err != nil {
		http.Error(w, "could not save profile", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.setup.State())
}

func (s *Server) handleSetupProfile(w http.ResponseWriter, r *http.Request) {
	var body struct{ Profile string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch model.FirewallProfile(body.Profile) {
	case model.ProfileLab, model.ProfileBoothDiagnostic, model.ProfilePatronFacing, model.ProfileLowCostReplacement:
	default:
		http.Error(w, "unknown profile", http.StatusBadRequest)
		return
	}
	_ = s.deps.Store.PutSetting(config.SettingActiveProfile, body.Profile)
	writeJSON(w, s.setup.State())
}

func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	st := s.setup.State()
	if ok, reason := st.canComplete(); !ok {
		http.Error(w, reason, http.StatusPreconditionFailed)
		return
	}
	// §8.1 steps 11-13 run the firewall/network verification and the readiness
	// checks BEFORE setup exits, and §8.4 makes that a hard gate: in the locked
	// profiles "an admin surface reachable from the patron side refuses to leave
	// setup". Consult the readiness snapshot the supervisor assembles from passes
	// that actually ran, rather than declaring success on the operator's behalf.
	if blocking := s.setupBlockers(); len(blocking) > 0 {
		s.deps.Log.Warn(logging.EvStateTransition,
			logging.Field("phase", "setup_complete_refused"),
			logging.Field("blocking", strings.Join(blocking, ",")))
		http.Error(w, "cannot leave setup until these are resolved: "+strings.Join(blocking, ", "),
			http.StatusPreconditionFailed)
		return
	}

	_ = s.deps.Store.PutSetting(config.SettingSetupComplete, "true")
	// §10.8's LastReadinessCheckOK is deliberately NOT stamped here. It records
	// when a readiness check last PASSED, and the supervisor writes it from the
	// verification passes it actually runs; stamping it on the operator pressing
	// Complete would record a check that never ran — and would race the
	// supervisor's own read-modify-write of the same row.
	s.deps.Log.Info(logging.EvStateTransition, logging.Field("phase", "setup_complete"))
	writeJSON(w, s.setup.State())
}

// setupBlockers lists the §8.3 warning codes that must clear before setup may
// close. It returns codes, not prose, so the list is stable for the UI and
// carries nothing sensitive into the log (§9.4).
//
// With no Status dependency wired there is nothing to consult, and the gate
// stays open rather than blocking an operator on a check the build cannot run;
// the shipped core always wires it.
func (s *Server) setupBlockers() []string {
	if s.deps.Status == nil {
		return nil
	}
	st := s.deps.Status()

	var blocking []string
	seen := map[string]bool{}
	add := func(code string) {
		if code != "" && !seen[code] {
			seen[code] = true
			blocking = append(blocking, code)
		}
	}

	// The §8.4 hard check applies to the locked profiles, and to an unrecognized
	// one (model.FirewallProfile.Production treats unknown as production).
	if s.cfg.Profile.Production() && !st.AdminIsolated {
		add(model.WarnAdminOnPatronNet)
	}
	// Every critical condition blocks regardless of profile: leaving setup is
	// the moment the appliance claims it is ready for a show.
	for _, warn := range st.Warnings {
		if warn.Severity == model.SeverityCritical {
			add(warn.Code)
		}
	}
	return blocking
}

// requireAdmin authenticates admin requests (§8.4). On first boot (no credential
// yet) it permits access so the operator can create one — but ONLY while setup
// mode is open. Past the window the surface fails CLOSED: an appliance that
// booted and was never finished must not go on serving the admin dashboard and
// its diagnostics (server IPs, interface assignment, firewall state) to anyone
// who can reach the port, which in the Lab and Booth Diagnostic profiles can be
// the patron network itself (§8.1 step 14, §8.4).
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Store == nil {
			next(w, r) // no store configured (e.g. tests) → open
			return
		}
		hasCred, _ := s.deps.Store.HasAnyAdminCredential()
		if !hasCred {
			if s.setup == nil || s.setup.Open() {
				next(w, r) // first boot: allow until a credential is created
				return
			}
			// No credential and setup is over: there is nothing to authenticate
			// against, so say so plainly instead of pretending a login exists.
			http.Error(w, "setup mode has closed and no administrator credential was created — "+
				"restart the appliance to reopen setup", http.StatusServiceUnavailable)
			return
		}
		user, pass, ok := r.BasicAuth()
		if ok {
			if cred, found, _ := s.deps.Store.GetAdminCredential(user); found && config.VerifyPassword(cred.PWHash, pass) {
				next(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="CaptionBridge Admin"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}
