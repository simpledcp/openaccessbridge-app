package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/logging/debugcapture"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

func captureServer(t *testing.T) (*Server, *debugcapture.Manager) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// RequireTmpfs off: a temp dir on this host is not tmpfs, and the constraint
	// itself is the debugcapture package's own test to make.
	mgr := debugcapture.New(debugcapture.Options{Dir: dir})
	t.Cleanup(mgr.Close)
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), DebugCapture: mgr})
	if err != nil {
		t.Fatal(err)
	}
	return s, mgr
}

func capturePost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.SetBasicAuth("tech", "pw")
	s.AdminHandler().ServeHTTP(rec, req)
	return rec
}

// §9.4 offers debug capture as the SANCTIONED trade against the no-content rule.
// The manager implemented every constraint and had no production caller, so the
// sanctioned way did not exist — which makes the unsanctioned ways the only ways.
func TestACaptureCanBeStartedAndStoppedByAnAdmin(t *testing.T) {
	s, mgr := captureServer(t)

	if mgr.Active() {
		t.Fatal("a capture is running on a fresh appliance")
	}
	rec := capturePost(t, s, "/api/admin/debug-capture",
		`{"kind":"caption","seconds":60,"reason":"patron reports captions one line late"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("starting a capture: %d %s", rec.Code, rec.Body.String())
	}
	if !mgr.Active() {
		t.Fatal("the admin route reported success and nothing is capturing")
	}

	if rec := capturePost(t, s, "/api/admin/debug-capture/stop", `{"kind":"caption"}`); rec.Code != http.StatusOK {
		t.Fatalf("stopping: %d %s", rec.Code, rec.Body.String())
	}
	if mgr.Active() {
		t.Fatal("the capture survived being stopped")
	}
}

// §8.3: an active capture must be visibly indicated for as long as it runs. The
// manager produced this warning and nothing consumed it.
func TestAnActiveCaptureRaisesTheDashboardAlert(t *testing.T) {
	s, mgr := captureServer(t)
	if len(mgr.Warnings()) != 0 {
		t.Fatal("an idle appliance is warning about a capture")
	}
	capturePost(t, s, "/api/admin/debug-capture", `{"kind":"caption","seconds":60,"reason":"ticket 12"}`)

	warnings := mgr.Warnings()
	if len(warnings) == 0 {
		t.Fatal("a running capture raises no warning, so it cannot be visibly indicated")
	}
	if warnings[0].Code != model.WarnDebugCaptureActive {
		t.Fatalf("the alert uses code %q", warnings[0].Code)
	}
}

// The window is bounded whatever is asked for: §9.4 says time-limited, and a
// limit an operator can raise indefinitely is not one.
func TestTheCaptureWindowIsClamped(t *testing.T) {
	s, _ := captureServer(t)
	rec := capturePost(t, s, "/api/admin/debug-capture",
		`{"kind":"caption","seconds":86400,"reason":"long one"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out DebugCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Active) != 1 {
		t.Fatalf("expected one active capture, got %d", len(out.Active))
	}
	got := out.Active[0].ExpiresAt.Sub(out.Active[0].StartedAt)
	if got > debugcapture.DefaultMaxTTL {
		t.Fatalf("a day-long request produced a %s window; the ceiling is %s", got, debugcapture.DefaultMaxTTL)
	}
}

// An unknown kind is refused rather than silently capturing the wrong thing.
func TestAnUnknownCaptureKindIsRefused(t *testing.T) {
	s, mgr := captureServer(t)
	if rec := capturePost(t, s, "/api/admin/debug-capture", `{"kind":"everything","seconds":60}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown kind returned %d", rec.Code)
	}
	if mgr.Active() {
		t.Fatal("a refused request started a capture anyway")
	}
}

// Capturing caption text or audio is the most sensitive thing this appliance can
// be asked to do. It is never on the patron listener (§8.4).
func TestDebugCaptureIsNotOnThePatronListener(t *testing.T) {
	s, _ := captureServer(t)
	for _, path := range []string{"/api/admin/debug-capture", "/api/admin/debug-capture/stop"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// An appliance with no sink says so, rather than implying a capture is possible.
func TestWithNoSinkTheSurfaceSaysUnavailable(t *testing.T) {
	s := newQRTestServer(t)
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/debug-capture", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}
