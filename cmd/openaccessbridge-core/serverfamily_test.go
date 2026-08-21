package main

import (
	"context"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/readiness"
)

// TestTheConfiguredServerFamilyReachesTheMachine is the D38 lesson applied to a
// second field, and it is here because a mutation proved the lesson had not
// stuck: deleting `ServerFamily: aud.ServerFamily` from captionMachineOptions
// broke nothing in the suite.
//
// That is the exact shape of the wrong-room defect — the setup wizard collected
// a value, the profile stored it, the machine implemented the behaviour behind
// it, and production never passed it across. Every layer was correct and the
// feature did nothing. The only test that catches it is one that asserts the
// PRODUCTION options builder carries the field, so this asserts that.
func TestTheConfiguredServerFamilyReachesTheMachine(t *testing.T) {
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15", ServerFamily: "Dolby IMS3000"})
	got, ok := auditoriumProfile(store)
	if !ok || got.ServerFamily != "Dolby IMS3000" {
		t.Fatalf("the profile did not round-trip: %+v", got)
	}
	opts := captionMachineOptions(store, logging.Nop(), got, readiness.NewCaptionWarnings(0))
	if opts.ServerFamily != "Dolby IMS3000" {
		t.Fatalf("captionMachineOptions.ServerFamily = %q, want %q — the configured "+
			"family never reaches the machine, so §5.3 observations cannot be attributed",
			opts.ServerFamily, "Dolby IMS3000")
	}
}

// TestTheUnresolvedLocatorTallyReachesTheDashboard: the resolver counted these
// from the day it was written and NOTHING read the count. Adding the reader is
// not the fix on its own — a reader nothing tests is the same defect one level
// up, which a mutation confirmed by deleting the reader and breaking no test.
//
// This drives the real supervisor's status assembly, so removing the field from
// readiness.Inputs, the supervisor's accessor, or the wiring that gives the
// supervisor its resolver all fail here.
func TestTheUnresolvedLocatorTallyReachesTheDashboard(t *testing.T) {
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15"})
	sup, _ := newSupervisorWithFetcher(t, store, t.TempDir())

	if got := sup.status().UnresolvedLocators; len(got) != 0 {
		t.Fatalf("nothing has been fetched, so there is nothing to report: %+v", got)
	}

	sup.res.SetServerFamily("GDC SR-1000")
	if _, err := sup.res.Resolve(context.Background(), "dcp://vendor/an_unreleased_title"); err == nil {
		t.Fatal("expected a vendor-specific locator to be unresolvable")
	}

	got := sup.status().UnresolvedLocators
	if len(got) != 1 {
		t.Fatalf("the observation did not reach the dashboard document: %+v", got)
	}
	if got[0].Family != "GDC SR-1000" || got[0].Form != "scheme:dcp" || got[0].Count != 1 {
		t.Fatalf("tally = %+v, want one scheme:dcp observation attributed to GDC SR-1000", got[0])
	}
	// §9.4: the form travels, the locator never does.
	for _, u := range got {
		if strings.Contains(u.Form, "an_unreleased_title") {
			t.Fatalf("the raw locator reached the dashboard document: %+v", u)
		}
	}
}

// An appliance that has resolved everything reports an EMPTY tally, and the
// field is omitempty so it is absent from the JSON rather than present-and-null.
// Absence is the honest encoding of "no observation": a present empty list on a
// dashboard reads as a checked-and-clean result.
func TestACleanApplianceReportsNoTally(t *testing.T) {
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15"})
	sup, _ := newSupervisorWithFetcher(t, store, t.TempDir())
	st := sup.status()
	if len(st.UnresolvedLocators) != 0 {
		t.Fatalf("unexpected observations: %+v", st.UnresolvedLocators)
	}
}
