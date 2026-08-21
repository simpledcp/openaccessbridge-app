package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/qr"
)

// §10.4 / §7.2: "a per-show QR encodes the local URL + session token", so a
// patron joins in one scan. The image must carry the SAME payload the operator
// is shown as text — a QR that disagrees with the printed URL is worse than no
// QR, because the two cannot both be checked at the door.
func TestTheQRCarriesTheJoinURLItselfNotSomethingElse(t *testing.T) {
	s := newQRTestServer(t)
	admin := s.AdminHandler()

	issue := httptest.NewRecorder()
	admin.ServeHTTP(issue, httptest.NewRequest(http.MethodPost, "/api/admin/session", strings.NewReader("{}")))
	if issue.Code != http.StatusOK {
		t.Fatalf("issuing a session: %d %s", issue.Code, issue.Body.String())
	}
	var resp SessionResponse
	if err := json.Unmarshal(issue.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.JoinURL == "" {
		t.Fatal("no join URL was issued")
	}
	if resp.QR == nil {
		t.Fatal("a session was issued with no QR description: a patron would have to type the code")
	}

	// The description must match a symbol actually built from that join URL.
	sym, err := qr.Encode([]byte(resp.JoinURL), qr.DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	if resp.QR.Version != sym.Version || resp.QR.Modules != sym.Modules() {
		t.Fatalf("the QR is described as version %d / %d modules; the join URL encodes to version %d / %d",
			resp.QR.Version, resp.QR.Modules, sym.Version, sym.Modules())
	}
	if resp.QR.PrintWidthMM <= 0 {
		t.Fatal("no print size was given, so a booth has nothing to size a card from")
	}

	// And the served image must be that symbol, module for module.
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, resp.QR.URL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching the QR: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("QR served as %q", ct)
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the appliance served a PNG nothing can decode: %v", err)
	}
	scale := img.Bounds().Dx() / sym.Modules()
	if scale < 1 {
		t.Fatalf("image is %d px for a %d-module symbol", img.Bounds().Dx(), sym.Modules())
	}
	for y := 0; y < sym.Size; y++ {
		for x := 0; x < sym.Size; x++ {
			px := (x+qr.QuietZone)*scale + scale/2
			py := (y+qr.QuietZone)*scale + scale/2
			r, _, _, _ := img.At(px, py).RGBA()
			if (r == 0) != sym.Dark(x, y) {
				t.Fatalf("the served image differs from the join URL's symbol at module (%d,%d)", x, y)
			}
		}
	}
}

// §8.5's failure mode is a STALE QR admitting patrons to the wrong room. A cached
// image is exactly that, so the response must forbid caching.
func TestTheQRIsNeverCached(t *testing.T) {
	s := newQRTestServer(t)
	admin := s.AdminHandler()
	post(t, admin, "/api/admin/session", "{}")

	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/session/qr.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching the QR: %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("the per-show QR is cacheable (Cache-Control: %q): yesterday's code could admit patrons today", cc)
	}
}

// With no live show there must be no image at all. A blank or stale PNG held up
// to a patron's phone is indistinguishable from a broken scanner.
func TestWithNoShowThereIsNoQR(t *testing.T) {
	s := newQRTestServer(t)
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/session/qr.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("with no session live the QR route returned %d, want 404", rec.Code)
	}
}

// The QR is a bearer credential for the show: whatever can read it can join. It
// belongs on the admin listener only (§8.4), never on the patron network.
func TestTheQRIsNotServedOnThePatronListener(t *testing.T) {
	s := newQRTestServer(t)
	post(t, s.AdminHandler(), "/api/admin/session", "{}")

	for _, path := range []string{"/api/admin/session/qr.png", "/api/admin/session"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// A scale the appliance will not render is refused rather than silently changed,
// and the refusal cannot be used to ask for an enormous allocation.
func TestAnImpossibleQRScaleIsRefused(t *testing.T) {
	s := newQRTestServer(t)
	admin := s.AdminHandler()
	post(t, admin, "/api/admin/session", "{}")

	for _, q := range []string{"?scale=0", "?scale=-3", "?scale=99999", "?scale=abc"} {
		rec := httptest.NewRecorder()
		admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/session/qr.png"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("scale query %q returned %d, want 400", q, rec.Code)
		}
	}
}

// Issuing a new show must change the QR, because the token inside it changed.
// A QR that survived the show boundary would be the §9.4 carry-over the session
// model forbids.
func TestANewShowGetsANewQR(t *testing.T) {
	s := newQRTestServer(t)
	admin := s.AdminHandler()

	post(t, admin, "/api/admin/session", `{"show_id":"first"}`)
	first := fetch(t, admin, "/api/admin/session/qr.png")
	post(t, admin, "/api/admin/session", `{"show_id":"second"}`)
	second := fetch(t, admin, "/api/admin/session/qr.png")

	if bytes.Equal(first, second) {
		t.Fatal("a new show served the identical QR image: the previous show's token is still being handed out")
	}
}

// newQRTestServer builds a server with a patron bind that has a real address, so
// the join URL a QR encodes is the one a phone would be given rather than a
// wildcard.
func newQRTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile:         model.ProfileBoothDiagnostic,
		PatronBind:      "192.168.44.1:8080",
		AdminBind:       "127.0.0.1:0",
		Auditorium:      "Auditorium 3",
		AuditoriumID:    "aud-3",
		DefaultLanguage: "en",
		PerShowToken:    true,
	}, Deps{Hub: fanout.NewHub(fanout.Options{})})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func post(t *testing.T, h http.Handler, path, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
	}
}

func fetch(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
