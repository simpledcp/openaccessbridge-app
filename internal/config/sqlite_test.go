package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	// Use a file in the test temp dir so WAL behaves like production.
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAuditoriumProfileRoundTrip(t *testing.T) {
	s := newTestStore(t)
	p := model.AuditoriumProfile{
		AuditoriumID:       "screen-3",
		InputDeviceID:      "usb-XYZ-interface-00",
		ChannelMapping:     "stereo:L=HI,R=VI-N",
		GainNotes:          "attenuator pad inline; isolator fitted",
		LastQualification:  model.QualUsable,
		CinemaServerIP:     "10.10.20.15",
		ServerFamily:       "vendor/model",
		ACSPPort:           4170,
		ACSPInitiation:     model.ACSInitiated,
		ExpectedScreenName: "Auditorium 3",
		IdentityStrength:   model.IdentityComposite,
		FirewallProfile:    model.ProfileLowCostReplacement,
		PhysicalPortLabel:  "THEATER-A",
		PreferredLanguages: []string{"en-US", "es-419"},
	}
	if err := s.PutAuditoriumProfile(p); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := s.GetAuditoriumProfile("screen-3")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.CinemaServerIP != p.CinemaServerIP || got.ChannelMapping != p.ChannelMapping ||
		got.FirewallProfile != p.FirewallProfile || len(got.PreferredLanguages) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	list, err := s.ListAuditoriumProfiles()
	if err != nil || len(list) != 1 {
		t.Fatalf("List: n=%d err=%v", len(list), err)
	}
	if err := s.DeleteAuditoriumProfile("screen-3"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.GetAuditoriumProfile("screen-3"); ok {
		t.Fatalf("profile not deleted")
	}
}

func TestSystemConfigDefaultsAndPersist(t *testing.T) {
	s := newTestStore(t)
	v, err := s.GetSystemConfig()
	if err != nil {
		t.Fatalf("GetSystemConfig: %v", err)
	}
	if v.ConfigSchemaVersion != model.CurrentConfigSchemaVersion || v.UpdateChannel != "stable" {
		t.Fatalf("unexpected defaults: %+v", v)
	}
	v.HardwareProfileVersion = "lowcost-v2"
	if err := s.PutSystemConfig(v); err != nil {
		t.Fatalf("PutSystemConfig: %v", err)
	}
	v2, _ := s.GetSystemConfig()
	if v2.HardwareProfileVersion != "lowcost-v2" {
		t.Fatalf("system config not persisted: %+v", v2)
	}
}

func TestAdminCredentials(t *testing.T) {
	s := newTestStore(t)
	has, _ := s.HasAnyAdminCredential()
	if has {
		t.Fatalf("expected no credentials initially")
	}
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := s.PutAdminCredential(AdminCredential{Username: "admin", PWHash: hash, MustChange: true}); err != nil {
		t.Fatalf("PutAdminCredential: %v", err)
	}
	// §8.1: a must_change credential is treated as a default that blocks setup exit.
	def, _ := s.HasDefaultCredential()
	if !def {
		t.Fatalf("must_change credential should report as default")
	}
	c, ok, err := s.GetAdminCredential("admin")
	if err != nil || !ok {
		t.Fatalf("GetAdminCredential: ok=%v err=%v", ok, err)
	}
	if !VerifyPassword(c.PWHash, "correct horse battery staple") {
		t.Fatalf("password verify failed")
	}
	if VerifyPassword(c.PWHash, "wrong password") {
		t.Fatalf("wrong password verified")
	}
	// Replace with a permanent credential.
	if err := s.PutAdminCredential(AdminCredential{Username: "admin", PWHash: hash, MustChange: false}); err != nil {
		t.Fatalf("update credential: %v", err)
	}
	if def, _ := s.HasDefaultCredential(); def {
		t.Fatalf("default credential should be cleared after change")
	}
}

func TestPasswordHashNotPlaintext(t *testing.T) {
	pw := "super secret value 123"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(h, pw) {
		t.Fatalf("hash contains plaintext password")
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
}

// TestNoContentColumns asserts the schema contains no table or column that would
// hold caption text or audio content (§9.4). This is a structural guard: the
// config DB must never grow a content column.
func TestNoContentColumns(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table','column')`)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	defer rows.Close()
	forbidden := []string{"cue", "caption_text", "audio", "pcm", "sample", "transcript"}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ln := strings.ToLower(name)
		for _, f := range forbidden {
			if strings.Contains(ln, f) {
				t.Fatalf("config schema has a content-bearing object %q (matches %q)", name, f)
			}
		}
	}
}
