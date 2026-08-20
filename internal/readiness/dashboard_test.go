package readiness

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// TestCaptionWarningsReachTheDashboard: guardrail #7 is not audio-only. A
// caption-path condition raised by the ACSP link has to appear in the composite
// warning set, or a caption failure is silent while an audio failure is loud.
func TestCaptionWarningsReachTheDashboard(t *testing.T) {
	cw := []model.Warning{
		{Code: model.WarnCaptionLeaseNearExpiry, Severity: model.SeverityWarning, Message: "close to expiry"},
		{Code: model.WarnServerIdentityMismatch, Severity: model.SeverityCritical, Message: "wrong room"},
	}
	r := Assemble(Inputs{
		Link:            model.CaptionLinkState{State: model.StatePlaying, LeaseActive: true, RPLLoaded: true},
		Verify:          cleanVerify(),
		CaptionWarnings: cw,
	})
	if !hasCode(r.Warnings, model.WarnCaptionLeaseNearExpiry) {
		t.Errorf("lease-near-expiry must reach the dashboard, got %v", codes(r.Warnings))
	}
	if !hasCode(r.Warnings, model.WarnServerIdentityMismatch) {
		t.Errorf("server-identity mismatch must reach the dashboard, got %v", codes(r.Warnings))
	}
	if !r.HasCritical() {
		t.Errorf("a wrong-room mismatch is critical")
	}
}

// TestImpossibleMappingMatchesOnlyTheMappingFault: §8.3's mapping warning is
// specifically "a configured stereo mapping impossible on a detected mono input".
// A reason that merely contains the word "requires" is not that fault and must
// not raise a CRITICAL mapping alarm.
func TestImpossibleMappingMatchesOnlyTheMappingFault(t *testing.T) {
	notMapping := []model.AudioStreamState{{
		StreamType: model.StreamHI, Status: model.QualMarginal, DeviceConnected: true,
		Reasons: []string{"capture format requires 48 kHz"},
	}}
	if r := Assemble(Inputs{Audio: notMapping, Verify: cleanVerify()}); hasCode(r.Warnings, model.WarnImpossibleMapping) {
		t.Errorf("a non-mapping reason must not raise the mapping fault, got %v", codes(r.Warnings))
	}
	mapping := []model.AudioStreamState{{
		StreamType: model.StreamHI, Status: model.QualFailed, DeviceConnected: true,
		Reasons: []string{"stereo split requires two channels, device has 1"},
	}}
	if r := Assemble(Inputs{Audio: mapping, Verify: cleanVerify()}); !hasCode(r.Warnings, model.WarnImpossibleMapping) {
		t.Errorf("the real mapping fault must still raise, got %v", codes(r.Warnings))
	}
}

// TestDisconnectedDeviceIsNamed: §6.6 reports device connect/disconnect; the
// booth must be told "the dongle fell out", not just "audio not ready".
func TestDisconnectedDeviceIsNamed(t *testing.T) {
	audio := []model.AudioStreamState{{
		StreamType: model.StreamHI, Status: model.QualFailed, DeviceConnected: false,
		Reasons: []string{"capture device stopped delivering audio"},
	}}
	r := Assemble(Inputs{Audio: audio, Verify: cleanVerify()})
	if r.AudioReady {
		t.Errorf("a failed stream is not audio_ready")
	}
	if !hasCode(r.Warnings, model.WarnAudioDeviceDisconnected) {
		t.Errorf("expected a disconnected-device warning, got %v", codes(r.Warnings))
	}
}

// TestUnavailableEncoderIsNamed: a clean input with no encoder delivers silence
// to patrons. The dashboard must say so rather than report a healthy input.
func TestUnavailableEncoderIsNamed(t *testing.T) {
	audio := []model.AudioStreamState{{
		StreamType: model.StreamVIN, Status: model.QualExcellent, DeviceConnected: true,
		EncoderState: model.EncoderStateUnavailable,
	}}
	r := Assemble(Inputs{Audio: audio, Verify: cleanVerify()})
	if !hasCode(r.Warnings, model.WarnAudioEncoderUnavailable) {
		t.Errorf("expected an encoder-unavailable warning, got %v", codes(r.Warnings))
	}
	// A working encoder says nothing.
	audio[0].EncoderState = model.EncoderStateReadyNoTransport
	if r := Assemble(Inputs{Audio: audio, Verify: cleanVerify()}); hasCode(r.Warnings, model.WarnAudioEncoderUnavailable) {
		t.Errorf("a ready encoder must not warn, got %v", codes(r.Warnings))
	}
}

// TestAssertedFirewallIsNotAVerifiedFirewall: a posture that was claimed but
// never measured must be reported as unverified — and must NOT be reported as
// "firewall down", which would send a tech chasing the wrong fault (§9.3).
func TestAssertedFirewallIsNotAVerifiedFirewall(t *testing.T) {
	rep := cleanVerify()
	rep.FirewallIntrospected = false
	r := Assemble(Inputs{Verify: rep})
	if !r.FirewallActive {
		t.Errorf("the caller's claim still stands as a claim")
	}
	if r.FirewallIntrospected {
		t.Errorf("nothing read the ruleset; the dashboard must not say it did")
	}
	if !hasCode(r.Warnings, model.WarnFirewallUnverified) {
		t.Errorf("expected an unverified-posture warning, got %v", codes(r.Warnings))
	}
	if hasCode(r.Warnings, model.WarnFirewallInactive) {
		t.Errorf("unverified must not be reported as inactive")
	}
}

// TestProcessLivenessSurfaces: §3.5 — core reports each supervised process's
// liveness, and a helper that is not answering is a named critical fault rather
// than an unexplained "audio not ready".
func TestProcessLivenessSurfaces(t *testing.T) {
	procs := []model.ProcessLiveness{
		{Name: model.ProcessCore, Alive: true},
		{Name: model.ProcessMedia, Alive: false, Detail: "dial_failed"},
		{Name: model.ProcessFirewall, Alive: true},
	}
	r := Assemble(Inputs{Verify: cleanVerify(), Processes: procs})
	if len(r.Processes) != 3 {
		t.Fatalf("expected three processes reported, got %d", len(r.Processes))
	}
	if !hasCode(r.Warnings, model.WarnProcessUnreachable) {
		t.Errorf("a dead helper must raise a loud warning, got %v", codes(r.Warnings))
	}
	if !r.HasCritical() {
		t.Errorf("a dead supervised process is critical")
	}
}

// TestClientCountsAndVersionsSurface: §8.2's client counts, the D11 cap actually
// in force, and the §10.8 versioning record all reach the dashboard.
func TestClientCountsAndVersionsSurface(t *testing.T) {
	v := model.SystemConfigVersion{ConfigSchemaVersion: 1, FirewallProfileVersion: "booth_diagnostic@abc123"}
	r := Assemble(Inputs{Verify: cleanVerify(), Clients: Clients{Caption: 4, Max: 25}, Versions: v})
	if r.ConnectedCaptionClients != 4 || r.MaxConcurrentClients != 25 {
		t.Errorf("client counts should pass through, got %d/%d", r.ConnectedCaptionClients, r.MaxConcurrentClients)
	}
	if r.Versions.FirewallProfileVersion != "booth_diagnostic@abc123" {
		t.Errorf("the §10.8 record should be surfaced, got %+v", r.Versions)
	}
}

// TestPinnedServerIsTheConfiguredOne: the dashboard's "allowed cinema server" is
// the CONFIGURED allowlist entry (§8.2/§10.5). Showing the connected server's own
// claim there would make a wrong-room mismatch impossible to see (§8.5).
func TestPinnedServerIsTheConfiguredOne(t *testing.T) {
	net := Network{
		AllowedServer: "10.10.20.15", ConnectedServer: "some-other-server",
		IdentityStrength: model.IdentityComposite,
	}
	r := Assemble(Inputs{Verify: cleanVerify(), Network: net})
	if r.AllowedServer != "10.10.20.15" {
		t.Errorf("allowed server must be the configured one, got %q", r.AllowedServer)
	}
	if r.ConnectedServer != "some-other-server" {
		t.Errorf("the connected server is reported separately, got %q", r.ConnectedServer)
	}
	if got := r.IdentityStrengthLabel(); !strings.Contains(got, "composite") || !strings.Contains(got, "not authentication") {
		t.Errorf("a composite identity must be labelled as such (§10.5), got %q", got)
	}
}
