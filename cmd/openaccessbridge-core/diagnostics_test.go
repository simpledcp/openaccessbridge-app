package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/security/verify"
)

// §10.8 exists to answer "what changed since this last worked?". Two of its four
// compatibility revisions were declared and never written, so a tech comparing a
// working appliance with a broken one saw the schema and the firewall and
// nothing about the hardware or the server family.
func TestTheHardwareRevisionNamesTheDevicesActuallyServing(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)

	// Nothing observed yet: empty is the honest answer. A revision string nobody
	// set would compare EQUAL between two appliances that differ, which is worse
	// than an obviously absent field.
	if got := sup.hardwareProfileVersion(); got != "" {
		t.Fatalf("an appliance with no observed inputs reports hardware revision %q", got)
	}

	sup.media.mu.Lock()
	sup.media.latest = []model.AudioStreamState{
		{StreamType: model.StreamHI, InputDeviceID: "synthetic-HI"},
		{StreamType: model.StreamVIN, InputDeviceID: "synthetic-VI-N"},
	}
	sup.media.mu.Unlock()

	got := sup.hardwareProfileVersion()
	if !strings.Contains(got, runtime.GOARCH) {
		t.Errorf("the revision does not name the platform: %q", got)
	}
	for _, dev := range []string{"synthetic-HI", "synthetic-VI-N"} {
		if !strings.Contains(got, dev) {
			t.Errorf("the revision does not name the device %q it is serving from: %q", dev, got)
		}
	}
}

// The revision must be STABLE for the same hardware and CHANGE when it changes —
// that is the whole use of it.
func TestTheHardwareRevisionChangesOnlyWhenTheHardwareDoes(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)

	set := func(ids ...string) string {
		sup.media.mu.Lock()
		sup.media.latest = nil
		for _, id := range ids {
			sup.media.latest = append(sup.media.latest,
				model.AudioStreamState{InputDeviceID: id})
		}
		sup.media.mu.Unlock()
		return sup.hardwareProfileVersion()
	}

	a := set("dev-a", "dev-b")
	// Same devices, reported in the other order: the revision must not move, or
	// every poll would look like a hardware change.
	if b := set("dev-b", "dev-a"); a != b {
		t.Fatalf("the revision changed on report order alone:\n %q\n %q", a, b)
	}
	if c := set("dev-a", "dev-c"); a == c {
		t.Fatal("swapping a capture device did not change the hardware revision")
	}
}

// The server family is a Phase 0A finding (B1) and is often unknown. "unknown"
// written into a revision field compares EQUAL across two appliances talking to
// different servers, so an unknown family is an empty revision.
func TestAnUnknownServerFamilyIsAnEmptyRevisionNotAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.0.0.5"})
	sup := newSupervisor(t, store, dir)
	if got := sup.serverFamilyCompat(); got != "" {
		t.Fatalf("an unknown server family produced the revision %q", got)
	}

	if err := store.PutAuditoriumProfile(model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.0.0.5",
		ServerFamily: "dolby-imb", ACSPInitiation: model.ACSInitiated,
	}); err != nil {
		t.Fatal(err)
	}
	got := sup.serverFamilyCompat()
	if !strings.Contains(got, "dolby-imb") {
		t.Fatalf("the revision does not name the family: %q", got)
	}
	if !strings.Contains(got, string(model.ACSInitiated)) {
		t.Fatalf("the revision does not name the ACSP variant in force: %q", got)
	}
}

// And both must reach the persisted §10.8 record, not just exist as functions.
func TestBothRevisionsReachThePersistedRecord(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.0.0.5",
		ServerFamily: "dolby-imb", ACSPInitiation: model.ACSInitiated,
	})
	sup := newSupervisor(t, store, dir)
	sup.media.mu.Lock()
	sup.media.latest = []model.AudioStreamState{{InputDeviceID: "synthetic-HI"}}
	sup.media.mu.Unlock()

	// recordReadiness directly rather than through reconcile: reconcile polls the
	// media helper first, and with no helper running that poll legitimately
	// clears the observed devices. An appliance with no media process genuinely
	// has no hardware to report — which is why the record only ever OVERWRITES
	// these fields with a non-empty value, so the last known hardware survives
	// exactly the outage a tech would be debugging.
	sup.recordReadiness(sup.buildStatus(verify.Report{}), verify.Report{})

	v, err := store.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	if v.HardwareProfileVersion == "" {
		t.Error("§10.8's hardware revision was never persisted")
	}
	if v.ServerFamilyCompat == "" {
		t.Error("§10.8's server-family revision was never persisted")
	}
}

// The record keeps the LAST KNOWN hardware when the media process goes away,
// because that is exactly the outage a tech is debugging: losing the revision at
// the moment it is needed would make §10.8 useless for its one question.
func TestTheHardwareRevisionSurvivesTheMediaProcessGoingAway(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)

	sup.media.mu.Lock()
	sup.media.latest = []model.AudioStreamState{{InputDeviceID: "synthetic-HI"}}
	sup.media.mu.Unlock()
	sup.recordReadiness(sup.buildStatus(verify.Report{}), verify.Report{})

	before, err := store.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	if before.HardwareProfileVersion == "" {
		t.Fatal("the hardware revision was never recorded")
	}

	// The media helper stops answering: nothing is observed any more.
	sup.media.noteDown("gone")
	sup.recordReadiness(sup.buildStatus(verify.Report{}), verify.Report{})

	after, err := store.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.HardwareProfileVersion != before.HardwareProfileVersion {
		t.Fatalf("the last known hardware was lost when media went away: %q -> %q",
			before.HardwareProfileVersion, after.HardwareProfileVersion)
	}
}
