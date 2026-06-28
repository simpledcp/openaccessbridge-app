package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
