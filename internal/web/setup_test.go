package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/model"
)

func newSetupServer(t *testing.T) (*Server, config.Store) {
	t.Helper()
	store, err := config.OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	s, err := NewServer(Config{
		Profile:    model.ProfileLab,
		PatronBind: "0.0.0.0:0",
		AdminBind:  "127.0.0.1:0",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, store
}

func TestSetupWizardFlow(t *testing.T) {
	s, _ := newSetupServer(t)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	post := func(path string, body any, user, pass string) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// First boot: no credentials → setup endpoints open.
	resp := post("/api/admin/setup/credentials", map[string]string{"username": "admin", "password": "longenough1"}, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create credentials: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Now a credential exists → further admin calls require auth.
	resp = post("/api/admin/setup/network", map[string]string{"theaterIface": "eth0", "clientIface": "wlan0"}, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth after credential exists, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// With auth, proceed.
	resp = post("/api/admin/setup/network", map[string]string{"theaterIface": "eth0", "clientIface": "wlan0"}, "admin", "longenough1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("network with auth: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Complete should fail until server + profile are set.
	resp = post("/api/admin/setup/complete", nil, "admin", "longenough1")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("complete should be blocked, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	post("/api/admin/setup/server", model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15", ExpectedScreenName: "Auditorium 3"}, "admin", "longenough1").Body.Close()
	post("/api/admin/setup/profile", map[string]string{"profile": "booth_diagnostic"}, "admin", "longenough1").Body.Close()

	resp = post("/api/admin/setup/complete", nil, "admin", "longenough1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete should succeed once prerequisites met, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// State reflects completion.
	req, _ := http.NewRequest("GET", ts.URL+"/api/admin/setup/state", nil)
	req.SetBasicAuth("admin", "longenough1")
	r2, _ := http.DefaultClient.Do(req)
	var st SetupState
	json.NewDecoder(r2.Body).Decode(&st)
	r2.Body.Close()
	if !st.Complete {
		t.Fatalf("setup should be complete: %+v", st)
	}
}

func TestAdminAuthRequiredAfterCredential(t *testing.T) {
	s, store := newSetupServer(t)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	// Before any credential: /admin is open (first boot).
	r, _ := http.Get(ts.URL + "/admin")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("first-boot /admin should be open, got %d", r.StatusCode)
	}
	r.Body.Close()

	// Create a credential directly.
	hash, _ := config.HashPassword("longenough1")
	store.PutAdminCredential(config.AdminCredential{Username: "admin", PWHash: hash})

	// Now unauthenticated /admin is 401.
	r, _ = http.Get(ts.URL + "/admin")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after credential, got %d", r.StatusCode)
	}
	r.Body.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/admin", nil)
	req.SetBasicAuth("admin", "longenough1")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid auth, got %d", r.StatusCode)
	}
	r.Body.Close()
}

// §8.1 step 14: setup mode exits automatically on timeout. Before this was
// enforced, an appliance that booted and was never finished served the whole
// admin surface — dashboard and diagnostics JSON — unauthenticated forever,
// which in the Lab/Booth profiles can mean on the patron network (§8.4).
func TestSetupModeClosesOnTimeout(t *testing.T) {
	s, _ := newSetupServer(t)
	clock := time.Now()
	s.setup.SetClock(func() time.Time { return clock })
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Inside the window, with no credential yet, the operator can reach setup.
	if code := get("/admin/setup"); code != http.StatusOK {
		t.Fatalf("during setup window: /admin/setup = %d, want 200", code)
	}
	if !s.setup.State().SetupModeActive {
		t.Fatal("setup should report active inside the window")
	}

	// Past the window, an unauthenticated caller gets nothing — not the setup
	// page, and above all not the dashboard or its diagnostics.
	clock = clock.Add(DefaultSetupWindow + time.Second)
	for _, path := range []string{"/admin", "/admin/setup", "/api/admin/status", "/api/admin/setup/state"} {
		if code := get(path); code != http.StatusServiceUnavailable {
			t.Errorf("after the setup window: %s = %d, want 503 (fail closed)", path, code)
		}
	}
	if s.setup.State().SetupModeActive {
		t.Error("setup should not report active past the window")
	}

	// The close is latched: winding the clock back must not reopen it.
	clock = clock.Add(-2 * DefaultSetupWindow)
	if s.setup.Open() {
		t.Error("setup reopened after the window closed; it must require a restart")
	}
	if code := get("/admin"); code != http.StatusServiceUnavailable {
		t.Errorf("/admin = %d after clock rewind, want 503", code)
	}
}

// Completing setup also closes the window (§8.1 step 14, the other half), so a
// credential-less caller cannot slip in afterwards.
func TestSetupModeClosesOnCompletion(t *testing.T) {
	s, store := newSetupServer(t)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	if !s.setup.Open() {
		t.Fatal("setup should start open")
	}
	if err := store.PutSetting(config.SettingSetupComplete, "true"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if s.setup.Open() {
		t.Error("setup must close once complete")
	}

	resp, err := http.Get(ts.URL + "/api/admin/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unauthenticated status after completion = %d, want 503", resp.StatusCode)
	}
}

// §8.1 steps 11-13 run the verification and readiness checks BEFORE setup
// exits, and §8.4 makes it a hard gate: in the locked profiles "an admin
// surface reachable from the patron side refuses to leave setup". Completion
// previously ran no check at all — and stamped §10.8's LastReadinessCheckOK as
// if one had passed, recording a check that never ran.
func TestSetupCompletionIsGatedOnARealCheck(t *testing.T) {
	newServer := func(profile model.FirewallProfile, status func() model.SystemReadiness) (*Server, config.Store) {
		store, err := config.OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		s, err := NewServer(Config{
			Profile: profile, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store, Status: status})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		return s, store
	}

	// Drive the wizard to the point where only the readiness gate is left.
	satisfyPrereqs := func(t *testing.T, ts *httptest.Server) {
		t.Helper()
		post := func(path string, body any) *http.Response {
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.SetBasicAuth("admin", "longenough1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			resp.Body.Close()
			return resp
		}
		b, _ := json.Marshal(map[string]string{"username": "admin", "password": "longenough1"})
		req, _ := http.NewRequest("POST", ts.URL+"/api/admin/setup/credentials", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create credentials: %v", err)
		}
		resp.Body.Close()
		post("/api/admin/setup/network", map[string]string{"TheaterIface": "eth0", "ClientIface": "wlan0"})
		post("/api/admin/setup/server", map[string]string{"auditorium_id": "screen-1", "cinema_server_ip": "10.10.20.15"})
		post("/api/admin/setup/profile", map[string]string{"profile": "patron_facing"})
	}

	complete := func(t *testing.T, ts *httptest.Server) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/api/admin/setup/complete", nil)
		req.SetBasicAuth("admin", "longenough1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("admin on the patron net refuses to leave setup", func(t *testing.T) {
		s, store := newServer(model.ProfilePatronFacing, func() model.SystemReadiness {
			return model.SystemReadiness{AdminIsolated: false}
		})
		ts := httptest.NewServer(s.AdminHandler())
		defer ts.Close()
		satisfyPrereqs(t, ts)

		code, body := complete(t, ts)
		if code != http.StatusPreconditionFailed {
			t.Fatalf("complete = %d, want 412 (§8.4 refuses to leave setup)", code)
		}
		if !strings.Contains(body, model.WarnAdminOnPatronNet) {
			t.Errorf("refusal should name the blocking condition, got %q", body)
		}
		if v, _, _ := store.GetSetting(config.SettingSetupComplete); v == "true" {
			t.Error("setup was marked complete despite the refusal")
		}
	})

	t.Run("a critical warning blocks completion", func(t *testing.T) {
		s, _ := newServer(model.ProfileBoothDiagnostic, func() model.SystemReadiness {
			return model.SystemReadiness{AdminIsolated: true, Warnings: []model.Warning{
				{Code: model.WarnForwardingEnabled, Severity: model.SeverityCritical},
			}}
		})
		ts := httptest.NewServer(s.AdminHandler())
		defer ts.Close()
		satisfyPrereqs(t, ts)

		code, body := complete(t, ts)
		if code != http.StatusPreconditionFailed {
			t.Fatalf("complete = %d, want 412 for a critical warning", code)
		}
		if !strings.Contains(body, model.WarnForwardingEnabled) {
			t.Errorf("refusal should name the critical code, got %q", body)
		}
	})

	t.Run("a clean check completes and does not fabricate a readiness stamp", func(t *testing.T) {
		s, store := newServer(model.ProfilePatronFacing, func() model.SystemReadiness {
			return model.SystemReadiness{AdminIsolated: true}
		})
		ts := httptest.NewServer(s.AdminHandler())
		defer ts.Close()
		satisfyPrereqs(t, ts)

		if code, body := complete(t, ts); code != http.StatusOK {
			t.Fatalf("complete = %d (%s), want 200 on a clean check", code, body)
		}
		if v, _, _ := store.GetSetting(config.SettingSetupComplete); v != "true" {
			t.Error("setup should be complete after a clean check")
		}
		// §10.8's LastReadinessCheckOK records when a check last PASSED. The
		// supervisor writes it from passes it actually ran; pressing Complete
		// must not stamp one on its behalf.
		cfgv, err := store.GetSystemConfig()
		if err != nil {
			t.Fatalf("GetSystemConfig: %v", err)
		}
		if cfgv.LastReadinessCheckOK != nil {
			t.Errorf("completion fabricated a §10.8 readiness stamp (%v) from a check it never ran",
				cfgv.LastReadinessCheckOK)
		}
	})
}
