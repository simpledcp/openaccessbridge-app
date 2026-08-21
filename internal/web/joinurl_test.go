package web

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// shippedBindsServer reproduces the SHIPPED unit exactly:
// --patron-bind=0.0.0.0:80 --admin-bind=127.0.0.1:8443.
func shippedBindsServer(t *testing.T, store *config.SQLiteStore) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "0.0.0.0:80",
		AdminBind: "127.0.0.1:8443", Auditorium: "Auditorium 3",
		AuditoriumID: "aud-3", DefaultLanguage: "en", PerShowToken: true,
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func issueSession(t *testing.T, s *Server) SessionResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/session", strings.NewReader("{}"))
	// The admin reaches the appliance on the loopback bind, as the shipped unit
	// requires.
	req.Host = "127.0.0.1:8443"
	s.AdminHandler().ServeHTTP(rec, req)
	var out SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return out
}

// THE DEFECT THIS TEST EXISTS FOR. On the shipped unit the patron listener binds
// to 0.0.0.0 and admin binds to loopback, so deriving the patron address from the
// ADMIN request's Host produced http://127.0.0.1:80/ — a QR that scans perfectly
// and points every patron's phone at itself.
func TestTheJoinURLIsNeverALoopbackAddress(t *testing.T) {
	s := shippedBindsServer(t, newTestStore(t))
	out := issueSession(t, s)

	if out.JoinURL != "" {
		if strings.Contains(out.JoinURL, "127.0.0.1") || strings.Contains(out.JoinURL, "localhost") ||
			strings.Contains(out.JoinURL, "[::1]") {
			t.Fatalf("the join URL points a patron's phone at itself: %s", out.JoinURL)
		}
	}
	// With no patron interface configured there is nothing to derive, so the
	// honest answer is no URL and no QR — with a reason.
	if out.JoinURL == "" {
		if out.QR != nil {
			t.Fatal("a QR was offered for a join URL that does not exist")
		}
		if out.JoinNote == "" {
			t.Fatal("no join URL and no explanation: a blank space on the operator's screen")
		}
	}
}

// With the §9.1 client interface configured, the join URL is that interface's
// address — the patron LAN by definition, and the same interface D28 confines
// ICE gathering to.
func TestTheJoinURLUsesTheConfiguredPatronInterface(t *testing.T) {
	iface, addr := firstRoutableIfaceAddr(t)
	store := newTestStore(t)
	if err := store.PutSetting(config.SettingClientInterface, iface); err != nil {
		t.Fatal(err)
	}
	s := shippedBindsServer(t, store)
	out := issueSession(t, s)

	if out.JoinURL == "" {
		t.Fatalf("no join URL with %q configured as the patron interface: %s", iface, out.JoinNote)
	}
	if !strings.Contains(out.JoinURL, addr) {
		t.Fatalf("the join URL %q does not use the patron interface's address %s", out.JoinURL, addr)
	}
	if out.QR == nil {
		t.Fatal("a reachable join URL produced no QR")
	}
}

// A QR that would encode an unreachable address must not be served at all. A
// code that scans and goes nowhere is worse than no code: nobody looks for a
// fault, because the scan worked.
func TestNoQRIsServedWhenNoAddressCanBeDetermined(t *testing.T) {
	s := shippedBindsServer(t, newTestStore(t))
	issueSession(t, s)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/session/qr.png", nil)
	req.Host = "127.0.0.1:8443"
	s.AdminHandler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("a QR was served for an address no phone could reach")
	}
	if !strings.Contains(rec.Body.String(), "patron interface") {
		t.Fatalf("the refusal does not say how to fix it: %s", rec.Body.String())
	}
}

// A non-loopback admin Host is still usable — that is the Lab convenience the
// fallback exists for, and it is kept.
func TestANonLoopbackAdminHostIsStillUsable(t *testing.T) {
	s := shippedBindsServer(t, newTestStore(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/session", strings.NewReader("{}"))
	req.Host = "192.168.44.9:8443"
	s.AdminHandler().ServeHTTP(rec, req)
	var out SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.JoinURL, "192.168.44.9:80") {
		t.Fatalf("a reachable admin host was not used: %q (%s)", out.JoinURL, out.JoinNote)
	}
}

func firstRoutableIfaceAddr(t *testing.T) (string, string) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces")
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP == nil || ipnet.IP.To4() == nil {
				continue
			}
			if ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			return i.Name, ipnet.IP.String()
		}
	}
	t.Skip("no routable IPv4 interface on this host")
	return "", ""
}
