package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/config"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/security/support"
)

type memTrail struct {
	opened []model.SupportSession
	closed []string
}

func (m *memTrail) RecordSupportOpen(s model.SupportSession) error {
	m.opened = append(m.opened, s)
	return nil
}

func (m *memTrail) RecordSupportClose(id string, _ model.SupportSessionState, _ time.Time) error {
	m.closed = append(m.closed, id)
	return nil
}

func (m *memTrail) OpenSupportSessions() ([]model.SupportSession, error) { return nil, nil }

// newTestStore is a real config store: these tests are about the write→read
// path, so a stub would test nothing.
func newTestStore(t *testing.T) *config.SQLiteStore {
	t.Helper()
	store, err := config.OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// supportServerWithStore wires the manager to the SAME store the admin handler
// writes to, so the endpoint an operator configures is the one a session uses.
func supportServerWithStore(t *testing.T, store *config.SQLiteStore) (*Server, *support.Manager) {
	t.Helper()
	trail := &memTrail{}
	mgr := support.New(support.Options{
		Store: trail,
		Endpoint: func() support.Endpoint {
			host, _, _ := store.GetSetting(config.SettingSupportEndpointHost)
			iface, _, _ := store.GetSetting(config.SettingSupportEgressIface)
			portStr, _, _ := store.GetSetting(config.SettingSupportEndpointPort)
			port, _ := strconv.Atoi(strings.TrimSpace(portStr))
			return support.Endpoint{
				Iface: strings.TrimSpace(iface), Host: strings.TrimSpace(host), Port: port,
			}
		},
	})
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{
		Hub: fanout.NewHub(fanout.Options{}), Support: mgr, Store: store,
		SupportTrail: func(int) ([]model.SupportSession, error) { return trail.opened, nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, mgr
}

func supportServer(t *testing.T, ep support.Endpoint) (*Server, *memTrail) {
	t.Helper()
	trail := &memTrail{}
	mgr := support.New(support.Options{
		Store:    trail,
		Endpoint: func() support.Endpoint { return ep },
	})
	s, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{
		Hub:          fanout.NewHub(fanout.Options{}),
		Support:      mgr,
		SupportTrail: func(int) ([]model.SupportSession, error) { return trail.opened, nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, trail
}

func supportGet(t *testing.T, s *Server) SupportResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/support", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET support: %d %s", rec.Code, rec.Body.String())
	}
	var out SupportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func supportPost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.SetBasicAuth("tech", "pw")
	s.AdminHandler().ServeHTTP(rec, req)
	return rec
}

var liveEndpoint = support.Endpoint{Iface: "eth0", Host: "203.0.113.10", Port: 443}

// §9.4: off by default, and the surface says so before anything is opened.
func TestSupportSurfaceReportsOffByDefault(t *testing.T) {
	s, _ := supportServer(t, liveEndpoint)
	rep := supportGet(t, s)
	if rep.Active {
		t.Fatal("a freshly built appliance reports an active support session")
	}
	if rep.MaxMinutes != int(model.MaxSupportSession/time.Minute) {
		t.Fatalf("the surface advertises a %d-minute ceiling, want %d",
			rep.MaxMinutes, int(model.MaxSupportSession/time.Minute))
	}
}

// An audit entry with no reason is a timestamp.
func TestOpeningWithoutAReasonIsRefused(t *testing.T) {
	s, trail := supportServer(t, liveEndpoint)
	rec := supportPost(t, s, "/api/admin/support", `{"minutes":30}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("opened with no reason: %d %s", rec.Code, rec.Body.String())
	}
	if len(trail.opened) != 0 {
		t.Fatal("a session was recorded despite being refused")
	}
}

// The operator's identity reaches the trail as a HASH, and the raw name never
// appears in what the surface returns.
func TestTheOperatorIsRecordedAsAHashNotAName(t *testing.T) {
	s, trail := supportServer(t, liveEndpoint)
	if rec := supportPost(t, s, "/api/admin/support", `{"reason":"ticket 4","minutes":30}`); rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if len(trail.opened) != 1 {
		t.Fatalf("the trail recorded %d sessions", len(trail.opened))
	}
	got := trail.opened[0]
	if got.OpenedBy == "" || got.OpenedBy == "tech" {
		t.Fatalf("the operator was recorded as %q; it must be a hash", got.OpenedBy)
	}
	rep := supportGet(t, s)
	body, _ := json.Marshal(rep)
	if strings.Contains(string(body), `"tech"`) {
		t.Fatalf("the operator's username is in the response body:\n%s", body)
	}
}

// A window is not a connection. Nothing on this surface may let one read as the
// other: the appliance permits an outbound destination and dials nothing.
func TestTheSurfaceNeverClaimsATunnelIsEstablished(t *testing.T) {
	s, _ := supportServer(t, liveEndpoint)
	supportPost(t, s, "/api/admin/support", `{"reason":"ticket 6","minutes":30}`)
	rep := supportGet(t, s)
	if !rep.Active {
		t.Fatal("the session did not open")
	}
	if rep.TunnelEstablished {
		t.Fatal("the surface claims a support tunnel is established")
	}
	if !strings.Contains(rep.Note, "it does not make one") {
		t.Fatalf("the note does not distinguish permitting from connecting: %q", rep.Note)
	}
	if rep.RemainingSeconds <= 0 {
		t.Fatal("an open window reports no time remaining")
	}
}

// Closing must actually close, and the trail must show it.
func TestClosingEndsTheWindow(t *testing.T) {
	s, trail := supportServer(t, liveEndpoint)
	supportPost(t, s, "/api/admin/support", `{"reason":"ticket 8","minutes":30}`)
	if rec := supportPost(t, s, "/api/admin/support/close", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	if rep := supportGet(t, s); rep.Active {
		t.Fatal("the session is still active after closing")
	}
	if len(trail.closed) != 1 {
		t.Fatalf("the close was not recorded: %+v", trail.closed)
	}
}

// An appliance with no configured endpoint cannot be re-entered at all once SSH
// is off, and the surface must say that rather than offering a switch that
// cannot work.
func TestAnUnconfiguredEndpointIsReportedNotHidden(t *testing.T) {
	s, _ := supportServer(t, support.Endpoint{})
	rep := supportGet(t, s)
	if rep.EndpointConfigured {
		t.Fatal("an unconfigured endpoint reports as configured")
	}
	if !strings.Contains(rep.Note, "no way back into this appliance") {
		t.Fatalf("the surface does not warn that there is no re-entry path: %q", rep.Note)
	}
	if rec := supportPost(t, s, "/api/admin/support", `{"reason":"ticket 9"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("opening without an endpoint returned %d", rec.Code)
	}
}

// The switch that opens a way into the appliance is never on the patron network.
func TestTheSupportSurfaceIsNotServedToPatrons(t *testing.T) {
	s, _ := supportServer(t, liveEndpoint)
	for _, path := range []string{"/api/admin/support", "/api/admin/support/close"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// An appliance built without a manager must say so, not panic and not imply a
// window could be opened.
func TestWithNoManagerTheSurfaceSaysUnavailable(t *testing.T) {
	s := newQRTestServer(t)
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/support", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

// The endpoint needs a writer an operator can reach. Without one, support mode
// could be opened only on an appliance whose database somebody had edited by
// hand — which is how this was found: the live check had to write the settings
// with sqlite3.
func TestTheSupportEndpointCanBeConfiguredThroughTheAdminAPI(t *testing.T) {
	store := newTestStore(t)
	s, mgr := supportServerWithStore(t, store)

	// Nothing configured: opening must be refused.
	if rec := supportPost(t, s, "/api/admin/support", `{"reason":"before"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("opening with no endpoint returned %d", rec.Code)
	}

	rec := supportPost(t, s, "/api/admin/support/endpoint",
		`{"host":"203.0.113.10","port":443,"iface":"eth0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("configuring the endpoint: %d %s", rec.Code, rec.Body.String())
	}
	// And now a session opens, through the same manager the appliance uses.
	if _, err := mgr.Open(support.OpenRequest{Reason: "after", AdminUser: "tech"}); err != nil {
		t.Fatalf("a configured endpoint still refused a session: %v", err)
	}
}

// A hostname is refused where an IP is required: resolving one needs DNS from an
// appliance whose §9.1 checks exist to catch DNS leaks, and a name that resolves
// differently tomorrow is a rule that quietly changes what it permits.
func TestTheSupportEndpointRefusesAHostname(t *testing.T) {
	s, _ := supportServerWithStore(t, newTestStore(t))
	for _, body := range []string{
		`{"host":"support.example.com","port":443,"iface":"eth0"}`,
		`{"host":"203.0.113.10","port":0,"iface":"eth0"}`,
		`{"host":"203.0.113.10","port":443}`,
	} {
		if rec := supportPost(t, s, "/api/admin/support/endpoint", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s was accepted (%d)", body, rec.Code)
		}
	}
}
