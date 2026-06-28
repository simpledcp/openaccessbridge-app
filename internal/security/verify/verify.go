// Package verify performs install-time and runtime SELF-INTROSPECTION
// verification of the appliance's network posture (buildplan.md §9.3).
//
// It asserts that CaptionBridge behaves as an application-layer bridge and never
// an IP-layer device: IPv4 and IPv6 forwarding disabled, no bridge netdevs, a
// default-deny firewall active, the admin surface not reachable from the patron
// network (in production profiles), no default credentials, and SSH state correct
// for the profile. Any failure becomes a loud dashboard warning (§8.3), and for
// the locked profiles a critical failure is grounds to refuse to leave setup.
//
// CRITICAL DESIGN CONSTRAINT (§9.3): this package verifies the appliance by
// INTROSPECTING ITS OWN kernel state — its own /proc/sys forwarding flags, its
// own /sys/class/net bridge devices, and caller-supplied facts about its own
// firewall and admin bindings. It NEVER scans the theater subnet or probes other
// hosts; the appliance must never behave like a network scanner on a cinema LAN.
// All filesystem roots are injectable so tests run against fixtures.
package verify

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/simpledcp/captionbridge/internal/model"
)

// Paths locates the kernel introspection trees. They are injectable so tests can
// point them at fixtures; production uses DefaultPaths.
type Paths struct {
	// ProcSysNet is the root of the networking sysctl tree (default "/proc/sys/net").
	ProcSysNet string
	// SysClassNet is the netdev directory (default "/sys/class/net").
	SysClassNet string
}

// DefaultPaths returns the standard Linux introspection roots.
func DefaultPaths() Paths {
	return Paths{
		ProcSysNet:  "/proc/sys/net",
		SysClassNet: "/sys/class/net",
	}
}

// Config is the input to a verification pass. The boolean facts (firewall, admin
// reachability, credentials, SSH) are supplied by the caller — typically the
// firewall helper that owns nftables and the admin-bind configuration — because
// they are properties of the appliance's own configuration, not of the network.
type Config struct {
	Paths                    Paths
	Profile                  model.FirewallProfile
	FirewallActive           bool
	AdminReachableFromPatron bool
	DefaultCredentials       bool
	SSHEnabledOutsideSupport bool
}

// Report is the outcome of a verification pass. The booleans are positive
// assertions (true = the secure condition holds); Warnings carries the loud
// conditions and Pass is false if any critical warning is present.
type Report struct {
	ForwardingDisabledV4 bool
	ForwardingDisabledV6 bool
	BridgesPresent       bool
	AdminIsolated        bool
	FirewallActive       bool
	NoDefaultCreds       bool
	SSHCorrect           bool
	Warnings             []model.Warning
	Pass                 bool
}

// Run executes a verification pass against the configured paths and facts. It
// only reads the appliance's own kernel state and never touches the network.
func Run(cfg Config) Report {
	p := cfg.Paths
	if p.ProcSysNet == "" {
		p.ProcSysNet = DefaultPaths().ProcSysNet
	}
	if p.SysClassNet == "" {
		p.SysClassNet = DefaultPaths().SysClassNet
	}

	// IPv4 ip_forward essentially always exists; a MISSING file is suspicious and
	// fails loud. IPv6 forwarding is impossible when IPv6 is disabled at the kernel
	// (ipv6.disable=1) or compiled out — then the file is absent, which is the
	// SECURE state, not "forwarding enabled". Treating a missing v6 file as a
	// failure would spam a false critical warning on hardened IPv6-off appliances.
	disabledV4 := forwardingDisabled(filepath.Join(p.ProcSysNet, "ipv4", "ip_forward"), false)
	disabledV6 := forwardingDisabled(filepath.Join(p.ProcSysNet, "ipv6", "conf", "all", "forwarding"), true)
	bridges := bridgesPresent(p.SysClassNet)

	// In production the admin surface must be off the patron network; in the known
	// non-production profiles (Lab, Booth Diagnostic) admin-on-patron is permitted
	// behind a banner (§8.4). An UNKNOWN/empty profile requires isolation
	// conservatively (never fail open on an unrecognized posture).
	requiresIsolation := adminIsolationRequired(cfg.Profile)
	adminIsolated := !requiresIsolation || !cfg.AdminReachableFromPatron

	rep := Report{
		ForwardingDisabledV4: disabledV4,
		ForwardingDisabledV6: disabledV6,
		BridgesPresent:       bridges,
		AdminIsolated:        adminIsolated,
		FirewallActive:       cfg.FirewallActive,
		NoDefaultCreds:       !cfg.DefaultCredentials,
		SSHCorrect:           !cfg.SSHEnabledOutsideSupport,
	}

	var warnings []model.Warning
	if !disabledV4 || !disabledV6 {
		which := "IPv4"
		if !disabledV4 && !disabledV6 {
			which = "IPv4 and IPv6"
		} else if !disabledV6 {
			which = "IPv6"
		}
		warnings = append(warnings, model.Warning{
			Code:     model.WarnForwardingEnabled,
			Message:  which + " forwarding is enabled (or cannot be confirmed disabled); the appliance must never route between interfaces",
			Severity: model.SeverityCritical,
		})
	}
	if bridges {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnInterfacesBridged,
			Message:  "A bridge network device is present; the appliance must not bridge interfaces",
			Severity: model.SeverityCritical,
		})
	}
	if !cfg.FirewallActive {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnFirewallInactive,
			Message:  "The default-deny firewall is not active",
			Severity: model.SeverityCritical,
		})
	}
	if cfg.AdminReachableFromPatron && requiresIsolation {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnAdminOnPatronNet,
			Message:  "The admin surface is reachable from the patron network where it must be isolated",
			Severity: model.SeverityCritical,
		})
	}
	if cfg.DefaultCredentials {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnDefaultPassword,
			Message:  "Default credentials are still in place",
			Severity: model.SeverityCritical,
		})
	}
	if cfg.SSHEnabledOutsideSupport {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnSSHEnabled,
			Message:  "SSH is enabled outside of setup/support mode",
			Severity: model.SeverityWarning,
		})
	}

	rep.Warnings = warnings
	rep.Pass = !hasCritical(warnings)
	return rep
}

// forwardingDisabled reports whether the sysctl file at path reads exactly "0"
// (forwarding off). missingMeansDisabled controls the absent-file case: for the
// IPv6 forwarding knob a missing file means IPv6 is disabled at the kernel
// (forwarding impossible → secure), so pass true; for IPv4 ip_forward a missing
// file is suspicious, so pass false to fail loud. Other read errors always fail
// loud.
func forwardingDisabled(path string, missingMeansDisabled bool) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return missingMeansDisabled
		}
		return false
	}
	return strings.TrimSpace(string(b)) == "0"
}

// adminIsolationRequired reports whether admin-on-patron must be treated as a
// failure. Production profiles require isolation; the known non-production
// profiles (Lab, Booth Diagnostic) permit admin-on-patron behind a banner. An
// unknown/empty profile requires isolation conservatively — never fail open.
func adminIsolationRequired(p model.FirewallProfile) bool {
	switch p {
	case model.ProfileLab, model.ProfileBoothDiagnostic:
		return false
	default:
		return true
	}
}

// bridgesPresent reports whether any netdev under sysClassNet is a bridge. A
// bridge netdev is marked by a "bridge" subdirectory (e.g. /sys/class/net/br0/
// bridge). Entries are symlinks; os.Stat follows them to the real bridge path.
func bridgesPresent(sysClassNet string) bool {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(sysClassNet, e.Name(), "bridge"))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// hasCritical reports whether any warning is critical (fails the pass).
func hasCritical(warnings []model.Warning) bool {
	for _, w := range warnings {
		if w.Severity == model.SeverityCritical {
			return true
		}
	}
	return false
}
