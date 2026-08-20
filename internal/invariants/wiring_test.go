package invariants

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/audio/pipeline"
	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/readiness"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

// The invariants in this file are about WIRING: a capability that exists but has
// no production caller protects nobody. Each one guards a fact that was true of
// the shipped binaries and should not become true again — a posture asserted
// instead of measured, a cap enforced nowhere, a warning nothing receives.

// TestInvariant_FirewallPostureIsIntrospectedInBothBinaries: §9.3 says the
// posture is PROVEN by reading this appliance's own state. Both binaries that run
// a verification pass must supply a ruleset reader; neither may fall back to the
// deprecated caller-asserted flag, which is what made "Firewall: active" a
// synonym for "a helper process answered a ping".
func TestInvariant_FirewallPostureIsIntrospectedInBothBinaries(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		filepath.Join("cmd", "captionbridge-core", "firewall.go"),
		filepath.Join("cmd", "captionbridge-firewall", "main.go"),
	} {
		src := readFile(t, filepath.Join(root, rel))
		if !strings.Contains(src, "verify.NFTRulesetReader()") {
			t.Errorf("%s: the verification pass must read the loaded ruleset (§9.3)", rel)
		}
		if strings.Contains(src, "FirewallActive:") {
			t.Errorf("%s: the deprecated caller-asserted FirewallActive must not be supplied — the posture is measured", rel)
		}
	}
}

// TestInvariant_ClientCapIsWiredIntoTheFanout: D11's per-profile concurrent-client
// caps are "enforced by the fan-out backpressure", which requires the cap to
// reach the fan-out. The mechanism existing in internal/caption/fanout is not the
// same as it being in force.
func TestInvariant_ClientCapIsWiredIntoTheFanout(t *testing.T) {
	src := readFile(t, filepath.Join(repoRoot(t), "cmd", "captionbridge-core", "main.go"))
	if !strings.Contains(src, "MaxClients: deployed.MaxConcurrentClients") {
		t.Errorf("the deployment profile's client cap must reach fanout.NewHub (D11, §12)")
	}
}

// TestInvariant_CaptionWarningsHaveAChannelToTheDashboard: guardrail #7 is not
// audio-only. The ACSP machine's warning codes and the dashboard's must be the
// same strings, and a caption warning must survive Assemble.
func TestInvariant_CaptionWarningsHaveAChannelToTheDashboard(t *testing.T) {
	if acsp.WarnLeaseNearExpiry != model.WarnCaptionLeaseNearExpiry {
		t.Errorf("lease-warning code drift: acsp %q vs model %q", acsp.WarnLeaseNearExpiry, model.WarnCaptionLeaseNearExpiry)
	}
	if acsp.WarnCaptionAssetUnavailable != model.WarnCaptionAssetUnavailable {
		t.Errorf("asset-warning code drift: acsp %q vs model %q", acsp.WarnCaptionAssetUnavailable, model.WarnCaptionAssetUnavailable)
	}
	set := readiness.NewCaptionWarnings(0)
	set.Observe(model.CaptionLinkState{
		State: model.StatePlaying, Server: "srv", LeaseActive: true,
		LeaseExpiresInMS: 100, RequestIntervalMS: 15000, RPLLoaded: true,
	})
	set.Raise(model.Warning{Code: acsp.WarnLeaseNearExpiry, Severity: model.SeverityWarning, Message: "close"})
	r := readiness.Assemble(readiness.Inputs{
		Verify:          verify.Report{ForwardingDisabledV4: true, ForwardingDisabledV6: true, FirewallActive: true, FirewallIntrospected: true, NoDefaultCreds: true, SSHCorrect: true, AdminIsolated: true, Pass: true},
		CaptionWarnings: set.Snapshot(),
	})
	if !hasCode(r.Warnings, model.WarnCaptionLeaseNearExpiry) {
		t.Errorf("a caption-link warning must reach the dashboard, got %v", r.Warnings)
	}
}

// TestInvariant_EncoderStateVocabularyMatches: the dashboard names an encoder
// state without importing the media pipeline, so the two vocabularies have to
// stay identical or the "no encoder" warning silently stops firing.
func TestInvariant_EncoderStateVocabularyMatches(t *testing.T) {
	pairs := [][2]string{
		{pipeline.EncoderUnknown, model.EncoderStateUnknown},
		{pipeline.EncoderUnavailable, model.EncoderStateUnavailable},
		{pipeline.EncoderReadyNoTransport, model.EncoderStateReadyNoTransport},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("encoder-state drift: pipeline %q vs model %q", p[0], p[1])
		}
	}
}

// TestInvariant_ProcessLivenessIsReported: §3.5 — core reports each supervised
// process's liveness, and an unreachable helper is its own named fault rather
// than an unexplained audio or firewall failure.
func TestInvariant_ProcessLivenessIsReported(t *testing.T) {
	r := readiness.Assemble(readiness.Inputs{
		Processes: []model.ProcessLiveness{
			{Name: model.ProcessCore, Alive: true},
			{Name: model.ProcessMedia, Alive: false},
			{Name: model.ProcessFirewall, Alive: true},
		},
	})
	if len(r.Processes) != 3 {
		t.Fatalf("all three processes must be reported, got %v", r.Processes)
	}
	if !hasCriticalCode(r.Warnings, model.WarnProcessUnreachable) {
		t.Errorf("a dead supervised process must be a critical warning, got %v", r.Warnings)
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
