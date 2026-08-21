package main

import (
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/sim"

	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/readiness"
)

// §8.5's wrong-room guard has two pinned halves, and production supplied one.
//
// The screen name was collected by the setup wizard, stored in the §10.5
// profile, and never passed to the machine that compares it — so an installer
// who filled it in configured nothing. The fingerprint half is the one a server
// may not even announce (B1), which made the unwired half the only one an
// operator can actually use today.
func TestBothHalvesOfThePinnedIdentityReachTheMachine(t *testing.T) {
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID:              "screen-3",
		CinemaServerIP:            "10.10.20.15",
		ExpectedScreenName:        "Auditorium 3",
		ExpectedServerFingerprint: "pinned-fingerprint",
	})
	aud, ok := auditoriumProfile(store)
	if !ok {
		t.Fatal("no stored profile")
	}
	opts := captionMachineOptions(store, logging.Nop(), aud, readiness.NewCaptionWarnings(0))

	if opts.ExpectedFingerprint != "pinned-fingerprint" {
		t.Errorf("the pinned fingerprint did not reach the machine, got %q", opts.ExpectedFingerprint)
	}
	if opts.ExpectedScreenName != "Auditorium 3" {
		t.Errorf("the pinned SCREEN NAME did not reach the machine (got %q): "+
			"an installer's entry in the wizard is ignored", opts.ExpectedScreenName)
	}
}

// The guard has to CATCH something, not merely receive its input. This drives the
// real machine through the connection sequence with a server announcing the
// wrong screen name — the re-cabling §8.5 is about — and requires the mismatch
// to reach the dashboard.
func TestAWrongScreenNameIsCaughtAndReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID:       "screen-3",
		CinemaServerIP:     "10.10.20.15",
		ExpectedScreenName: "Auditorium 3",
	})
	sup := newSupervisor(t, store, dir)

	// A server that answers as a DIFFERENT room: the appliance was re-patched, or
	// the wrong endpoint was configured. NOTE the fingerprint is empty on both
	// sides — this is the case where the screen name is the ONLY pin available,
	// which is the case an installer can actually configure today (B1).
	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("the-server", "Screen 7", ""))

	st := sup.status()
	if st.IdentityCheck != model.IdentityCheckMismatch {
		t.Fatalf("a server announcing %q against a pin of %q reports identity %q",
			"Screen 7", "Auditorium 3", st.IdentityCheck)
	}
	if !hasCode(st.Warnings, model.WarnServerIdentityMismatch) {
		t.Fatalf("a wrong-room mismatch raised no warning: %v", codes(st.Warnings))
	}
}

// The same path with the RIGHT room must pass, so the test above is not simply
// asserting that everything mismatches.
func TestTheRightScreenNameMatches(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID:       "screen-3",
		CinemaServerIP:     "10.10.20.15",
		ExpectedScreenName: "Auditorium 3",
	})
	sup := newSupervisor(t, store, dir)
	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("the-server", "Auditorium 3", ""))

	if st := sup.status(); st.IdentityCheck != model.IdentityCheckMatch {
		t.Fatalf("the pinned room reports identity %q", st.IdentityCheck)
	}
}
