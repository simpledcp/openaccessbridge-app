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

func namedServer(t *testing.T, store *config.SQLiteStore, flagName string) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile: model.ProfileBoothDiagnostic, PatronBind: "192.168.44.1:8080",
		AdminBind: "127.0.0.1:0", Auditorium: flagName, DefaultLanguage: "en",
	}, Deps{
		Hub: fanout.NewHub(fanout.Options{}), Store: store,
		AuditoriumName: func() string {
			name, _, _ := store.GetSetting(config.SettingAuditoriumName)
			return name
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func patronPage(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.PatronHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accessibility", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("patron page: %d", rec.Code)
	}
	return rec.Body.String()
}

// THE DEFECT THIS TEST EXISTS FOR. The shipped systemd unit passes no
// --auditorium, so every appliance installed through the wizard rendered the
// flag's build-time default: every screen in a multiplex told its patrons the
// same room name, which is precisely the confusion §8.5 exists to catch.
func TestAnUnnamedRoomShowsNoNameRatherThanADefault(t *testing.T) {
	store := newTestStore(t)
	s := namedServer(t, store, "Auditorium 1")

	page := patronPage(t, s)
	if strings.Contains(page, "Auditorium 1") {
		t.Fatalf("an unnamed appliance told its patrons a build-time default room name:\n%s",
			extractAuditorium(page))
	}
}

// Named through the wizard, the page shows the room — and does so WITHOUT a
// restart, because an operator correcting the room must not have to.
func TestNamingTheRoomTakesEffectWithoutARestart(t *testing.T) {
	store := newTestStore(t)
	s := namedServer(t, store, "Auditorium 1")

	if err := store.PutSetting(config.SettingAuditoriumName, "Screen 7"); err != nil {
		t.Fatal(err)
	}
	page := patronPage(t, s)
	if !strings.Contains(page, "Screen 7") {
		t.Fatalf("the configured room name did not reach the patron page:\n%s", extractAuditorium(page))
	}
}

// An appliance that does not know its room fails the pre-show gate: §8.5's last
// human check cannot happen, and that is worth stopping a house for.
func TestAnUnnamedRoomFailsThePreShowChecklist(t *testing.T) {
	store := newTestStore(t)
	s := namedServer(t, store, "Auditorium 1")
	s.deps.Status = func() model.SystemReadiness { return model.SystemReadiness{} }

	cl := s.preShowChecklist()
	var found bool
	for _, c := range cl.Checks {
		if c.ID != "auditorium_named" {
			continue
		}
		found = true
		if c.State != model.CheckFail || !c.Blocking {
			t.Fatalf("an unnamed room reports %q blocking=%v", c.State, c.Blocking)
		}
	}
	if !found {
		t.Fatal("the checklist has no item for the room's identity")
	}

	if err := store.PutSetting(config.SettingAuditoriumName, "Screen 7"); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.preShowChecklist().Checks {
		if c.ID == "auditorium_named" && c.State != model.CheckPass {
			t.Fatalf("a named room reports %q", c.State)
		}
	}
}

func extractAuditorium(page string) string {
	i := strings.Index(page, `id="auditorium"`)
	if i < 0 {
		return "(no auditorium element)"
	}
	end := i + 200
	if end > len(page) {
		end = len(page)
	}
	return page[i:end]
}
