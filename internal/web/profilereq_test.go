package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/model"
)

func replacementServer(t *testing.T, st model.SystemReadiness) (*Server, *config.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	s, err := NewServer(Config{
		Profile: model.ProfileLowCostReplacement, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{
		Hub: fanout.NewHub(fanout.Options{}), Store: store,
		Status: func() model.SystemReadiness { return st },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}

// §12.4 calls the calibration wizard MANDATORY for this profile. The point of
// routing it through a critical warning is that the same condition stops an
// installer declaring setup complete — a warning nobody has to act on is not a
// requirement.
func TestSetupCannotBeCompletedWithTheProfileRequirementsUnmet(t *testing.T) {
	st := model.SystemReadiness{Warnings: []model.Warning{
		{Code: model.WarnCalibrationRequired, Severity: model.SeverityCritical,
			Message: "calibration required"},
	}}
	s, _ := replacementServer(t, st)

	blocking := s.setupBlockers()
	if len(blocking) == 0 {
		t.Fatal("an uncalibrated replacement-profile appliance can declare setup complete")
	}
	var found bool
	for _, code := range blocking {
		if code == model.WarnCalibrationRequired {
			found = true
		}
	}
	if !found {
		t.Fatalf("the calibration requirement does not block setup: %v", blocking)
	}
}

// The Phase 9 review is a RECORD, not a check, and needs a writer an operator
// can reach — otherwise it is another setting nothing can set.
func TestThePhase9ReviewCanBeRecordedThroughTheAdminAPI(t *testing.T) {
	s, store := replacementServer(t, model.SystemReadiness{})

	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/phase9-review", nil))
	if !strings.Contains(rec.Body.String(), `"recorded":false`) {
		t.Fatalf("a fresh appliance reports a review: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"required":true`) {
		t.Fatalf("this profile does not report the review as required: %s", rec.Body.String())
	}

	post := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/phase9-review",
		strings.NewReader(`{"reference":"OPS-441, reviewed 2026-08-01, J. Okafor"}`))
	req.SetBasicAuth("tech", "pw")
	s.AdminHandler().ServeHTTP(post, req)
	if post.Code != http.StatusOK {
		t.Fatalf("recording the review: %d %s", post.Code, post.Body.String())
	}
	if v, _, _ := store.GetSetting(config.SettingPhase9Review); !strings.Contains(v, "OPS-441") {
		t.Fatalf("the reference was not persisted: %q", v)
	}
}

// It must never present itself as a verification. The appliance cannot know
// whether a legal review happened.
func TestThePhase9RecordNeverClaimsToBeACheck(t *testing.T) {
	s, _ := replacementServer(t, model.SystemReadiness{})
	post := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/phase9-review",
		strings.NewReader(`{"reference":"OPS-441"}`))
	req.SetBasicAuth("tech", "pw")
	s.AdminHandler().ServeHTTP(post, req)

	if !strings.Contains(post.Body.String(), "cannot verify") {
		t.Fatalf("the surface does not say it is a record rather than a check: %s", post.Body.String())
	}
}

// The review record is not on the patron network.
func TestThePhase9RecordIsNotServedToPatrons(t *testing.T) {
	s, _ := replacementServer(t, model.SystemReadiness{})
	rec := httptest.NewRecorder()
	s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/phase9-review", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("the patron listener served the review record")
	}
}
