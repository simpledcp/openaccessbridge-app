package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// fixture builds a fake /proc/sys/net + /sys/class/net tree under a temp root and
// returns Paths pointing at it. It writes the forwarding files with v4/v6 content
// and, when bridge is true, creates a br0 bridge netdev plus a plain eth0 (no
// bridge subdir) so we also prove non-bridge interfaces don't trip detection.
func fixture(t *testing.T, v4, v6 string, bridge bool) Paths {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "sys", "net")
	sys := filepath.Join(root, "sys", "class", "net")

	mustMkdir(t, filepath.Join(proc, "ipv4"))
	mustMkdir(t, filepath.Join(proc, "ipv6", "conf", "all"))
	mustWrite(t, filepath.Join(proc, "ipv4", "ip_forward"), v4)
	mustWrite(t, filepath.Join(proc, "ipv6", "conf", "all", "forwarding"), v6)

	mustMkdir(t, filepath.Join(sys, "eth0")) // ordinary interface, no bridge subdir
	if bridge {
		mustMkdir(t, filepath.Join(sys, "br0", "bridge"))
	}
	return Paths{ProcSysNet: proc, SysClassNet: sys}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func codes(ws []model.Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRun(t *testing.T) {
	cases := []struct {
		name   string
		v4, v6 string
		bridge bool
		// cfg fields other than Paths.
		profile     model.FirewallProfile
		firewall    bool
		adminPatron bool
		defCreds    bool
		ssh         bool
		// expectations.
		wantDisV4    bool
		wantDisV6    bool
		wantBridges  bool
		wantAdminIso bool
		wantPass     bool
		wantCodes    []string
	}{
		{
			name: "all_clean_lab", v4: "0", v6: "0",
			profile: model.ProfileLab, firewall: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: nil,
		},
		{
			name: "all_clean_production", v4: "0", v6: "0",
			profile: model.ProfilePatronFacing, firewall: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: nil,
		},
		{
			name: "forwarding_trailing_newline_ok", v4: "0\n", v6: "0\n",
			profile: model.ProfilePatronFacing, firewall: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: nil,
		},
		{
			name: "forwarding_v4_enabled", v4: "1", v6: "0",
			profile: model.ProfilePatronFacing, firewall: true,
			wantDisV4: false, wantDisV6: true, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnForwardingEnabled},
		},
		{
			name: "forwarding_v6_enabled", v4: "0", v6: "1",
			profile: model.ProfilePatronFacing, firewall: true,
			wantDisV4: true, wantDisV6: false, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnForwardingEnabled},
		},
		{
			name: "forwarding_both_enabled_single_warning", v4: "1", v6: "1",
			profile: model.ProfilePatronFacing, firewall: true,
			wantDisV4: false, wantDisV6: false, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnForwardingEnabled},
		},
		{
			name: "bridge_present", v4: "0", v6: "0", bridge: true,
			profile: model.ProfileLab, firewall: true,
			wantDisV4: true, wantDisV6: true, wantBridges: true, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnInterfacesBridged},
		},
		{
			name: "firewall_inactive", v4: "0", v6: "0",
			profile: model.ProfileLab, firewall: false,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnFirewallInactive},
		},
		{
			name: "admin_on_patron_production", v4: "0", v6: "0",
			profile: model.ProfilePatronFacing, firewall: true, adminPatron: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: false,
			wantPass: false, wantCodes: []string{model.WarnAdminOnPatronNet},
		},
		{
			name: "admin_on_patron_low_cost_replacement", v4: "0", v6: "0",
			profile: model.ProfileLowCostReplacement, firewall: true, adminPatron: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: false,
			wantPass: false, wantCodes: []string{model.WarnAdminOnPatronNet},
		},
		{
			name: "admin_on_patron_lab_ok", v4: "0", v6: "0",
			profile: model.ProfileLab, firewall: true, adminPatron: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: nil,
		},
		{
			name: "admin_on_patron_booth_diagnostic_ok", v4: "0", v6: "0",
			profile: model.ProfileBoothDiagnostic, firewall: true, adminPatron: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: nil,
		},
		{
			name: "default_creds", v4: "0", v6: "0",
			profile: model.ProfileLab, firewall: true, defCreds: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: false, wantCodes: []string{model.WarnDefaultPassword},
		},
		{
			name: "ssh_enabled_is_noncritical", v4: "0", v6: "0",
			profile: model.ProfileLab, firewall: true, ssh: true,
			wantDisV4: true, wantDisV6: true, wantAdminIso: true,
			wantPass: true, wantCodes: []string{model.WarnSSHEnabled},
		},
		{
			name: "everything_wrong", v4: "1", v6: "1", bridge: true,
			profile: model.ProfilePatronFacing, firewall: false, adminPatron: true, defCreds: true, ssh: true,
			wantDisV4: false, wantDisV6: false, wantBridges: true, wantAdminIso: false,
			wantPass: false,
			wantCodes: []string{
				model.WarnForwardingEnabled,
				model.WarnInterfacesBridged,
				model.WarnFirewallInactive,
				model.WarnAdminOnPatronNet,
				model.WarnDefaultPassword,
				model.WarnSSHEnabled,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Paths:                    fixture(t, tc.v4, tc.v6, tc.bridge),
				Profile:                  tc.profile,
				FirewallActive:           tc.firewall,
				AdminReachableFromPatron: tc.adminPatron,
				DefaultCredentials:       tc.defCreds,
				SSHEnabledOutsideSupport: tc.ssh,
			}
			rep := Run(cfg)

			if rep.ForwardingDisabledV4 != tc.wantDisV4 {
				t.Errorf("ForwardingDisabledV4 = %v, want %v", rep.ForwardingDisabledV4, tc.wantDisV4)
			}
			if rep.ForwardingDisabledV6 != tc.wantDisV6 {
				t.Errorf("ForwardingDisabledV6 = %v, want %v", rep.ForwardingDisabledV6, tc.wantDisV6)
			}
			if rep.BridgesPresent != tc.wantBridges {
				t.Errorf("BridgesPresent = %v, want %v", rep.BridgesPresent, tc.wantBridges)
			}
			if rep.AdminIsolated != tc.wantAdminIso {
				t.Errorf("AdminIsolated = %v, want %v", rep.AdminIsolated, tc.wantAdminIso)
			}
			if rep.FirewallActive != tc.firewall {
				t.Errorf("FirewallActive = %v, want %v", rep.FirewallActive, tc.firewall)
			}
			if rep.NoDefaultCreds != !tc.defCreds {
				t.Errorf("NoDefaultCreds = %v, want %v", rep.NoDefaultCreds, !tc.defCreds)
			}
			if rep.SSHCorrect != !tc.ssh {
				t.Errorf("SSHCorrect = %v, want %v", rep.SSHCorrect, !tc.ssh)
			}
			if rep.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v", rep.Pass, tc.wantPass)
			}
			if got := codes(rep.Warnings); !equalStrings(got, tc.wantCodes) {
				t.Errorf("warning codes = %v, want %v", got, tc.wantCodes)
			}
		})
	}
}

// TestSeverities pins the severity of each warning code: every isolation/forwarding/
// firewall/admin/creds failure is critical, SSH is a non-critical warning.
func TestSeverities(t *testing.T) {
	cfg := Config{
		Paths:                    fixture(t, "1", "1", true),
		Profile:                  model.ProfilePatronFacing,
		FirewallActive:           false,
		AdminReachableFromPatron: true,
		DefaultCredentials:       true,
		SSHEnabledOutsideSupport: true,
	}
	rep := Run(cfg)

	want := map[string]string{
		model.WarnForwardingEnabled: model.SeverityCritical,
		model.WarnInterfacesBridged: model.SeverityCritical,
		model.WarnFirewallInactive:  model.SeverityCritical,
		model.WarnAdminOnPatronNet:  model.SeverityCritical,
		model.WarnDefaultPassword:   model.SeverityCritical,
		model.WarnSSHEnabled:        model.SeverityWarning,
	}
	for _, w := range rep.Warnings {
		exp, ok := want[w.Code]
		if !ok {
			t.Errorf("unexpected warning code %q", w.Code)
			continue
		}
		if w.Severity != exp {
			t.Errorf("code %q severity = %q, want %q", w.Code, w.Severity, exp)
		}
		if w.Message == "" {
			t.Errorf("code %q has empty message", w.Code)
		}
	}
}

// TestForwardingMissingFilesFailLoud ensures unreadable sysctl files are treated
// as NOT disabled (a surfaced problem), never as a silent pass.
func TestForwardingMissingFilesFailLoud(t *testing.T) {
	root := t.TempDir()
	p := Paths{
		ProcSysNet:  filepath.Join(root, "proc", "sys", "net"), // does not exist
		SysClassNet: filepath.Join(root, "sys", "class", "net"),
	}
	mustMkdir(t, p.SysClassNet)

	rep := Run(Config{Paths: p, Profile: model.ProfileLab, FirewallActive: true})
	if rep.ForwardingDisabledV4 || rep.ForwardingDisabledV6 {
		t.Errorf("missing files should not be reported as disabled: v4=%v v6=%v",
			rep.ForwardingDisabledV4, rep.ForwardingDisabledV6)
	}
	if rep.Pass {
		t.Error("Pass should be false when forwarding cannot be confirmed disabled")
	}
	if got := codes(rep.Warnings); !equalStrings(got, []string{model.WarnForwardingEnabled}) {
		t.Errorf("warning codes = %v, want [%s]", got, model.WarnForwardingEnabled)
	}
}

// TestBridgesMissingSysDir ensures a missing /sys/class/net does not report a
// bridge (no false positive when introspection is unavailable).
func TestBridgesMissingSysDir(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "sys", "net")
	mustMkdir(t, filepath.Join(proc, "ipv4"))
	mustMkdir(t, filepath.Join(proc, "ipv6", "conf", "all"))
	mustWrite(t, filepath.Join(proc, "ipv4", "ip_forward"), "0")
	mustWrite(t, filepath.Join(proc, "ipv6", "conf", "all", "forwarding"), "0")
	p := Paths{ProcSysNet: proc, SysClassNet: filepath.Join(root, "sys", "class", "net")}

	rep := Run(Config{Paths: p, Profile: model.ProfileLab, FirewallActive: true})
	if rep.BridgesPresent {
		t.Error("BridgesPresent should be false when /sys/class/net is absent")
	}
	if !rep.Pass {
		t.Error("clean config with missing sys dir should still pass")
	}
}

func TestDefaultPaths(t *testing.T) {
	p := DefaultPaths()
	if p.ProcSysNet != "/proc/sys/net" {
		t.Errorf("ProcSysNet = %q, want /proc/sys/net", p.ProcSysNet)
	}
	if p.SysClassNet != "/sys/class/net" {
		t.Errorf("SysClassNet = %q, want /sys/class/net", p.SysClassNet)
	}
}

// TestEmptyPathsFallBackToDefaults ensures Run fills empty Paths from
// DefaultPaths rather than building relative paths. We only assert it does not
// panic and returns a populated report; values depend on the host kernel.
func TestEmptyPathsFallBackToDefaults(t *testing.T) {
	rep := Run(Config{Profile: model.ProfileLab, FirewallActive: true})
	// Pass is host-dependent, but the call must complete and the firewall fact
	// must round-trip regardless of the kernel state.
	if !rep.FirewallActive {
		t.Error("FirewallActive should round-trip from Config")
	}
}
