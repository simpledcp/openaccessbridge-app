package readiness

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/model"
)

// TestEstimateIsNeverPromotedToAMeasurement: §8.2 asks for "Estimated audio
// latency", and the only number this appliance owns is an ENCODE-PATH estimate.
// §6.7 / D10 budgets are glass-to-ear and the dominant terms (Wi-Fi, browser
// jitter buffer, phone output) are outside this box, so until BLOCKER B3 lifts
// the derived row must report no measurement at all — the estimate travels under
// its own name or not at all.
func TestEstimateIsNeverPromotedToAMeasurement(t *testing.T) {
	audio := []model.AudioStreamState{{
		StreamType: model.StreamHI, Status: model.QualUsable, Present: true, DeviceConnected: true,
		LatencyMSEst: 42, LatencyMeasured: false, EncoderState: model.EncoderStateReadyNoTransport,
	}}
	r := Assemble(Inputs{Verify: cleanVerify(), Audio: audio})
	if len(r.AudioStreams) != 1 {
		t.Fatalf("expected one derived audio row, got %+v", r.AudioStreams)
	}
	got := r.AudioStreams[0]
	if got.LatencyMeasured {
		t.Errorf("no end-to-end latency has been measured (B3)")
	}
	if got.LatencyMS != 0 {
		t.Errorf("an unmeasured latency must stay empty, got %d ms", got.LatencyMS)
	}
	if got.EncodePathEstimateMS != 42 {
		t.Errorf("the encode-path estimate belongs under its own name, got %d", got.EncodePathEstimateMS)
	}
}

// TestMeasuredLatencySurfaces: the honest half of the rule above — once an
// end-to-end figure really is measured, the row carries it.
func TestMeasuredLatencySurfaces(t *testing.T) {
	audio := []model.AudioStreamState{{
		StreamType: model.StreamVIN, Status: model.QualUsable, Present: true,
		LatencyMSEst: 118, LatencyMeasured: true,
	}}
	r := Assemble(Inputs{Verify: cleanVerify(), Audio: audio})
	if got := r.AudioStreams[0]; !got.LatencyMeasured || got.LatencyMS != 118 {
		t.Errorf("a measured latency must be rendered, got %+v", got)
	}
}

// TestStreamActiveNeverGuesses: §8.2's "HI stream active / VI stream active".
// An encode path that was never exercised is NOT MEASURED; a proven encoder with
// no subscriber transport reaches nobody and is a plain no; and an encoder state
// this build does not recognise is not evidence of a live stream either.
func TestStreamActiveNeverGuesses(t *testing.T) {
	cases := []struct {
		encoder string
		want    model.Tri
	}{
		{model.EncoderStateUnknown, model.TriUnknown},
		{"", model.TriUnknown},
		{"some_future_state", model.TriUnknown},
		{model.EncoderStateUnavailable, model.TriNo},
		{model.EncoderStateReadyNoTransport, model.TriNo},
	}
	for _, c := range cases {
		audio := []model.AudioStreamState{{
			StreamType: model.StreamHI, Status: model.QualExcellent, Present: true,
			DeviceConnected: true, EncoderState: c.encoder,
		}}
		r := Assemble(Inputs{Verify: cleanVerify(), Audio: audio})
		if got := r.AudioStreams[0].StreamActive; got != c.want {
			t.Errorf("encoder %q: stream_active = %q, want %q", c.encoder, got, c.want)
		}
	}
}

// TestAudioClientsAreTotalled: §8.2's "Connected audio clients" row.
func TestAudioClientsAreTotalled(t *testing.T) {
	audio := []model.AudioStreamState{
		{StreamType: model.StreamHI, Status: model.QualUsable, Present: true, ConnectedClients: 2},
		{StreamType: model.StreamVIN, Status: model.QualUsable, Present: true, ConnectedClients: 3},
	}
	r := Assemble(Inputs{Verify: cleanVerify(), Audio: audio})
	if r.ConnectedAudioClients != 5 {
		t.Errorf("connected audio clients = %d, want 5", r.ConnectedAudioClients)
	}
	if r.AudioStreams[0].InputPresent != model.TriYes {
		t.Errorf("a present input must report present, got %q", r.AudioStreams[0].InputPresent)
	}
}

// TestNoAudioMeansNoInventedRows: an input the media process never reported
// produces no row, so the dashboard says "nothing was measured" rather than
// drawing an HI row with reassuring zeroes in it.
func TestNoAudioMeansNoInventedRows(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify()})
	if len(r.AudioStreams) != 0 {
		t.Errorf("no measurements must produce no rows, got %+v", r.AudioStreams)
	}
	if r.ConnectedAudioClients != 0 {
		t.Errorf("audio clients = %d with no inputs", r.ConnectedAudioClients)
	}
}

// TestDeploymentBannerIsRaisedWhereTheProfileDemandsOne: §8.4 permits an admin
// surface on the patron network ONLY in Lab and Booth Diagnostic and ONLY behind
// a prominent warning banner. The profile descriptor has carried WarningBanner
// all along and the dashboard never rendered one, so the condition existed on
// paper only.
func TestDeploymentBannerIsRaisedWhereTheProfileDemandsOne(t *testing.T) {
	// The verification report calls this ISOLATED, because Lab imposes no
	// isolation requirement. That is exactly why the banner cannot be driven from
	// it: admin really is on the patron network here.
	rep := cleanVerify()
	rep.AdminIsolated = true
	r := Assemble(Inputs{
		Verify: rep,
		Deploy: Deploy{
			Profile: model.ProfileLab, Title: "Lab", WarningBanner: true,
			AdminOnPatronNet: model.TriYes,
		},
	})
	if r.DeploymentBanner == "" {
		t.Fatalf("a profile that requires a banner must produce one")
	}
	if !strings.Contains(r.DeploymentBanner, "PATRON") {
		t.Errorf("the banner must name the admin-on-patron-network condition, got %q", r.DeploymentBanner)
	}
	if r.AdminOnPatronNet != model.TriYes {
		t.Errorf("the raw §8.4 fact must reach the dashboard, got %q", r.AdminOnPatronNet)
	}
	if !r.AdminIsolated {
		t.Errorf("the profile-requirement verdict is a separate row and still says yes")
	}
	if r.DeploymentTitle != "Lab" {
		t.Errorf("the posture in force must be named, got %q", r.DeploymentTitle)
	}
}

// TestBannerStillWarnsWhenAdminIsIsolated: Lab and Booth Diagnostic are not
// public-install postures whether or not admin happens to be isolated today, so
// the banner does not vanish the moment one sub-condition clears.
func TestBannerStillWarnsWhenAdminIsIsolated(t *testing.T) {
	r := Assemble(Inputs{
		Verify: cleanVerify(),
		Deploy: Deploy{
			Profile: model.ProfileBoothDiagnostic, Title: "Booth Diagnostic", WarningBanner: true,
			AdminOnPatronNet: model.TriNo,
		},
	})
	if r.DeploymentBanner == "" {
		t.Fatalf("a banner profile must always carry its banner")
	}
	if strings.Contains(r.DeploymentBanner, "PATRON") {
		t.Errorf("with admin isolated the banner must not claim otherwise, got %q", r.DeploymentBanner)
	}
}

// TestProductionProfileHasNoBanner: Patron-Facing and Low-Cost Replacement carry
// no banner, and a banner that appears everywhere is a banner nobody reads.
func TestProductionProfileHasNoBanner(t *testing.T) {
	r := Assemble(Inputs{
		Verify: cleanVerify(),
		Deploy: Deploy{Profile: model.ProfilePatronFacing, Title: "Patron-Facing", WarningBanner: false},
	})
	if r.DeploymentBanner != "" {
		t.Errorf("a production profile must not display a diagnostic banner, got %q", r.DeploymentBanner)
	}
}

// TestSupportSessionShouts: §8.3's final condition is "debug capture / support
// mode active". model.WarnDebugCaptureActive was declared and no live code path
// ever raised it, so the condition was invisible on the dashboard.
func TestSupportSessionShouts(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), Support: model.TriYes})
	if !hasCode(r.Warnings, model.WarnDebugCaptureActive) {
		t.Errorf("an open support session must be visibly indicated (§9.4), got %v", codes(r.Warnings))
	}
	if r.SupportSession != model.TriYes {
		t.Errorf("the §8.2 support row must carry the state, got %q", r.SupportSession)
	}
	off := Assemble(Inputs{Verify: cleanVerify(), Support: model.TriNo})
	if hasCode(off.Warnings, model.WarnDebugCaptureActive) {
		t.Errorf("a closed support session must not shout, got %v", codes(off.Warnings))
	}
}

// TestUnobservedRowsStayUnmeasured: the Tri rows default to "not measured", so a
// caller that never fills one cannot make the dashboard print a reassuring "no".
func TestUnobservedRowsStayUnmeasured(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify()})
	for name, got := range map[string]model.Tri{
		"current_cue_visible": r.CueVisible,
		"discovery_mode":      r.DiscoveryMode,
		"support_session":     r.SupportSession,
	} {
		if got != model.TriUnknown {
			t.Errorf("%s = %q, want not-measured", name, got)
		}
		if lbl := got.Label(); lbl != "not measured" {
			t.Errorf("%s label = %q, want \"not measured\"", name, lbl)
		}
	}
}

// TestTimelineRowIsCarried: §8.2's play/pause/stop row reaches the dashboard.
func TestTimelineRowIsCarried(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), Timeline: model.TimelinePaused})
	if r.Timeline != model.TimelinePaused || r.Timeline.Label() != "paused" {
		t.Errorf("timeline row = %q, want paused", r.Timeline)
	}
	none := Assemble(Inputs{Verify: cleanVerify()})
	if none.Timeline.Label() != "no timeline" {
		t.Errorf("an undriven timeline must read \"no timeline\", got %q", none.Timeline.Label())
	}
}

// TestDriftIsShownAgainstItsBudgetAndNotAsAnAlarm: §5.5 asks diagnostics to show
// the drift number against the limit it is judged by. It stays a NUMBER: drift
// is measured at each re-anchor, so a legitimate server seek or show loop
// produces a large figure that is not a fault, and §8.3's list does not contain
// it. A banner that fires on every seek is one a booth learns to ignore.
func TestDriftIsShownAgainstItsBudgetAndNotAsAnAlarm(t *testing.T) {
	link := model.CaptionLinkState{State: model.StatePlaying, LeaseActive: true, RPLLoaded: true, DriftMSEstimate: 60000}
	r := Assemble(Inputs{Verify: cleanVerify(), Link: link, DriftBudgetMS: 200})
	if r.CaptionDriftBudgetMS != 200 {
		t.Errorf("the budget the number is judged by must reach the dashboard, got %d", r.CaptionDriftBudgetMS)
	}
	if r.CaptionLink.DriftMSEstimate != 60000 {
		t.Errorf("the measurement must reach the dashboard, got %d", r.CaptionLink.DriftMSEstimate)
	}
	if r.HasCritical() {
		t.Errorf("a large re-anchor is not a critical fault, got %v", codes(r.Warnings))
	}
}

// TestServerConnectedRowIsDerivedOnce: §8.2's "Server connected" row. Which ACSP
// states count as connected is decided in the model and carried to the page, so
// the dashboard does not re-implement the rule in JavaScript.
func TestServerConnectedRowIsDerivedOnce(t *testing.T) {
	up := Assemble(Inputs{Verify: cleanVerify(), Link: model.CaptionLinkState{State: model.StatePlaying}})
	if up.ServerConnected != model.TriYes {
		t.Errorf("a playing link is connected, got %q", up.ServerConnected)
	}
	down := Assemble(Inputs{Verify: cleanVerify(), Link: model.CaptionLinkState{State: model.StateDisconnected}})
	if down.ServerConnected != model.TriNo {
		t.Errorf("a disconnected link is not connected, got %q", down.ServerConnected)
	}
}

// TestWrongRoomOutcomeIsNeverAPassUnlessItRan: §8.5/§10.5 — four of the five
// outcomes are not a match, and each has to say WHY it is not. A guard that
// never ran rendering as "no mismatch" is precisely the green-tick fabrication
// this codebase has already been caught doing twice.
func TestWrongRoomOutcomeIsNeverAPassUnlessItRan(t *testing.T) {
	for _, check := range []string{
		model.IdentityCheckUnknown, model.IdentityCheckUnpinned, model.IdentityCheckUnverified, "",
	} {
		r := Assemble(Inputs{Verify: cleanVerify(), Network: Network{IdentityCheck: check}})
		if r.IdentityCheckOK() {
			t.Errorf("outcome %q must not count as a pass", check)
		}
		if lbl := r.IdentityCheckLabel(); !strings.Contains(lbl, "not ") {
			t.Errorf("outcome %q must explain why it is not a pass, got %q", check, lbl)
		}
	}
	match := Assemble(Inputs{Verify: cleanVerify(), Network: Network{IdentityCheck: model.IdentityCheckMatch}})
	if !match.IdentityCheckOK() {
		t.Errorf("a guard that ran and passed is a pass")
	}
	mismatch := Assemble(Inputs{Verify: cleanVerify(), Network: Network{IdentityCheck: model.IdentityCheckMismatch}})
	if !strings.Contains(mismatch.IdentityCheckLabel(), "wrong room") {
		t.Errorf("a mismatch must name the wrong-room condition, got %q", mismatch.IdentityCheckLabel())
	}
}

// TestIdentityCheckVocabularyMatchesTheACSPLayer: the dashboard model restates
// acsp.IdentityState so the web layer need not import the state machine. Drift
// between the two sets would send every outcome to the default branch, where a
// mismatch would render as a raw string instead of a wrong-room alarm.
func TestIdentityCheckVocabularyMatchesTheACSPLayer(t *testing.T) {
	pairs := [][2]string{
		{string(acsp.IdentityUnknown), model.IdentityCheckUnknown},
		{string(acsp.IdentityUnpinned), model.IdentityCheckUnpinned},
		{string(acsp.IdentityUnverified), model.IdentityCheckUnverified},
		{string(acsp.IdentityMatch), model.IdentityCheckMatch},
		{string(acsp.IdentityMismatch), model.IdentityCheckMismatch},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("identity vocabulary drift: acsp %q vs model %q", p[0], p[1])
		}
	}
}
