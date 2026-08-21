package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serviceWith(t *testing.T, dir string, ts *TrustStore, cur Current) *Service {
	t.Helper()
	return NewService(Options{
		Trust: ts, MediaDir: dir,
		Current: func() Current { return cur },
		Status:  func() SlotStatus { return SlotStatus{Active: SlotA, ActiveBuild: cur.Build} },
	})
}

func writeBundle(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A good bundle on the media verifies, and says which key vouched for it.
func TestAGoodBundleOnTheMediaVerifies(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t, "vendor-2026")
	writeBundle(t, dir, "cb-1.4.2.oabupd", s.bundle(t, []byte("image"), nil))

	svc := serviceWith(t, dir, trust(t, s), Current{Build: 1000, BuildKnown: true})
	res, err := svc.VerifyFile("cb-1.4.2.oabupd")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("a good bundle failed: %s", res.Reason)
	}
	if res.KeyID != "vendor-2026" {
		t.Fatalf("verified under %q", res.KeyID)
	}
}

// THE distinction this service exists to keep. A bundle can be perfectly valid
// and still not installable, because this appliance has no A/B slots. Collapsing
// the two would let "this bundle is good" read as "this update can be applied",
// and a booth would wait for something that is never going to happen.
func TestAValidBundleIsNotReportedAsInstallableWithoutSlots(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t, "vendor-2026")
	writeBundle(t, dir, "cb-1.4.2.oabupd", s.bundle(t, []byte("image"), nil))

	svc := serviceWith(t, dir, trust(t, s), Current{Build: 1000, BuildKnown: true})
	res, _ := svc.VerifyFile("cb-1.4.2.oabupd")
	if !res.OK {
		t.Fatalf("the bundle should verify: %s", res.Reason)
	}
	if res.Installable {
		t.Fatal("a bundle is reported installable on an appliance with no A/B slots")
	}
	rep := svc.Status()
	if rep.ABSupported {
		t.Fatal("the appliance claims A/B support it does not have")
	}
	if !strings.Contains(rep.ABNote, "B8") {
		t.Fatalf("the note does not point at the blocker: %q", rep.ABNote)
	}
}

// A failed verification still shows what the bundle CLAIMS, because an operator
// holding the wrong stick needs to know which build is on it.
func TestAFailedVerificationStillNamesTheBuild(t *testing.T) {
	dir := t.TempDir()
	real, other := newSigner(t, "vendor-2026"), newSigner(t, "vendor-2030")
	writeBundle(t, dir, "hostile.oabupd", other.bundle(t, []byte("image"), nil))

	svc := serviceWith(t, dir, trust(t, real), Current{Build: 1000, BuildKnown: true})
	res, _ := svc.VerifyFile("hostile.oabupd")
	if res.OK {
		t.Fatal("a bundle signed by an untrusted key verified")
	}
	if res.Manifest == nil || res.Manifest.Version != "1.4.2" {
		t.Fatalf("the failed result does not say what the bundle claims: %+v", res.Manifest)
	}
	if res.Reason == "" {
		t.Fatal("no reason was given for the refusal")
	}
}

// The name comes from an operator surface and is never interpreted as a path.
func TestABundleNameIsNeverAPath(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t, "vendor-2026")
	svc := serviceWith(t, dir, trust(t, s), Current{Build: 1000, BuildKnown: true})

	for _, name := range []string{
		"../../etc/passwd",
		"sub/dir.oabupd",
		"/etc/shadow",
		"",
		"   ",
	} {
		if _, err := svc.VerifyFile(name); err == nil {
			t.Errorf("%q was accepted as a bundle name", name)
		}
	}
}

// An appliance with no keys can verify nothing, and the status must say so
// before a bundle ever arrives.
func TestAnApplianceWithNoKeysSaysSoUpFront(t *testing.T) {
	empty, err := NewTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	svc := serviceWith(t, t.TempDir(), empty, Current{BuildKnown: true})
	if rep := svc.Status(); rep.TrustedKeys != 0 {
		t.Fatalf("reported %d trusted keys", rep.TrustedKeys)
	}
}

// The media listing shows the candidates an operator can act on.
func TestTheMediaListingShowsBundles(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t, "vendor-2026")
	writeBundle(t, dir, "cb-1.4.2.oabupd", s.bundle(t, []byte("image"), nil))
	writeBundle(t, dir, "notes.txt", []byte("not a bundle"))

	rep := serviceWith(t, dir, trust(t, s), Current{BuildKnown: true}).Status()
	if len(rep.Bundles) != 1 || rep.Bundles[0] != "cb-1.4.2.oabupd" {
		t.Fatalf("the listing is %v", rep.Bundles)
	}
}

// Unreadable media is not the same fact as empty media. An operator who has just
// pushed a stick in needs to tell "nothing on it" from "I cannot read it".
func TestUnreadableMediaIsNotReportedAsEmpty(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	svc := serviceWith(t, "/nonexistent/update/media", trust(t, s), Current{BuildKnown: true})
	rep := svc.Status()
	if rep.MediaReadable {
		t.Fatal("a missing media directory reports as readable")
	}
	if rep.MediaError == "" {
		t.Fatal("nothing says why the media could not be read")
	}
	if len(rep.Bundles) != 0 {
		t.Fatalf("bundles listed from unreadable media: %v", rep.Bundles)
	}

	// No directory configured at all is a THIRD answer, not the same as broken
	// media: a bare false would be read as "the stick is unreadable".
	none := serviceWith(t, "", trust(t, s), Current{BuildKnown: true})
	if rep := none.Status(); rep.MediaReadable || !strings.Contains(rep.MediaError, "no update media directory") {
		t.Fatalf("unconfigured media reports readable=%v err=%q", rep.MediaReadable, rep.MediaError)
	}

	// And a genuinely empty, readable directory is a different answer.
	empty := serviceWith(t, t.TempDir(), trust(t, s), Current{BuildKnown: true})
	if rep := empty.Status(); !rep.MediaReadable || rep.MediaError != "" {
		t.Fatalf("readable empty media reports readable=%v err=%q", rep.MediaReadable, rep.MediaError)
	}
}
