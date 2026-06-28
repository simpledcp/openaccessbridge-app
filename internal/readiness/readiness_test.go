package readiness

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

func cleanVerify() verify.Report {
	return verify.Report{
		ForwardingDisabledV4: true, ForwardingDisabledV6: true,
		FirewallActive: true, AdminIsolated: true, NoDefaultCreds: true, SSHCorrect: true, Pass: true,
	}
}

// TestIndependentReadiness proves captions and audio are computed independently:
// valid captions + a failed VI feed is caption_ready=true, audio_ready=false.
func TestIndependentReadiness(t *testing.T) {
	link := model.CaptionLinkState{State: model.StatePlaying, LeaseActive: true, RPLLoaded: true}
	audio := []model.AudioStreamState{
		{StreamType: model.StreamHI, Status: model.QualUsable, LevelDBFS: -18},
		{StreamType: model.StreamVIN, Status: model.QualFailed, Silence: true, Reasons: []string{"configured input is silent during playback"}},
	}
	r := Assemble(link, audio, cleanVerify(), Network{}, DefaultLatencyBudgets())
	if !r.CaptionReady {
		t.Errorf("captions should be ready")
	}
	if r.AudioReady {
		t.Errorf("audio should NOT be ready with a failed VI feed")
	}
	// A silent stream raises a critical loud warning (§8.3).
	if !hasCode(r.Warnings, model.WarnAudioSilentInPlayback) {
		t.Errorf("expected silent-input warning, got %v", codes(r.Warnings))
	}
	if !r.HasCritical() {
		t.Errorf("a silent VI feed during playback is critical")
	}
}

// TestCleanAudioReady: two usable streams, no failures → audio ready, no warnings.
func TestCleanAudioReady(t *testing.T) {
	audio := []model.AudioStreamState{
		{StreamType: model.StreamHI, Status: model.QualExcellent, LevelDBFS: -16, LatencyMSEst: 60},
		{StreamType: model.StreamVIN, Status: model.QualUsable, LevelDBFS: -20, LatencyMSEst: 90},
	}
	r := Assemble(model.CaptionLinkState{}, audio, cleanVerify(), Network{}, DefaultLatencyBudgets())
	if !r.AudioReady {
		t.Errorf("two usable streams should be audio_ready")
	}
	if len(r.Warnings) != 0 {
		t.Errorf("clean audio should have no warnings, got %v", codes(r.Warnings))
	}
}

// TestSecurityWarningsPropagate: a forwarding-enabled report surfaces as a critical
// dashboard warning and forces caption/audio independence to still hold.
func TestSecurityWarningsPropagate(t *testing.T) {
	rep := verify.Report{
		ForwardingDisabledV4: false, ForwardingDisabledV6: true,
		FirewallActive: true, AdminIsolated: true, NoDefaultCreds: true, SSHCorrect: true,
		Warnings: []model.Warning{{Code: model.WarnForwardingEnabled, Severity: model.SeverityCritical, Message: "x"}},
		Pass:     false,
	}
	r := Assemble(model.CaptionLinkState{}, nil, rep, Network{TheaterInterface: "eth0"}, DefaultLatencyBudgets())
	if r.ForwardingDisabled {
		t.Errorf("forwarding should be reported not-fully-disabled")
	}
	if !hasCode(r.Warnings, model.WarnForwardingEnabled) {
		t.Errorf("forwarding warning should propagate")
	}
	if r.TheaterInterface != "eth0" {
		t.Errorf("network facts should pass through")
	}
}

// TestClippingAndLatencyWarnings.
func TestClippingAndLatencyWarnings(t *testing.T) {
	audio := []model.AudioStreamState{
		{StreamType: model.StreamHI, Status: model.QualMarginal, Clipping: true, LatencyMSEst: 400},
	}
	r := Assemble(model.CaptionLinkState{}, audio, cleanVerify(), Network{}, DefaultLatencyBudgets())
	if !hasCode(r.Warnings, model.WarnAudioClipping) {
		t.Errorf("expected clipping warning")
	}
	if !hasCode(r.Warnings, model.WarnAudioLatencyOver) {
		t.Errorf("expected latency-over warning (400ms > 150ms HI budget)")
	}
}

func hasCode(ws []model.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}
func codes(ws []model.Warning) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Code
	}
	return out
}
