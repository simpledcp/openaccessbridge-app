package readiness

import (
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
)

// healthy is an appliance with everything the checklist can measure passing, so
// each test can break exactly one thing.
func healthy() model.SystemReadiness {
	return model.SystemReadiness{
		ForwardingDisabled:   true,
		FirewallActive:       true,
		FirewallIntrospected: true,
		SSHDisabled:          true,
		SSHIntrospected:      true,
		AdminOnPatronNet:     model.TriNo,
		DefaultCreds:         false,
		IdentityCheck:        "match",
		DeploymentProfile:    model.ProfilePatronFacing,
		CaptionLink: model.CaptionLinkState{
			LeaseActive: true, RPLLoaded: true, AssetsLoaded: true,
			RequestIntervalMS: 30000, AvailableLanguages: []string{"en", "es-419"},
		},
		AudioStreams: []model.AudioStreamSummary{
			{StreamType: model.StreamHI, InputPresent: model.TriYes, StreamActive: model.TriYes, Quality: model.QualExcellent},
			{StreamType: model.StreamVIN, InputPresent: model.TriYes, StreamActive: model.TriYes, Quality: model.QualUsable},
		},
		ICEScope: model.ICEScopeReport{Configured: true, Interface: "wlan0", InForce: true},
	}
}

func liveFacts() ChecklistFacts {
	now := time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC)
	return ChecklistFacts{
		Auditorium: "Auditorium 3", PerShowTokenRequired: true,
		SessionIssued: true, JoinCodeIssued: true,
		SessionExpiresAt: now.Add(4 * time.Hour),
		Now:              func() time.Time { return now },
	}
}

func find(t *testing.T, cl model.PreShowChecklist, id string) model.PreShowCheck {
	t.Helper()
	for _, c := range cl.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no checklist item %q", id)
	return model.PreShowCheck{}
}

// §11.5's actual requirement, and the one most often skipped: "define what staff
// do when a check fails". An item without a remedy has moved the problem to a
// person who now has a red line and no instruction.
func TestEveryItemTellsStaffWhatToDoAboutIt(t *testing.T) {
	cl := BuildChecklist(healthy(), liveFacts())
	if len(cl.Checks) < 10 {
		t.Fatalf("only %d items on the sheet; §11.5 lists more than that", len(cl.Checks))
	}
	for _, c := range cl.Checks {
		if strings.TrimSpace(c.Remedy) == "" {
			t.Errorf("%s has no remedy: a booth reading this has a red line and no procedure", c.ID)
		}
		if strings.TrimSpace(c.Detail) == "" {
			t.Errorf("%s says nothing about what was observed", c.ID)
		}
		if strings.TrimSpace(c.Section) == "" {
			t.Errorf("%s cites no requirement, so nobody can trace why it is on the sheet", c.ID)
		}
	}
}

// A check nobody ran must not buy the same silence as a check that passed. This
// is the whole reason the state is three-valued.
func TestAnUnrunBlockingCheckIsNotAPass(t *testing.T) {
	st := healthy()
	// The firewall is asserted active but nothing read the ruleset (D17) — the
	// real state on a host without root.
	st.FirewallIntrospected = false
	cl := BuildChecklist(st, liveFacts())

	fw := find(t, cl, "firewall_active")
	if fw.State != model.CheckNotRun {
		t.Fatalf("an unintrospected firewall reports %q", fw.State)
	}
	if fw.Passing() {
		t.Fatal("an unverified posture is passing the pre-show gate")
	}
	if cl.ApplianceReady {
		t.Fatal("the appliance reports itself ready with a blocking check that never ran")
	}
	if !contains(cl.BlockingProblems, "firewall_active") {
		t.Fatalf("the unrun check is not listed as a blocking problem: %v", cl.BlockingProblems)
	}
}

// The sharpest line on the sheet. A generated test tone can qualify perfectly;
// presenting that as the room's audio would send a booth to open a house on a
// 440 Hz tone.
func TestATestToneIsNeverAPassingAudioCheck(t *testing.T) {
	st := healthy()
	st.AudioStreams[0].SyntheticSource = true
	cl := BuildChecklist(st, liveFacts())

	hi := find(t, cl, "audio_hi")
	if hi.State == model.CheckPass {
		t.Fatal("a synthetic test tone passed the HI check")
	}
	if hi.State != model.CheckUnmeasurable {
		t.Fatalf("a test tone reports %q, want unmeasurable", hi.State)
	}
	if !strings.Contains(strings.ToLower(hi.Detail), "test tone") {
		t.Fatalf("the item does not say it is a test tone: %q", hi.Detail)
	}
	if cl.ApplianceReady {
		t.Fatal("the appliance is ready for a show on a generated tone")
	}
}

// §11.5 lists "latency within threshold". Nothing here can measure glass-to-ear
// latency (B2/B3), and a tick beside it would tell a booth something nobody
// knows.
func TestLatencyIsReportedAsUnmeasurableNotAsPassing(t *testing.T) {
	cl := BuildChecklist(healthy(), liveFacts())
	lat := find(t, cl, "audio_latency_within_budget")
	if lat.State != model.CheckUnmeasurable {
		t.Fatalf("latency reports %q on an appliance that has never measured it", lat.State)
	}
	if !strings.Contains(lat.Detail, "jitter buffer") {
		t.Errorf("the item does not name the dominant unmeasured term: %q", lat.Detail)
	}
	// It must not block: refusing to open a house over a measurement that CANNOT
	// be taken here would make the sheet unusable and it would be ignored.
	if lat.Blocking {
		t.Error("an unmeasurable item is blocking the house")
	}
}

// The items only a person can do must never be reported as done, and the sheet
// must say how many are outstanding. Storing an attestation the appliance cannot
// verify is the §10.8 button-press bug in a new place.
func TestStaffStepsAreAlwaysOutstandingAndNeverPass(t *testing.T) {
	cl := BuildChecklist(healthy(), liveFacts())
	staff := 0
	for _, c := range cl.Checks {
		if c.Source != model.SourceStaff {
			continue
		}
		staff++
		if c.State != model.CheckNotRun {
			t.Errorf("%s is a staff step and the appliance reports it as %q", c.ID, c.State)
		}
	}
	if staff == 0 {
		t.Fatal("the sheet has no staff steps at all: §11.5's seat-level checks cannot be measured here")
	}
	if cl.StaffStepsOutstanding != staff {
		t.Fatalf("%d staff steps, %d reported outstanding", staff, cl.StaffStepsOutstanding)
	}
	// And an appliance that is happy with itself must not read as "ready for a
	// show" — the staff steps are still undone.
	if !cl.ApplianceReady {
		t.Fatal("the healthy fixture should have the appliance's own items passing")
	}
}

// A wrong-room guard that never ran must not read like a match (§8.5).
func TestAnUnpinnedServerIdentityIsNotAMatch(t *testing.T) {
	st := healthy()
	st.IdentityCheck = "unpinned"
	cl := BuildChecklist(st, liveFacts())
	room := find(t, cl, "right_auditorium")
	if room.Passing() {
		t.Fatal("no identity is pinned and the wrong-room check passed")
	}
	if cl.ApplianceReady {
		t.Fatal("a wrong-room guard that never ran let the appliance report itself ready")
	}
}

// A session that expired before the show is not a session.
func TestAnExpiredShowSessionFailsTheJoinCheck(t *testing.T) {
	facts := liveFacts()
	facts.SessionExpiresAt = facts.Now().Add(-time.Minute)
	cl := BuildChecklist(healthy(), facts)
	s := find(t, cl, "show_session_issued")
	if s.State != model.CheckFail {
		t.Fatalf("an expired session reports %q", s.State)
	}
}

// On a profile that needs no per-show token, a missing session is not a fault —
// the patron page is simply open. The sheet must not manufacture a failure.
func TestAnOpenProfileDoesNotDemandAJoinCode(t *testing.T) {
	facts := liveFacts()
	facts.PerShowTokenRequired = false
	facts.SessionIssued = false
	cl := BuildChecklist(healthy(), facts)
	s := find(t, cl, "show_session_issued")
	if s.State != model.CheckPass {
		t.Fatalf("an open profile with no session reports %q", s.State)
	}
	if !cl.ApplianceReady {
		t.Fatalf("blocking problems on an otherwise healthy open profile: %v", cl.BlockingProblems)
	}
}

// The room's own name, not the cinema server's. Confusing the two is the
// wrong-room mix-up §8.5 exists to prevent.
func TestTheSheetNamesTheAuditoriumNotTheServer(t *testing.T) {
	st := healthy()
	st.CaptionLink.Server = "dolby-imb-7"
	cl := BuildChecklist(st, liveFacts())
	if cl.Auditorium != "Auditorium 3" {
		t.Fatalf("the sheet is headed %q, want the auditorium name", cl.Auditorium)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// §8.5: "Tie the auditorium profile to the physical port label so a re-cabling
// that swaps rooms is caught at setup, not mid-show." The appliance cannot see a
// cable, so the honest form is a staff step that prints the label it was given.
func TestThePortLabelIsPrintedForAPersonToCheck(t *testing.T) {
	facts := liveFacts()
	facts.PhysicalPortLabel = "rack A, port 3"
	cl := BuildChecklist(healthy(), facts)

	c := find(t, cl, "theater_cable_in_the_labelled_port")
	if c.Source != model.SourceStaff {
		t.Fatalf("the cable check is %q; an appliance cannot see a cable", c.Source)
	}
	if !strings.Contains(c.Detail, "rack A, port 3") {
		t.Fatalf("the sheet does not print the label to check against: %q", c.Detail)
	}
	if c.State != model.CheckNotRun {
		t.Fatalf("a staff step reports %q", c.State)
	}
}

// With no label recorded there is nothing to check the rack against, and the
// sheet says so rather than printing an empty instruction.
func TestNoPortLabelSaysThereIsNothingToCheckAgainst(t *testing.T) {
	cl := BuildChecklist(healthy(), liveFacts())
	c := find(t, cl, "theater_cable_in_the_labelled_port")
	if !strings.Contains(c.Detail, "No physical port label") {
		t.Fatalf("an unrecorded label does not say so: %q", c.Detail)
	}
	if !strings.Contains(c.Remedy, "setup wizard") {
		t.Fatalf("the remedy does not say how to record one: %q", c.Remedy)
	}
}
