package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

// kernelFixture builds a /proc/sys/net + /sys/class/net tree with forwarding in
// the requested state, so a verification pass can run without root.
func kernelFixture(t *testing.T, forwardingOn bool) verify.Paths {
	t.Helper()
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	sys := filepath.Join(dir, "sys")
	for _, d := range []string{filepath.Join(proc, "ipv4"), filepath.Join(proc, "ipv6", "conf", "all"), sys} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	v4 := "0"
	if forwardingOn {
		v4 = "1"
	}
	write(t, filepath.Join(proc, "ipv4", "ip_forward"), v4)
	write(t, filepath.Join(proc, "ipv6", "conf", "all", "forwarding"), "0")
	return verify.Paths{ProcSysNet: proc, SysClassNet: sys, ProcNet: filepath.Join(dir, "procnet"), ResolvConf: filepath.Join(dir, "resolv.conf")}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadedRuleset is a ruleset observation with all three chains at policy drop
// and no forward accepts — the §9.1 posture.
func loadedRuleset() verify.RulesetStatus {
	return verify.RulesetStatus{
		TablePresent: true,
		Chains: map[string]verify.ChainStatus{
			verify.ChainInput:   {Present: true, PolicyDrop: true},
			verify.ChainForward: {Present: true, PolicyDrop: true},
			verify.ChainOutput:  {Present: true, PolicyDrop: true},
		},
	}
}

func testHelper(t *testing.T, allowApply bool, paths verify.Paths, rs verify.RulesetStatus, rsErr error) *helper {
	t.Helper()
	h := newHelper(logging.Nop(), allowApply)
	h.paths = paths
	h.ruleset = verify.StaticRulesetReader(rs, rsErr)
	h.applyRules = func(string) error { return nil }
	h.applySysctls = func() error { return nil }
	return h
}

// TestVerifyMeasuresTheRulesetInsteadOfTrustingAFlag: the helper's own pass must
// read the loaded ruleset. Passing --allow-apply (or answering a ping) is not
// evidence that a default-deny ruleset exists (§9.3).
func TestVerifyMeasuresTheRulesetInsteadOfTrustingAFlag(t *testing.T) {
	// allowApply=true but NOTHING is loaded: the honest answer is "inactive".
	h := testHelper(t, true, kernelFixture(t, false), verify.RulesetStatus{}, nil)
	rep := decodeReport(t, h.verify(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "1"}))
	if !rep.FirewallIntrospected {
		t.Fatalf("the pass must be an introspection, not an assertion")
	}
	if rep.FirewallActive {
		t.Errorf("no table is loaded; the firewall must not be reported active")
	}
	if !hasCode(rep.Warnings, model.WarnFirewallInactive) {
		t.Errorf("expected a firewall-inactive warning, got %v", codes(rep.Warnings))
	}

	// Same flag, a real default-deny ruleset loaded: now it is active AND proven.
	h = testHelper(t, true, kernelFixture(t, false), loadedRuleset(), nil)
	rep = decodeReport(t, h.verify(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "2"}))
	if !rep.FirewallActive || !rep.FirewallIntrospected {
		t.Errorf("a loaded default-deny table must read as active and introspected, got %+v", rep)
	}
}

// TestVerifyCarriesTheCallerFacts: the profile/credential/SSH assertions are
// vacuous unless core's facts reach the pass.
func TestVerifyCarriesTheCallerFacts(t *testing.T) {
	h := testHelper(t, false, kernelFixture(t, false), loadedRuleset(), nil)
	facts := model.PostureFacts{
		Profile:                  model.ProfilePatronFacing,
		AdminReachableFromPatron: true,
		DefaultCredentials:       true,
	}
	payload, _ := json.Marshal(facts)
	rep := decodeReport(t, h.verify(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "1", Payload: payload}))
	if !hasCode(rep.Warnings, model.WarnAdminOnPatronNet) {
		t.Errorf("admin-on-patron must fail in a production profile, got %v", codes(rep.Warnings))
	}
	if !hasCode(rep.Warnings, model.WarnDefaultPassword) {
		t.Errorf("default credentials must be reported, got %v", codes(rep.Warnings))
	}
}

// TestWatchdogReassertsAFlip: §3.3 requires the watchdog to RE-ASSERT the
// posture, not merely notice it moved.
func TestWatchdogReassertsAFlip(t *testing.T) {
	paths := kernelFixture(t, true) // forwarding has been switched back on
	h := testHelper(t, true, paths, loadedRuleset(), nil)

	sysctls, rules := 0, 0
	h.applySysctls = func() error {
		sysctls++
		// The re-assert really does put the knob back; the re-measure must see it.
		write(t, filepath.Join(paths.ProcSysNet, "ipv4", "ip_forward"), "0")
		return nil
	}
	h.applyRules = func(string) error { rules++; return nil }

	after := h.watchdogPass()
	if sysctls != 1 {
		t.Errorf("the watchdog must re-assert the forwarding sysctls, got %d calls", sysctls)
	}
	if rules != 0 {
		t.Errorf("the ruleset was intact; it must not be reinstalled, got %d calls", rules)
	}
	if !after.ForwardingDisabledV4 {
		t.Errorf("the re-measure after a successful re-assert must show forwarding disabled")
	}
}

// TestWatchdogReinstallsTheLastRequestedRuleset: when the table is gone, the
// watchdog restores the ruleset core last asked for — never one it invents.
func TestWatchdogReinstallsTheLastRequestedRuleset(t *testing.T) {
	h := testHelper(t, true, kernelFixture(t, false), verify.RulesetStatus{}, nil)

	cfg := firewall.ConfigFromInputs(firewall.Inputs{
		Profile: model.ProfileBoothDiagnostic, TheaterIface: "eth0", ClientIface: "wlan0",
		Auditorium: model.AuditoriumProfile{CinemaServerIP: "10.10.20.15"},
	})
	payload, _ := json.Marshal(cfg)
	if resp := h.apply(ipc.Message{Type: ipc.TypeFirewallApply, ID: "1", Payload: payload}); resp == nil {
		t.Fatal("apply returned nothing")
	}

	var installed string
	h.applyRules = func(script string) error { installed = script; return nil }
	h.watchdogPass()
	if installed == "" {
		t.Fatalf("the watchdog must reinstall the ruleset when the table is missing")
	}
	if want, _ := firewall.Generate(cfg); installed != want {
		t.Errorf("the watchdog reinstalled a DIFFERENT ruleset than core asked for")
	}
}

// TestWatchdogCannotReassertWithoutPermission: in dry-run the watchdog must not
// pretend to have fixed anything — and must still leave the flip in the report so
// the dashboard screams (§3.3).
func TestWatchdogCannotReassertWithoutPermission(t *testing.T) {
	h := testHelper(t, false, kernelFixture(t, true), loadedRuleset(), nil)
	calls := 0
	h.applySysctls = func() error { calls++; return nil }
	h.applyRules = func(string) error { calls++; return nil }

	rep := h.watchdogPass()
	if calls != 0 {
		t.Errorf("a dry-run helper must not touch kernel state, got %d calls", calls)
	}
	if rep.ForwardingDisabledV4 {
		t.Errorf("the flip must still be reported")
	}
	if !hasCode(rep.Warnings, model.WarnForwardingEnabled) {
		t.Errorf("expected the forwarding warning to stand, got %v", codes(rep.Warnings))
	}
}

// TestWatchdogUsesTheLastKnownFacts: carrying core's facts into the loop is what
// keeps the profile-scoped assertions from checking a zero value.
func TestWatchdogUsesTheLastKnownFacts(t *testing.T) {
	h := testHelper(t, false, kernelFixture(t, false), loadedRuleset(), nil)
	facts := model.PostureFacts{Profile: model.ProfilePatronFacing, AdminReachableFromPatron: true}
	payload, _ := json.Marshal(facts)
	h.verify(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "1", Payload: payload})

	rep := h.watchdogPass()
	if !hasCode(rep.Warnings, model.WarnAdminOnPatronNet) {
		t.Errorf("the watchdog pass must carry the last known facts, got %v", codes(rep.Warnings))
	}
}

// TestUnreadableRulesetFailsLoud: "cannot confirm" is never "fine" (D17).
func TestUnreadableRulesetFailsLoud(t *testing.T) {
	h := testHelper(t, true, kernelFixture(t, false), verify.RulesetStatus{}, os.ErrPermission)
	rep := decodeReport(t, h.verify(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "1"}))
	if rep.FirewallActive {
		t.Errorf("an unreadable ruleset must not read as active")
	}
	if rep.Pass {
		t.Errorf("an unreadable ruleset must fail the pass")
	}
}

func decodeReport(t *testing.T, m *ipc.Message) verify.Report {
	t.Helper()
	if m == nil {
		t.Fatal("no reply")
	}
	var rep verify.Report
	if err := json.Unmarshal(m.Payload, &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return rep
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
