package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/support"
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
