package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

func serverWithStatus(t *testing.T, st model.SystemReadiness) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile:         model.ProfilePatronFacing,
		PatronBind:      "192.168.44.1:8080",
		AdminBind:       "127.0.0.1:0",
		Auditorium:      "Auditorium 3",
		AuditoriumID:    "aud-3",
		DefaultLanguage: "en",
		PerShowToken:    true,
	}, Deps{
		Hub:    fanout.NewHub(fanout.Options{}),
		Status: func() model.SystemReadiness { return st },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func goodStatus() model.SystemReadiness {
	return model.SystemReadiness{
		ForwardingDisabled: true, FirewallActive: true, FirewallIntrospected: true,
		// Measured, and nothing listening: the state a healthy appliance is in
		// (§9.3/D37 — an UNMEASURED SSH row now blocks, as it should).
		SSHDisabled: true, SSHIntrospected: true,
		AdminOnPatronNet: model.TriNo, IdentityCheck: "match",
		DeploymentProfile: model.ProfilePatronFacing,
		CaptionLink: model.CaptionLinkState{
			LeaseActive: true, RPLLoaded: true, AssetsLoaded: true,
			RequestIntervalMS: 30000, AvailableLanguages: []string{"en"},
		},
		AudioStreams: []model.AudioStreamSummary{
			{StreamType: model.StreamHI, InputPresent: model.TriYes, StreamActive: model.TriYes, Quality: model.QualExcellent},
			{StreamType: model.StreamVIN, InputPresent: model.TriYes, StreamActive: model.TriYes, Quality: model.QualUsable},
		},
		ICEScope: model.ICEScopeReport{Configured: true, Interface: "wlan0", InForce: true},
	}
}

// §11.5 asks for a codified STAFF procedure. A JSON document in a browser is not
// that; the sheet has to print, go in a binder, and be readable when the thing
// that is broken is the appliance.
func TestTheChecklistHasAPrintableForm(t *testing.T) {
	s := serverWithStatus(t, goodStatus())
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/preshow?format=text", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("printable form served as %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"pre-show checklist",
		"Auditorium 3",                         // §8.5: the sheet names its room
		"STAFF STEP",                           // the items the appliance cannot answer
		"BLOCKING",                             // which items stop a house opening
		"CANNOT BE CHECKED",                    // the unmeasurable latency line
		"does not know whether they were done", // the honesty line
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the printable sheet never says %q:\n%s", want, body)
		}
	}
}

// The appliance's own verdict must never be presentable as "the room is ready".
func TestTheSheetNeverClaimsTheRoomIsReady(t *testing.T) {
	s := serverWithStatus(t, goodStatus())
	post(t, s.AdminHandler(), "/api/admin/session", "{}")

	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/preshow", nil))
	var cl model.PreShowChecklist
	if err := json.Unmarshal(rec.Body.Bytes(), &cl); err != nil {
		t.Fatal(err)
	}
	if !cl.ApplianceReady {
		t.Fatalf("a healthy appliance is not ready: %v", cl.BlockingProblems)
	}
	if cl.StaffStepsOutstanding == 0 {
		t.Fatal("no staff steps are outstanding: the appliance is vouching for the seats")
	}
	// The printable form must carry the same caveat in words.
	text := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(text, httptest.NewRequest(http.MethodGet, "/api/admin/preshow?format=text", nil))
	if strings.Contains(text.Body.String(), "READY FOR SHOW") {
		t.Fatal("the sheet claims the show is ready; it can only speak for the appliance")
	}
}

// With no link to the supervisor the sheet must say so rather than deriving a
// page of failures from a zero value — which would send a booth chasing faults
// that were never measured.
func TestWithNoStatusLinkTheSheetSaysSoRatherThanInventingFailures(t *testing.T) {
	s := newQRTestServer(t) // built without a Status dep
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/preshow", nil))
	var cl model.PreShowChecklist
	if err := json.Unmarshal(rec.Body.Bytes(), &cl); err != nil {
		t.Fatal(err)
	}
	if len(cl.Checks) != 1 || cl.Checks[0].ID != "status_unavailable" {
		t.Fatalf("expected a single 'nothing could be measured' item, got %d items", len(cl.Checks))
	}
	if cl.ApplianceReady {
		t.Fatal("an appliance that measured nothing reports itself ready")
	}
}

// The sheet names the cinema server, the interface assignment and the room's
// faults. None of that belongs on the patron network (§8.4).
func TestTheChecklistIsNotServedToPatrons(t *testing.T) {
	s := serverWithStatus(t, goodStatus())
	for _, path := range []string{"/api/admin/preshow", "/api/admin/preshow?format=text"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// A failing blocking item must print its remedy where a person will see it.
func TestAFailingItemPrintsItsRemedy(t *testing.T) {
	st := goodStatus()
	st.CaptionLink.LeaseActive = false
	s := serverWithStatus(t, st)

	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/preshow?format=text", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "FAIL") {
		t.Fatalf("a dead lease did not print as a failure:\n%s", body)
	}
	if !strings.Contains(body, "-> ") {
		t.Fatalf("no remedy was printed under the failing item:\n%s", body)
	}
	if !strings.Contains(body, "NOT READY") {
		t.Fatalf("the summary does not say the appliance is not ready:\n%s", body)
	}
}
