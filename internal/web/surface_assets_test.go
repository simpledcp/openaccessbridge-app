package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// §7.2: "No admin/diagnostic leakage. The patron UI must never expose cinema
// server IPs, firewall details, logs, caption-asset paths, audio device names,
// or any diagnostics (enforced by route separation and bind-address
// separation)." Both listeners shared ONE asset handler, so the patron bind
// served the admin dashboard's markup and ~29KB of booth diagnostics — including
// the calibration wizard — to the untrusted BYOD network. No measured VALUES
// leaked (the admin APIs are absent from the patron mux), but the surface was.
func TestAdminAssetsAreNotServedOnThePatronListener(t *testing.T) {
	s, err := NewServer(Config{
		Profile: model.ProfileBoothDiagnostic, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
	}, Deps{Hub: fanout.NewHub(fanout.Options{})})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	patron := httptest.NewServer(s.PatronHandler())
	defer patron.Close()
	admin := httptest.NewServer(s.AdminHandler())
	defer admin.Close()

	get := func(base, path string) int {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s%s: %v", base, path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Every admin-surface asset must be absent from the patron listener.
	for _, name := range adminAssets {
		if code := get(patron.URL, "/static/"+name); code != http.StatusNotFound {
			t.Errorf("patron listener served admin asset %s (%d) — §7.2 forbids diagnostics on the patron surface", name, code)
		}
	}
	// And still present on the admin listener, or the dashboard is broken.
	for _, name := range adminAssets {
		if code := get(admin.URL, "/static/"+name); code != http.StatusOK {
			t.Errorf("admin listener does not serve %s (%d)", name, code)
		}
	}
	// The patron assets keep working, on the patron side only.
	for _, name := range patronAssets {
		if code := get(patron.URL, "/static/"+name); code != http.StatusOK {
			t.Errorf("patron listener does not serve %s (%d)", name, code)
		}
		if code := get(admin.URL, "/static/"+name); code != http.StatusNotFound {
			t.Errorf("admin listener served patron asset %s (%d); the split should cut both ways", name, code)
		}
	}
}

// The allowlist is the enforcement, so it must actually cover the directory: a
// new admin screen added to assets/ must be listed deliberately rather than
// reaching the patron network by default.
func TestEverySurfaceAssetIsClassified(t *testing.T) {
	entries, err := assetsFS.ReadDir("assets")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	classified := map[string]bool{}
	for _, n := range patronAssets {
		classified[n] = true
	}
	for _, n := range adminAssets {
		if classified[n] {
			t.Errorf("%s is listed as BOTH a patron and an admin asset", n)
		}
		classified[n] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !classified[e.Name()] {
			t.Errorf("embedded asset %s is classified to neither surface — add it to patronAssets "+
				"or adminAssets so it cannot reach the patron network by default (§7.2)", e.Name())
		}
	}
}
