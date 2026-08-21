package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/update"
)

// stubUpdate stands in for internal/update so this package's tests are about the
// SURFACE rather than about signature checking, which has its own tests.
type stubUpdate struct {
	report update.Report
	result update.VerifyResult
	err    error
	asked  string
}

func (s *stubUpdate) Status() update.Report { return s.report }

func (s *stubUpdate) VerifyFile(name string) (update.VerifyResult, error) {
	s.asked = name
	return s.result, s.err
}

func updateServer(t *testing.T, svc UpdateService) *Server {
	t.Helper()
	srv, err := NewServer(Config{
		Profile: model.ProfilePatronFacing, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Update: svc})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// A bundle that verifies is not the same as one that can be installed here, and
// the surface must carry both facts. Collapsing them would leave a booth waiting
// for an install that cannot happen on an appliance with no A/B slots.
func TestTheSurfaceSeparatesVerifiedFromInstallable(t *testing.T) {
	stub := &stubUpdate{
		report: update.Report{TrustedKeys: 1, ABSupported: false, ABNote: "no A/B slots (BLOCKERS B8)"},
		result: update.VerifyResult{Name: "cb.cbupd", OK: true, KeyID: "vendor", Installable: false},
	}
	s := updateServer(t, stub)

	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/update", nil))
	var rep update.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ABSupported {
		t.Fatal("the surface claims A/B support the appliance does not have")
	}
	if rep.ABNote == "" {
		t.Fatal("nothing tells the operator why an update cannot be installed")
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/update/verify", strings.NewReader(`{"name":"cb.cbupd"}`))
	s.AdminHandler().ServeHTTP(rec, req)
	var res update.VerifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal("a verifying bundle came back not OK")
	}
	if res.Installable {
		t.Fatal("a bundle is reported installable on an appliance with no slots")
	}
}

// A bundle that was read and found wanting is a 200 with OK:false — the
// appliance did its job and the answer is "no". Only a read or naming failure is
// an error status.
func TestARefusedBundleIsAnAnswerNotAnError(t *testing.T) {
	stub := &stubUpdate{result: update.VerifyResult{
		Name: "hostile.cbupd", OK: false, Reason: "signed by a key this appliance does not trust",
	}}
	s := updateServer(t, stub)
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/update/verify",
		strings.NewReader(`{"name":"hostile.cbupd"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a refused bundle returned %d; the verification itself succeeded", rec.Code)
	}
	var res update.VerifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Reason == "" {
		t.Fatalf("the refusal carries no reason: %+v", res)
	}
}

// The update surface names the cinema server's neighbours, the media path and
// the appliance's own build. It is not for the patron network (§8.4).
func TestTheUpdateSurfaceIsNotServedToPatrons(t *testing.T) {
	s := updateServer(t, &stubUpdate{})
	for _, path := range []string{"/api/admin/update", "/api/admin/update/verify"} {
		rec := httptest.NewRecorder()
		s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("the patron listener served %s", path)
		}
	}
}

// An appliance built without the service says so rather than implying an update
// could be verified.
func TestWithNoUpdateServiceTheSurfaceSaysUnavailable(t *testing.T) {
	s := newQRTestServer(t)
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/update", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}
