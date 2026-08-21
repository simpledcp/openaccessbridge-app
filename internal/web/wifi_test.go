package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/qr"
)

// syncLogBuffer is a concurrency-safe log sink.
type syncLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncLogBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncLogBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func wifiServer(t *testing.T) (*Server, *config.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}

func wifiGet(t *testing.T, s *Server) WiFiResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/wifi", nil))
	var out WiFiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return out
}

func wifiPost(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/wifi",
		strings.NewReader(body)))
	return rec
}

// §9.5's onboarding code, end to end: configure the network, get a QR that
// carries exactly the payload a phone needs.
func TestThePatronNetworkCodeCarriesTheConfiguredNetwork(t *testing.T) {
	s, _ := wifiServer(t)
	if rep := wifiGet(t, s); rep.Configured {
		t.Fatal("an unconfigured appliance reports a joinable network")
	}

	if rec := wifiPost(t, s, `{"ssid":"CinemaPatron","password":"popcorn2026","security":"WPA"}`); rec.Code != http.StatusOK {
		t.Fatalf("configuring: %d %s", rec.Code, rec.Body.String())
	}
	rep := wifiGet(t, s)
	if !rep.Configured || rep.QRURL == "" {
		t.Fatalf("the network did not become joinable: %+v", rep)
	}

	// The image must be the payload for THAT network, module for module.
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rep.QRURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching the code: %d", rec.Code)
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the appliance served a PNG nothing can decode: %v", err)
	}
	want, err := qr.Encode([]byte(qr.WiFiPayload(qr.WiFiNetwork{
		SSID: "CinemaPatron", Password: "popcorn2026", Security: qr.WiFiWPA,
	})), qr.DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	scale := img.Bounds().Dx() / want.Modules()
	for y := 0; y < want.Size; y++ {
		for x := 0; x < want.Size; x++ {
			px := (x+qr.QuietZone)*scale + scale/2
			py := (y+qr.QuietZone)*scale + scale/2
			r, _, _, _ := img.At(px, py).RGBA()
			if (r == 0) != want.Dark(x, y) {
				t.Fatalf("the served code differs from the configured network at (%d,%d)", x, y)
			}
		}
	}
}

// The passphrase goes into the CODE, because a phone needs it. It does not go
// into the status document a dashboard polls: those are different exposures.
func TestThePassphraseIsNotInTheStatusDocument(t *testing.T) {
	s, _ := wifiServer(t)
	wifiPost(t, s, `{"ssid":"CinemaPatron","password":"popcorn2026","security":"WPA"}`)

	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/wifi", nil))
	if strings.Contains(rec.Body.String(), "popcorn2026") {
		t.Fatalf("the passphrase is in the status document:\n%s", rec.Body.String())
	}
	if !wifiGet(t, s).PasswordSet {
		t.Fatal("the surface does not report that a passphrase is on file")
	}
}

// A secured network with no passphrase would produce a code that fails at the
// moment a patron scans it — the worst place to discover it.
func TestASecuredNetworkWithNoPassphraseIsRefused(t *testing.T) {
	s, _ := wifiServer(t)
	rec := wifiPost(t, s, `{"ssid":"CinemaPatron","security":"WPA"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a WPA network with no passphrase was accepted: %d", rec.Code)
	}
}

// Clearing the network clears the passphrase with it: a stored credential for a
// network that is no longer configured is the stale secret nobody remembers.
func TestClearingTheNetworkClearsThePassphrase(t *testing.T) {
	s, store := wifiServer(t)
	wifiPost(t, s, `{"ssid":"CinemaPatron","password":"popcorn2026","security":"WPA"}`)
	if rec := wifiPost(t, s, `{"ssid":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing: %d %s", rec.Code, rec.Body.String())
	}
	pw, _, _ := store.GetSetting(config.SettingPatronWiFiPassword)
	if pw != "" {
		t.Fatalf("the passphrase survived the network being cleared: %q", pw)
	}
	if wifiGet(t, s).Configured {
		t.Fatal("a cleared network still reports as joinable")
	}
}

// The network code and the passphrase behind it are never on the patron
// listener: a patron already on the network does not need it, and anyone who is
// not should not get it from the appliance (§8.4).
func TestTheNetworkCodeIsNotServedToPatrons(t *testing.T) {
	s, _ := wifiServer(t)
	wifiPost(t, s, `{"ssid":"CinemaPatron","password":"popcorn2026","security":"WPA"}`)
	for _, path := range []string{"/api/admin/wifi", "/api/admin/wifi-qr.png"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// §9.4: the passphrase must never reach a log, even though the SSID does.
func TestThePassphraseNeverReachesTheLog(t *testing.T) {
	store := newTestStore(t)
	var logs syncLogBuffer
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store,
		Log: logging.New(logging.Options{Component: "test", JSON: true, Writer: &logs})})
	if err != nil {
		t.Fatal(err)
	}
	wifiPost(t, s, `{"ssid":"CinemaPatron","password":"popcorn2026","security":"WPA"}`)
	out := logs.String()
	if strings.Contains(out, "popcorn2026") {
		t.Fatalf("the patron network passphrase reached the log:\n%s", out)
	}
	if !strings.Contains(out, "CinemaPatron") {
		t.Fatalf("the SSID — operational configuration — was not logged:\n%s", out)
	}
}

// An all-hex network is FLAGGED rather than silently guessed at, because both
// available fixes break for somebody and the venue has a third option.
func TestAnAllHexNetworkIsFlaggedToTheOperator(t *testing.T) {
	s, _ := wifiServer(t)
	wifiPost(t, s, `{"ssid":"CinemaPatron","password":"1234567890abcdef","security":"WPA"}`)
	rep := wifiGet(t, s)
	if !rep.HexAmbiguity {
		t.Fatal("an all-hex passphrase was not flagged")
	}
	if !strings.Contains(rep.Note, "non-hex") {
		t.Fatalf("the note does not tell the venue how to remove the ambiguity: %q", rep.Note)
	}
}
