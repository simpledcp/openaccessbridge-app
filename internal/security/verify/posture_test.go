package verify

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// knob is one forwarding sysctl in a fixture tree, named the sysctl way
// ("ipv6/conf/eth0/forwarding") with the value it should read.
type knob struct {
	path  string
	value string
}

// forwardingFixture builds a /proc/sys/net tree containing exactly the knobs
// given, plus an empty /sys/class/net so the bridge scan can be performed.
func forwardingFixture(t *testing.T, knobs ...knob) Paths {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "sys", "net")
	sys := filepath.Join(root, "sys", "class", "net")
	mustMkdir(t, filepath.Join(sys, "eth0"))
	for _, k := range knobs {
		full := filepath.Join(proc, filepath.FromSlash(k.path))
		mustMkdir(t, filepath.Dir(full))
		mustWrite(t, full, k.value)
	}
	return Paths{ProcSysNet: proc, SysClassNet: sys}
}

// TestForwarding_PerInterfaceAndDefaultKnobs closes the hole the global knobs
// leave: §9.1 requires "kernel forwarding disabled for IPv4 and IPv6", and
// forwarding is a PER-DEVICE property. net.ipv6.conf.eth0.forwarding=1 forwards
// on that device while conf/all/forwarding still reads 0, and a NIC appearing
// after boot inherits conf/default/forwarding.
func TestForwarding_PerInterfaceAndDefaultKnobs(t *testing.T) {
	allZero := []knob{
		{"ipv4/ip_forward", "0"},
		{"ipv4/conf/all/forwarding", "0"},
		{"ipv4/conf/default/forwarding", "0"},
		{"ipv4/conf/eth0/forwarding", "0"},
		{"ipv4/conf/wlan0/forwarding", "0"},
		{"ipv6/conf/all/forwarding", "0"},
		{"ipv6/conf/default/forwarding", "0"},
		{"ipv6/conf/eth0/forwarding", "0"},
		{"ipv6/conf/wlan0/forwarding", "0"},
	}
	with := func(path, value string) []knob {
		out := make([]knob, len(allZero))
		copy(out, allZero)
		for i := range out {
			if out[i].path == path {
				out[i].value = value
				return out
			}
		}
		return append(out, knob{path, value})
	}

	cases := []struct {
		name      string
		knobs     []knob
		wantV4    bool
		wantV6    bool
		wantPass  bool
		wantNamed string // substring the warning must name, so the operator knows WHICH knob
	}{
		{name: "everything zero", knobs: allZero, wantV4: true, wantV6: true, wantPass: true},
		{
			name:   "ipv6 per-interface enable while all reads 0",
			knobs:  with("ipv6/conf/eth0/forwarding", "1"),
			wantV4: true, wantV6: false, wantPass: false, wantNamed: "ipv6/conf/eth0/forwarding",
		},
		{
			name:   "ipv4 per-interface enable while ip_forward reads 0",
			knobs:  with("ipv4/conf/wlan0/forwarding", "1"),
			wantV4: false, wantV6: true, wantPass: false, wantNamed: "ipv4/conf/wlan0/forwarding",
		},
		{
			name:   "ipv6 default inherited by a future NIC",
			knobs:  with("ipv6/conf/default/forwarding", "1"),
			wantV4: true, wantV6: false, wantPass: false, wantNamed: "ipv6/conf/default/forwarding",
		},
		{
			name:   "ipv4 default inherited by a future NIC",
			knobs:  with("ipv4/conf/default/forwarding", "1"),
			wantV4: false, wantV6: true, wantPass: false, wantNamed: "ipv4/conf/default/forwarding",
		},
		{
			name:   "global ipv4 knob still checked",
			knobs:  with("ipv4/ip_forward", "1"),
			wantV4: false, wantV6: true, wantPass: false, wantNamed: "ipv4/ip_forward",
		},
		{
			name:   "trailing newline is fine",
			knobs:  with("ipv6/conf/eth0/forwarding", "0\n"),
			wantV4: true, wantV6: true, wantPass: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Run(Config{
				Paths:          forwardingFixture(t, tc.knobs...),
				Profile:        model.ProfileBoothDiagnostic,
				FirewallActive: true,
			})
			if rep.ForwardingDisabledV4 != tc.wantV4 {
				t.Errorf("ForwardingDisabledV4 = %v, want %v", rep.ForwardingDisabledV4, tc.wantV4)
			}
			if rep.ForwardingDisabledV6 != tc.wantV6 {
				t.Errorf("ForwardingDisabledV6 = %v, want %v", rep.ForwardingDisabledV6, tc.wantV6)
			}
			if rep.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (warnings %v)", rep.Pass, tc.wantPass, rep.Warnings)
			}
			if tc.wantNamed == "" {
				return
			}
			if !contains(rep.ForwardingKnobsEnabled, tc.wantNamed) {
				t.Errorf("ForwardingKnobsEnabled = %v, want it to name %q", rep.ForwardingKnobsEnabled, tc.wantNamed)
			}
			var msg string
			for _, w := range rep.Warnings {
				if w.Code == model.WarnForwardingEnabled {
					msg = w.Message
				}
			}
			if !strings.Contains(msg, tc.wantNamed) {
				t.Errorf("warning message %q should name the offending knob %q", msg, tc.wantNamed)
			}
		})
	}
}

// TestForwardingKnobs_Enumeration: the enumeration must cover every per-device
// knob present, including "all" and "default", without duplicates — it is also
// what the privileged helper re-asserts against.
func TestForwardingKnobs_Enumeration(t *testing.T) {
	p := forwardingFixture(t,
		knob{"ipv4/ip_forward", "0"},
		knob{"ipv4/conf/all/forwarding", "0"},
		knob{"ipv4/conf/default/forwarding", "0"},
		knob{"ipv4/conf/eth0/forwarding", "0"},
		knob{"ipv6/conf/all/forwarding", "0"},
		knob{"ipv6/conf/default/forwarding", "0"},
		knob{"ipv6/conf/eth0/forwarding", "0"},
	)
	v4, v6 := ForwardingKnobs(p.ProcSysNet)
	wantV4 := []string{"ipv4/ip_forward", "ipv4/conf/all/forwarding", "ipv4/conf/default/forwarding", "ipv4/conf/eth0/forwarding"}
	wantV6 := []string{"ipv6/conf/all/forwarding", "ipv6/conf/default/forwarding", "ipv6/conf/eth0/forwarding"}
	assertKnobSet(t, "v4", p.ProcSysNet, v4, wantV4)
	assertKnobSet(t, "v6", p.ProcSysNet, v6, wantV6)
}

func assertKnobSet(t *testing.T, label, root string, got, want []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, g := range got {
		rel, err := filepath.Rel(root, g)
		if err != nil {
			t.Fatalf("%s: %q is not under %q", label, g, root)
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			t.Errorf("%s: duplicate knob %q", label, rel)
		}
		seen[rel] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("%s: missing knob %q (got %v)", label, w, got)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("%s: got %d knobs, want %d: %v", label, len(seen), len(want), got)
	}
}

// TestForwarding_MissingIPv6TreeStaysSecure: an IPv6-off kernel (ipv6.disable=1)
// has no forwarding knobs at all — forwarding is impossible, the secure state
// (D17). It must not raise a false critical on a hardened appliance.
func TestForwarding_MissingIPv6TreeStaysSecure(t *testing.T) {
	rep := Run(Config{
		Paths: forwardingFixture(t,
			knob{"ipv4/ip_forward", "0"},
			knob{"ipv4/conf/all/forwarding", "0"},
		),
		Profile:        model.ProfilePatronFacing,
		FirewallActive: true,
	})
	if !rep.ForwardingDisabledV6 {
		t.Error("an absent IPv6 forwarding tree means IPv6 is off, i.e. disabled")
	}
	if !rep.Pass || len(rep.Warnings) != 0 {
		t.Errorf("IPv6-disabled-at-kernel must not warn: %v", rep.Warnings)
	}
}

// TestRefusesSetupCompletion implements §9.3's "for the locked profiles, can
// refuse to leave setup": the locked profiles block on a critical failure, the
// others stay loud-but-passable, and nothing blocks on a clean report.
func TestRefusesSetupCompletion(t *testing.T) {
	critical := Report{Warnings: []model.Warning{{Code: model.WarnFirewallInactive, Severity: model.SeverityCritical}}}
	warning := Report{Pass: true, Warnings: []model.Warning{{Code: model.WarnSSHEnabled, Severity: model.SeverityWarning}}}
	clean := Report{Pass: true}

	cases := []struct {
		name    string
		rep     Report
		profile model.FirewallProfile
		want    bool
	}{
		{"lab with a critical failure does not block", critical, model.ProfileLab, false},
		{"booth diagnostic with a critical failure does not block", critical, model.ProfileBoothDiagnostic, false},
		{"patron facing blocks", critical, model.ProfilePatronFacing, true},
		{"low cost replacement blocks", critical, model.ProfileLowCostReplacement, true},
		{"unknown profile blocks conservatively", critical, model.FirewallProfile("mystery"), true},
		{"warning-only report never blocks", warning, model.ProfilePatronFacing, false},
		{"clean report never blocks", clean, model.ProfileLowCostReplacement, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.RefusesSetupCompletion(tc.profile); got != tc.want {
				t.Errorf("RefusesSetupCompletion(%s) = %v, want %v", tc.profile, got, tc.want)
			}
		})
	}
}

// TestBlockingCodes names the offending conditions so a refusal can say why, and
// lists only the critical ones (a warning must not strand an installer).
func TestBlockingCodes(t *testing.T) {
	rep := Report{Warnings: []model.Warning{
		{Code: model.WarnForwardingEnabled, Severity: model.SeverityCritical},
		{Code: model.WarnSSHEnabled, Severity: model.SeverityWarning},
		{Code: model.WarnFirewallInactive, Severity: model.SeverityCritical},
		{Code: model.WarnForwardingEnabled, Severity: model.SeverityCritical},
	}}
	got := rep.BlockingCodes()
	want := []string{model.WarnForwardingEnabled, model.WarnFirewallInactive}
	if !equalStrings(got, want) {
		t.Errorf("BlockingCodes() = %v, want %v", got, want)
	}
	if len((Report{Pass: true}).BlockingCodes()) != 0 {
		t.Error("a clean report has no blocking codes")
	}
}

// TestRefusesSetupCompletion_EndToEnd wires the real pass to the gate: a locked
// profile with forwarding enabled must be refused, and told which condition did
// it — the check §9.3 requires before an install may leave setup.
func TestRefusesSetupCompletion_EndToEnd(t *testing.T) {
	rep := Run(Config{
		Paths: forwardingFixture(t,
			knob{"ipv4/ip_forward", "1"},
			knob{"ipv6/conf/all/forwarding", "0"},
		),
		Profile: model.ProfilePatronFacing,
		Ruleset: parseReader(t, compliant()),
	})
	if !rep.RefusesSetupCompletion(model.ProfilePatronFacing) {
		t.Fatalf("a locked profile with forwarding on must refuse to leave setup: %+v", rep)
	}
	if !contains(rep.BlockingCodes(), model.WarnForwardingEnabled) {
		t.Errorf("the refusal should name the forwarding condition, got %v", rep.BlockingCodes())
	}
	if rep.RefusesSetupCompletion(model.ProfileLab) {
		t.Error("the same posture must not strand a Lab bench")
	}
}
