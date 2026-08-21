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
		{pipeline.EncoderStreaming, model.EncoderStateStreaming},
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

// TestInvariant_PatronScopeReachesTheMediaPeer: the ICE gathering scope is only
// worth having if core actually pushes it. internal/audio/webrtcpeer can confine
// gathering and cmd/captionbridge-media can apply what it is told, but neither
// knows which interface is the patron's — that lives in the config store, which
// only core reads. Without the push, an appliance ships with the mechanism
// present, the dashboard row rendering, and every patron still being offered the
// venue's docker bridges (§9.1, §6.7, DECISIONS D28).
//
// The two literals below are LOAD-BEARING: they are what makes this assertion
// mean anything, so a refactor that renames them must update this test rather
// than delete it.
func TestInvariant_PatronScopeReachesTheMediaPeer(t *testing.T) {
	src := readFile(t, filepath.Join(repoRoot(t), "cmd", "captionbridge-core", "status.go"))
	if !strings.Contains(src, "s.media.applyPatronScope(client)") {
		t.Errorf("core never pushes the configured client interface to the media peer: " +
			"ICE gathering stays unconfined however complete internal/audio/webrtcpeer is (D28)")
	}
	if !strings.Contains(src, "ICEScope:") {
		t.Errorf("the peer's measured gathering report never reaches readiness.Inputs, " +
			"so the dashboard cannot show which networks a patron was offered (§8.2)")
	}
}

// TestInvariant_TheQREncoderHasAProductionCaller: internal/qr is verified to the
// module against independent implementations, which protects nobody if the only
// thing that ever calls it is its own test. §10.4 and §7.2 want a per-show QR on
// the operator surface, and the encoder existing is not the same as an operator
// being able to hold one up to a patron's phone.
//
// The literals below are LOAD-BEARING; a refactor that renames them updates this
// test rather than deleting it.
func TestInvariant_TheQREncoderHasAProductionCaller(t *testing.T) {
	src := readFile(t, filepath.Join(repoRoot(t), "internal", "web", "session_admin.go"))
	if !strings.Contains(src, "qr.Encode(") {
		t.Errorf("nothing in the admin surface encodes a QR: §10.4's per-show code is typed, not scanned")
	}
	if !strings.Contains(src, `"/api/admin/session/qr.png"`) {
		t.Errorf("the QR image has no route, so an operator has no way to show one")
	}
	// The image is a bearer credential for the show. It must be registered on the
	// admin surface behind requireAdmin, never on the patron mux (§8.4).
	patron := readFile(t, filepath.Join(repoRoot(t), "internal", "web", "patron.go"))
	if strings.Contains(patron, "qr.png") {
		t.Errorf("the patron listener references the QR image: anything that can read it can join this show")
	}
}

// TestInvariant_ThePreShowChecklistIsServedAndPrintable: §11.5 asks for a
// codified STAFF procedure, and a derivation with no route is not a procedure.
// The printable form is part of the requirement rather than a nicety — the sheet
// has to work when the appliance is the thing that is broken.
//
// The literals below are LOAD-BEARING.
func TestInvariant_ThePreShowChecklistIsServedAndPrintable(t *testing.T) {
	admin := readFile(t, filepath.Join(repoRoot(t), "internal", "web", "session_admin.go"))
	if !strings.Contains(admin, `"/api/admin/preshow"`) {
		t.Errorf("the §11.5 checklist has no route: it is a derivation nobody can reach")
	}
	sheet := readFile(t, filepath.Join(repoRoot(t), "internal", "web", "checklist.go"))
	if !strings.Contains(sheet, `"text"`) || !strings.Contains(sheet, "renderChecklistText") {
		t.Errorf("the checklist has no printable form (§11.5 wants a staff procedure, not a JSON document)")
	}
	// The sheet must never store a staff attestation. Recording that a person
	// ticked a box the appliance cannot verify is the §10.8 button-press bug in a
	// new place, and it ages worse: last week's tick sits on tonight's page.
	if strings.Contains(sheet, "PutSetting") || strings.Contains(sheet, "LastReadinessCheck") {
		t.Errorf("the checklist writes to the config store: a staff tick must not become a stored fact")
	}
}

// TestInvariant_TheSupportWindowIsOutboundOnlyAndReachesTheRuleset: §9.4's
// support mode is the only re-entry path once SSH is disabled, which makes it
// the one feature here whose failure is a permanent way in. Two things must hold
// in the SOURCE, because both are the kind of thing a refactor quietly changes.
//
// The literals below are LOAD-BEARING.
func TestInvariant_TheSupportWindowIsOutboundOnlyAndReachesTheRuleset(t *testing.T) {
	// 1. The window is wired into the ruleset the supervisor derives. A session
	//    that opens a state variable and no rule permits nothing; one whose rule
	//    is never removed is a permanent egress hole opened by a support switch.
	status := readFile(t, filepath.Join(repoRoot(t), "cmd", "captionbridge-core", "status.go"))
	if !strings.Contains(status, "SupportEgress: s.supportEgress()") {
		t.Errorf("the support window never reaches the firewall config: the session would permit nothing")
	}
	// 2. The rule is emitted in the OUTPUT chain only. An inbound accept would
	//    turn "outbound-only" into a listening port on an appliance whose SSH is
	//    deliberately off — the exact thing §9.4 replaces.
	fw := readFile(t, filepath.Join(repoRoot(t), "internal", "security", "firewall", "firewall.go"))
	outputIdx := strings.Index(fw, "--- output chain")
	supportIdx := strings.Index(fw, "support session (§9.4")
	if outputIdx < 0 || supportIdx < 0 {
		t.Fatalf("could not locate the output chain (%d) or the support rule (%d)", outputIdx, supportIdx)
	}
	if supportIdx < outputIdx {
		t.Errorf("the support rule is emitted before the output chain: it is not outbound-only")
	}
	// 3. The ruleset REPLACES the table. Without the delete, applying appends and
	//    a closed support window's rule outlives the session that justified it —
	//    as does every other rule any configuration change was meant to remove.
	if !strings.Contains(fw, "delete table inet captionbridge") {
		t.Errorf("the generated ruleset does not clear the table it replaces: old rules would survive")
	}
}

// TestInvariant_TheApplianceCanNeverSignItsOwnUpdates: §9.4's update path is
// only signed if the appliance holds the VERIFYING half and nothing else. A box
// that could sign an update it would then accept is one whose compromise is
// permanent — one write to its trust store and every future update is whatever
// the attacker wants.
//
// This asserts on source text across the whole update path, because the failure
// is a single field or import added in good faith ("for the signing tool").
func TestInvariant_TheApplianceCanNeverSignItsOwnUpdates(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		filepath.Join("internal", "update", "bundle.go"),
		filepath.Join("internal", "update", "service.go"),
		filepath.Join("internal", "update", "slots.go"),
		filepath.Join("cmd", "captionbridge-core", "update.go"),
	} {
		src := readFile(t, filepath.Join(root, rel))
		for _, forbidden := range []string{"ed25519.Sign(", "ed25519.PrivateKey", "ed25519.GenerateKey"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s contains %q: this appliance must verify signatures and never make them", rel, forbidden)
			}
		}
	}
	// And the trusted key must come from the appliance's own store, never from
	// the bundle. A bundle checked against a key it carries verifies perfectly
	// and proves nothing.
	bundle := readFile(t, filepath.Join(root, "internal", "update", "bundle.go"))
	if !strings.Contains(bundle, "ts.lookup(m.KeyID)") {
		t.Error("the verifier does not look the key up in the appliance's trust store")
	}
}

// TestInvariant_TheUpdateServiceHasAProductionCaller: a verifier nothing calls
// protects nobody. §9.4's bundle path exists so a booth can tell a good stick
// from a bad one BEFORE a show, which requires it to be reachable.
func TestInvariant_TheUpdateServiceHasAProductionCaller(t *testing.T) {
	main := readFile(t, filepath.Join(repoRoot(t), "cmd", "captionbridge-core", "main.go"))
	if !strings.Contains(main, "Update: newUpdateService(") {
		t.Error("core never builds an update service: the verifier is unreachable from the operator surface")
	}
	web := readFile(t, filepath.Join(repoRoot(t), "internal", "web", "session_admin.go"))
	if !strings.Contains(web, `"/api/admin/update/verify"`) {
		t.Error("there is no route to verify a bundle")
	}
}

// TestInvariant_EverySettingWithAReaderHasAWriter: the sharpest defect an
// adversarial review found in the last pass was three features that could not be
// reached on a real appliance — support mode's endpoint had a reader and no
// writer, so no operator could ever open a session, and the update path's build
// number was never stamped by any build, so its downgrade refusal was dead code.
//
// Both are the house's own rule ("a capability with no production caller
// protects nobody") pointed at configuration rather than at code. A setting the
// appliance READS and nothing WRITES is a feature that works only for whoever
// edits the database by hand.
func TestInvariant_EverySettingWithAReaderHasAWriter(t *testing.T) {
	root := repoRoot(t)
	// The §9.4 support endpoint: three settings, one operator-facing writer.
	web := readFile(t, filepath.Join(root, "internal", "web", "support.go"))
	for _, setting := range []string{
		"config.SettingSupportEndpointHost",
		"config.SettingSupportEndpointPort",
		"config.SettingSupportEgressIface",
	} {
		if !strings.Contains(web, setting) {
			t.Errorf("%s has no production writer: support mode could not be opened on an appliance "+
				"an operator had actually set up", setting)
		}
	}
	if !strings.Contains(web, `"/api/admin/support/endpoint"`) &&
		!strings.Contains(readFile(t, filepath.Join(root, "internal", "web", "session_admin.go")),
			`"/api/admin/support/endpoint"`) {
		t.Error("there is no route to configure the support endpoint")
	}
}

// TestInvariant_TheBuildNumberIsStamped: §9.4's update path orders upgrades by a
// monotonic build number, and a binary that does not carry one refuses to judge
// an update at all. That refusal is correct — and it means an unstamped build
// ships an update path that cannot be used.
func TestInvariant_TheBuildNumberIsStamped(t *testing.T) {
	mk := readFile(t, filepath.Join(repoRoot(t), "Makefile"))
	if !strings.Contains(mk, "buildinfo.Build=") {
		t.Error("the Makefile does not stamp the build number: every binary it produces refuses to judge an update (D32)")
	}
	ci := readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if !strings.Contains(ci, "make build") {
		t.Error("CI builds binaries without the Makefile's ldflags, so its artifacts carry no build number")
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
