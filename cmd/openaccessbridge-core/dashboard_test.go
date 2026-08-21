package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/acsp"
	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/security/support"
	"github.com/simpledcp/openaccessbridge/internal/sim"
)

// TestTimelineRowReachesTheDashboard: §8.2's "Timeline: playing / paused /
// stopped" row had no backing data at all — model.CaptionLinkState carried
// output_mode and a handshake state but no transport state. This drives the real
// machine and asserts the row follows the server.
func TestTimelineRowReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup, fetcher := newSupervisorWithFetcher(t, store, dir)
	fetcher.Files["tt_en.xml"] = sim.TimedText("en", []sim.CueSpec{{StartMS: 1000, EndMS: 4000, Text: "Hello"}})
	rplBytes := sim.RPL("show-1", []sim.ReelSpec{{Number: 1, DurationMS: 60000, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
	}}})

	if st := sup.status(); st.Timeline != model.TimelineNone {
		t.Errorf("before any show the timeline row must say so, got %q", st.Timeline)
	}

	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("srv", "Auditorium 3", ""))
	sup.machine.Handle(sim.GetNewLease(30000))
	sup.machine.Handle(sim.SetRPLInline(rplBytes))
	sup.machine.Handle(sim.SetOutputMode(true))
	sup.machine.Handle(sim.UpdateTimeline(0, "show-1", acsp.PlayPlaying))
	if st := sup.status(); st.Timeline != model.TimelinePlaying {
		t.Errorf("a rolling show must read as playing, got %q", st.Timeline)
	}

	sup.machine.Handle(sim.UpdateTimeline(2000, "show-1", acsp.PlayPaused))
	if st := sup.status(); st.Timeline != model.TimelinePaused {
		t.Errorf("a paused show must read as paused, got %q", st.Timeline)
	}

	sup.machine.Handle(sim.UpdateTimeline(2000, "show-1", acsp.PlayStopped))
	if st := sup.status(); st.Timeline != model.TimelineStopped {
		t.Errorf("a stopped show must read as stopped, got %q", st.Timeline)
	}
}

// TestCueVisibilityReachesTheDashboardWithoutTheText: §8.2's "Current cue:
// visible / none" row, under §9.4's caption.cue.visible_no_text rule. The row
// must exist, and the JSON the admin surface serves must not carry the words.
func TestCueVisibilityReachesTheDashboardWithoutTheText(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup, fetcher := newSupervisorWithFetcher(t, store, dir)

	// Before any show there is no language loaded, so the question is not
	// answerable — and must not be answered "none".
	if st := sup.status(); st.CueVisible != model.TriUnknown {
		t.Errorf("with no show loaded the cue row is unmeasured, got %q", st.CueVisible)
	}

	const cueText = "Captions are served locally to your phone."
	fetcher.Files["tt_en.xml"] = sim.TimedText("en", []sim.CueSpec{{StartMS: 0, EndMS: 30000, Text: cueText}})
	rplBytes := sim.RPL("show-1", []sim.ReelSpec{{Number: 1, DurationMS: 60000, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
	}}})
	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("srv", "Auditorium 3", ""))
	sup.machine.Handle(sim.GetNewLease(30000))
	sup.machine.Handle(sim.SetRPLInline(rplBytes))
	sup.machine.Handle(sim.SetOutputMode(true))
	sup.machine.Handle(sim.UpdateTimeline(0, "show-1", acsp.PlayPlaying))

	st := sup.status()
	if st.CueVisible != model.TriYes {
		t.Fatalf("a cue is on screen; the row must say so, got %q", st.CueVisible)
	}
	blob, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), cueText) {
		t.Fatalf("the admin status payload must never carry caption text (§9.4)")
	}
}

// TestSupportSessionRowAndWarning: §8.2's "Support session" row and §8.3's
// "debug capture / support mode active" alert. The warning code existed and
// nothing ever raised it, so the condition could not appear on the dashboard.
//
// The row is now driven by the SESSION MANAGER rather than by a stored flag. A
// persisted boolean cannot carry a deadline, so a "support mode on" setting
// written once stayed on forever — which is precisely the permanent way in that
// §9.4's time limit exists to prevent (D31).
func TestSupportSessionRowAndWarning(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())
	if st := sup.status(); st.SupportSession != model.TriNo {
		t.Errorf("support mode is off by default (§9.4), got %q", st.SupportSession)
	}
	if st := sup.status(); hasCode(st.Warnings, model.WarnDebugCaptureActive) {
		t.Errorf("nothing is capturing; the dashboard must not shout")
	}

	// Open a real, bounded, audited session — the only thing that can now put
	// the appliance into support mode.
	mustSetting(t, store, config.SettingSupportEndpointHost, "203.0.113.10")
	mustSetting(t, store, config.SettingSupportEndpointPort, "443")
	mustSetting(t, store, config.SettingSupportEgressIface, "eth0")
	if _, err := sup.support.Open(support.OpenRequest{Reason: "ticket 42", AdminUser: "tech"}); err != nil {
		t.Fatalf("opening a support session: %v", err)
	}
	sup.reconcile(context.Background())
	st := sup.status()
	if st.SupportSession != model.TriYes {
		t.Errorf("an open support session must be shown, got %q", st.SupportSession)
	}
	if !hasCode(st.Warnings, model.WarnDebugCaptureActive) {
		t.Errorf("an open support session must be visibly indicated, got %v", codes(st.Warnings))
	}

	// And closing it must take the row and the alert back down: a warning that
	// only ever goes up is one a booth learns to ignore.
	if err := sup.support.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	sup.reconcile(context.Background())
	st = sup.status()
	if st.SupportSession != model.TriNo {
		t.Errorf("a closed session still shows as open, got %q", st.SupportSession)
	}
	if hasCode(st.Warnings, model.WarnDebugCaptureActive) {
		t.Errorf("the alert survived the session closing: %v", codes(st.Warnings))
	}
}

func mustSetting(t *testing.T, store *config.SQLiteStore, key, value string) {
	t.Helper()
	if err := store.PutSetting(key, value); err != nil {
		t.Fatalf("PutSetting(%s): %v", key, err)
	}
}

// TestWrongRoomOutcomeReachesTheDashboard: §8.5 — the dashboard shows the pinned
// identity, who actually answered, AND the outcome of the check. Before this the
// outcome never left the ACSP layer, so an unrun guard and a passed guard were
// indistinguishable on the page.
func TestWrongRoomOutcomeReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15",
		ExpectedServerFingerprint: "pinned-server-id", IdentityStrength: model.IdentityComposite,
	})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	st := sup.status()
	if st.IdentityCheck != model.IdentityCheckUnknown {
		t.Errorf("no server has announced yet, got %q", st.IdentityCheck)
	}
	if st.IdentityCheckOK() {
		t.Errorf("a guard that has not run is not a pass")
	}

	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("some-other-server", "Auditorium 9", "different-fingerprint"))
	st = sup.status()
	if st.IdentityCheck != model.IdentityCheckMismatch {
		t.Fatalf("a wrong-room server must report a mismatch, got %q", st.IdentityCheck)
	}
	if !strings.Contains(st.IdentityCheckLabel(), "wrong room") {
		t.Errorf("the label must name the condition, got %q", st.IdentityCheckLabel())
	}
	// §10.5: the pin here is only a composite identity and must be labelled so.
	if !strings.Contains(st.IdentityStrengthLabel(), "not authentication") {
		t.Errorf("a composite identity must carry its caveat, got %q", st.IdentityStrengthLabel())
	}
}

// TestDeployPostureReachesTheDashboard: §8.4's banner obligation travels with
// the profile, and the dashboard is where it has to land.
func TestDeployPostureReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir) // Booth Diagnostic (D13 default)
	sup.reconcile(context.Background())
	st := sup.status()
	if st.DeploymentTitle != mustProfile(model.ProfileBoothDiagnostic).Title {
		t.Errorf("the posture in force must be named, got %q", st.DeploymentTitle)
	}
	if st.DeploymentBanner == "" {
		t.Errorf("Booth Diagnostic requires a prominent banner (§8.4), got none")
	}
}

// TestAdminOnPatronNetIsMeasuredFromTheRealBinds: §8.4's actual condition.
// verify.Report.AdminIsolated cannot answer it — it reports whether the binding
// meets the ACTIVE PROFILE's requirement, and Booth Diagnostic imposes none, so
// it reads "isolated" for an admin surface bound on every interface. The banner
// and the dashboard row both need the raw fact.
func TestAdminOnPatronNetIsMeasuredFromTheRealBinds(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir) // patron 0.0.0.0:8080, admin 127.0.0.1:8090
	sup.reconcile(context.Background())
	if st := sup.status(); st.AdminOnPatronNet != model.TriNo {
		t.Errorf("a loopback admin bind is not on the patron network, got %q", st.AdminOnPatronNet)
	}

	sup.adminBind = "0.0.0.0:8090" // admin now answers on the patron interface too
	sup.reconcile(context.Background())
	st := sup.status()
	if st.AdminOnPatronNet != model.TriYes {
		t.Fatalf("admin bound on every interface IS reachable from the patron net, got %q", st.AdminOnPatronNet)
	}
	if !st.AdminIsolated {
		t.Errorf("Booth Diagnostic imposes no isolation requirement, so that verdict is unchanged — " +
			"which is exactly why the raw fact has to be reported separately")
	}
	if !strings.Contains(st.DeploymentBanner, "PATRON") {
		t.Errorf("the §8.4 banner must name the condition, got %q", st.DeploymentBanner)
	}
}

// TestDiscoveryRowIsAnObservationAboutThisBuild: no OpenAccessBridge process runs a
// discovery/advertisement responder, so "off" here is a fact about the code.
func TestDiscoveryRowIsAnObservationAboutThisBuild(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	if st := sup.status(); st.DiscoveryMode != model.TriNo {
		t.Errorf("discovery row = %q, want no", st.DiscoveryMode)
	}
}

// TestAudioRowsAreDerivedFromTheMediaProcess: with no media helper answering,
// §8.2's audio rows must be absent rather than a set of reassuring zeroes.
func TestAudioRowsAreDerivedFromTheMediaProcess(t *testing.T) {
	dir := t.TempDir() // nothing listening
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())
	st := sup.status()
	if len(st.AudioStreams) != 0 {
		t.Errorf("an unreachable media helper measures nothing, got %+v", st.AudioStreams)
	}
	if st.AudioReady {
		t.Errorf("no measurements is not readiness")
	}
}

// TestDriftMeasuredReachesTheDashboard: §8.2's drift row against §5.5. A
// supervisor with no show loaded reports drift 0 because MaxDriftMS has no
// language engine to range over, and the dashboard has to be told that the zero
// is not a reading — otherwise a DISCONNECTED link renders as perfect sync.
func TestDriftMeasuredReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup, fetcher := newSupervisorWithFetcher(t, store, dir)
	fetcher.Files["tt_en.xml"] = sim.TimedText("en", []sim.CueSpec{{StartMS: 1000, EndMS: 4000, Text: "Hello"}})
	rplBytes := sim.RPL("show-1", []sim.ReelSpec{{Number: 1, DurationMS: 60000, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
	}}})

	st := sup.status()
	if st.CaptionLink.DriftMSEstimate != 0 {
		t.Fatalf("precondition: an idle controller reports 0 drift, got %d", st.CaptionLink.DriftMSEstimate)
	}
	if st.CaptionDriftMeasured != model.TriNo {
		t.Errorf("with no show loaded the drift figure is vacuous, got %q", st.CaptionDriftMeasured)
	}

	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("srv", "Auditorium 3", ""))
	sup.machine.Handle(sim.GetNewLease(30000))
	sup.machine.Handle(sim.SetRPLInline(rplBytes))
	sup.machine.Handle(sim.SetOutputMode(true))
	sup.machine.Handle(sim.UpdateTimeline(0, "show-1", acsp.PlayPlaying))
	if st := sup.status(); st.CaptionDriftMeasured != model.TriYes {
		t.Errorf("a loaded show has a timing engine to measure drift against, got %q", st.CaptionDriftMeasured)
	}
}
