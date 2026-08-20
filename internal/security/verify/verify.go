// Package verify performs install-time and runtime SELF-INTROSPECTION
// verification of the appliance's network posture (buildplan.md §9.3).
//
// It asserts that CaptionBridge behaves as an application-layer bridge and never
// an IP-layer device: IPv4 and IPv6 forwarding disabled (globally AND per
// interface), no bridge netdevs, a default-deny nftables ruleset loaded with no
// forward-chain accepts, a theater-side reachable scope no wider than the
// configured allowlist, no DNS/route leak across the two zones, the admin surface
// not reachable from the patron network (in production profiles), no default
// credentials, and SSH state correct for the profile. Any failure becomes a loud
// dashboard warning (§8.3), and for the locked profiles a critical failure is
// grounds to refuse to leave setup (see Report.RefusesSetupCompletion).
//
// CRITICAL DESIGN CONSTRAINT (§9.3): this package verifies the appliance by
// INTROSPECTING ITS OWN state — its own /proc/sys forwarding knobs, its own
// /sys/class/net bridge devices, its own routing table and resolver config, and
// its own loaded nftables table. It NEVER scans the theater subnet or probes
// other hosts; the appliance must never behave like a network scanner on a cinema
// LAN. Every posture claim is a MEASUREMENT: where a fact cannot be measured the
// pass says so loudly rather than accepting an assertion (D17 fail-loud stance).
// All filesystem roots and the ruleset reader are injectable so tests run against
// fixtures.
package verify

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/simpledcp/captionbridge/internal/deploy"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Paths locates the kernel introspection trees. They are injectable so tests can
// point them at fixtures; production uses DefaultPaths.
type Paths struct {
	// ProcSysNet is the root of the networking sysctl tree (default "/proc/sys/net").
	ProcSysNet string
	// SysClassNet is the netdev directory (default "/sys/class/net").
	SysClassNet string
	// ProcNet is the networking state directory holding the routing tables
	// (default "/proc/net").
	ProcNet string
	// ResolvConf is the resolver configuration file (default "/etc/resolv.conf").
	ResolvConf string
}

// DefaultPaths returns the standard Linux introspection roots.
func DefaultPaths() Paths {
	return Paths{
		ProcSysNet:  "/proc/sys/net",
		SysClassNet: "/sys/class/net",
		ProcNet:     "/proc/net",
		ResolvConf:  "/etc/resolv.conf",
	}
}

// withDefaults fills empty roots from DefaultPaths so a partially-specified
// Paths never builds relative paths.
func (p Paths) withDefaults() Paths {
	d := DefaultPaths()
	if p.ProcSysNet == "" {
		p.ProcSysNet = d.ProcSysNet
	}
	if p.SysClassNet == "" {
		p.SysClassNet = d.SysClassNet
	}
	if p.ProcNet == "" {
		p.ProcNet = d.ProcNet
	}
	if p.ResolvConf == "" {
		p.ResolvConf = d.ResolvConf
	}
	return p
}

// Config is the input to a verification pass.
//
// The kernel facts (forwarding, bridges, routes, DNS, ruleset) are MEASURED by
// this package. The remaining facts — admin binding, credentials, SSH — are
// properties of the appliance's own configuration that the kernel does not
// expose, so they are supplied by the caller that owns that configuration.
type Config struct {
	Paths   Paths
	Profile model.FirewallProfile

	// Ruleset introspects the loaded nftables table (§9.3 "default-deny active
	// with no forward-accepts"). When it is set, its OBSERVATION decides
	// Report.FirewallActive and FirewallActive below is ignored — a posture is
	// proven, never asserted. Use NFTRulesetReader() in production.
	//
	// When it is nil the pass falls back to the caller-supplied FirewallActive
	// and marks Report.FirewallIntrospected false: the claim is then unproven and
	// the dashboard must not present it as verified.
	Ruleset RulesetReader

	// FirewallActive is the legacy caller-asserted firewall fact, used only when
	// Ruleset is nil.
	//
	// Deprecated: supply Ruleset instead. "A helper process answered a ping" and
	// "--allow-apply was passed" are not evidence that a default-deny ruleset is
	// loaded.
	FirewallActive bool

	// TheaterIface and ClientIface name the two never-joined zones (§9.1). When
	// both are set, the route and DNS assertions run; when either is empty (a
	// fresh appliance mid-setup) they are skipped and the report claims nothing
	// about them.
	TheaterIface string
	ClientIface  string
	// TheaterAllowlist is the configured cinema server / media-block IP(s). It is
	// what "theater-side reachable scope == allowlist only" is measured against.
	TheaterAllowlist []string

	AdminReachableFromPatron bool
	DefaultCredentials       bool
	SSHEnabledOutsideSupport bool
}

// Report is the outcome of a verification pass. The booleans are positive
// assertions (true = the secure condition holds); Warnings carries the loud
// conditions and Pass is false if any critical warning is present.
type Report struct {
	ForwardingDisabledV4 bool `json:"forwarding_disabled_v4"`
	ForwardingDisabledV6 bool `json:"forwarding_disabled_v6"`
	// ForwardingKnobsEnabled names the specific sysctl knobs that are enabled or
	// unreadable, so an operator is told WHICH interface is forwarding rather than
	// just that something is (guardrail #7: no silent degradation).
	ForwardingKnobsEnabled []string `json:"forwarding_knobs_enabled,omitempty"`

	BridgesPresent bool `json:"bridges_present"`
	// BridgesConfirmed is false when the netdev scan could not be performed. An
	// unreadable /sys/class/net is "unknown", never "no bridges" — this pass does
	// not fail open.
	BridgesConfirmed bool `json:"bridges_confirmed"`

	AdminIsolated  bool `json:"admin_isolated"`
	FirewallActive bool `json:"firewall_active"`
	// FirewallIntrospected is true when FirewallActive came from reading the
	// loaded ruleset rather than from a caller's assertion.
	FirewallIntrospected bool `json:"firewall_introspected"`
	// ForwardAccepts is the number of accept rules observed in the forward chain.
	// §9.1 allows exactly zero.
	ForwardAccepts int `json:"forward_accepts"`

	// RoutesIntrospected is true when the route/DNS assertions ran (both zone
	// interfaces were known).
	RoutesIntrospected bool `json:"routes_introspected"`
	// ZonesSeparated: no route joins the patron side to the theater side.
	ZonesSeparated bool `json:"zones_separated"`
	// TheaterScopeAllowlistOnly: the theater interface carries no route beyond
	// the configured allowlist. False when it could not be asserted.
	TheaterScopeAllowlistOnly bool `json:"theater_scope_allowlist_only"`
	// DNSContained: no configured resolver is reached across a zone boundary.
	DNSContained bool `json:"dns_contained"`

	NoDefaultCreds bool `json:"no_default_credentials"`
	SSHCorrect     bool `json:"ssh_correct"`

	Warnings []model.Warning `json:"warnings"`
	Pass     bool            `json:"pass"`
}

// Run executes a verification pass against the configured paths and facts. It
// only reads the appliance's own state and never touches the network.
func Run(cfg Config) Report {
	p := cfg.Paths.withDefaults()

	fwd := checkForwarding(p.ProcSysNet)
	bridges, bridgesConfirmed := scanBridges(p.SysClassNet)
	fw := observeFirewall(cfg)
	rt := checkRoutes(cfg.TheaterIface, cfg.ClientIface, cfg.TheaterAllowlist, p)

	// In production the admin surface must be off the patron network; in the known
	// non-production profiles (Lab, Booth Diagnostic) admin-on-patron is permitted
	// behind a banner (§8.4). An UNKNOWN/empty profile requires isolation
	// conservatively (never fail open on an unrecognized posture).
	requiresIsolation := adminIsolationRequired(cfg.Profile)
	adminIsolated := !requiresIsolation || !cfg.AdminReachableFromPatron

	sshCorrect, sshSeverity := checkSSH(cfg.Profile, cfg.SSHEnabledOutsideSupport)

	rep := Report{
		ForwardingDisabledV4:      fwd.disabledV4,
		ForwardingDisabledV6:      fwd.disabledV6,
		ForwardingKnobsEnabled:    fwd.enabled,
		BridgesPresent:            bridges,
		BridgesConfirmed:          bridgesConfirmed,
		AdminIsolated:             adminIsolated,
		FirewallActive:            fw.active,
		FirewallIntrospected:      fw.introspected,
		ForwardAccepts:            fw.forwardAccepts,
		RoutesIntrospected:        rt.checked,
		ZonesSeparated:            rt.checked && rt.zonesSeparated,
		TheaterScopeAllowlistOnly: rt.scopeChecked && rt.theaterScopeAllowlistOnly,
		DNSContained:              rt.dnsChecked && rt.dnsContained,
		NoDefaultCreds:            !cfg.DefaultCredentials,
		SSHCorrect:                sshCorrect,
	}

	// Severity of the profile-scoped assertions: a locked (production) profile
	// treats them as blocking; the others still shout, but do not refuse setup.
	profileSeverity := model.SeverityWarning
	if cfg.Profile.Production() || !knownProfile(cfg.Profile) {
		profileSeverity = model.SeverityCritical
	}

	var warnings []model.Warning
	// Forwarding covers BOTH halves of "the appliance must never route": the
	// kernel knobs and the nftables forward chain. One condition, one warning.
	if !fwd.disabledV4 || !fwd.disabledV6 || fw.forwardAccepts > 0 {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnForwardingEnabled,
			Message:  forwardingMessage(fwd, fw.forwardAccepts),
			Severity: model.SeverityCritical,
		})
	}
	if bridges || !bridgesConfirmed {
		msg := "A bridge network device is present; the appliance must not bridge interfaces"
		if !bridgesConfirmed {
			msg = "Bridge devices could not be confirmed absent (" + p.SysClassNet + " is unreadable); the appliance must not bridge interfaces"
		}
		warnings = append(warnings, model.Warning{
			Code: model.WarnInterfacesBridged, Message: msg, Severity: model.SeverityCritical,
		})
	}
	if !fw.active {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnFirewallInactive,
			Message:  fw.message(),
			Severity: model.SeverityCritical,
		})
	}
	// NOTE: when no Ruleset reader is supplied the firewall claim is the caller's
	// assertion and nothing read the kernel. That is reported as
	// Report.FirewallIntrospected=false rather than as a warning, because the
	// existing §8.3 codes have no term for "asserted but unverified" and reusing
	// WarnFirewallInactive would tell an operator the firewall is DOWN when it may
	// well be up. Surfacing it properly wants a new code in internal/model.
	if rt.checked && !rt.zonesSeparated {
		msg := "The patron and theater zones are not separated: " + rt.sharedReason
		if rt.unreadable != "" {
			msg = "Zone separation could not be confirmed: " + rt.unreadable
		}
		warnings = append(warnings, model.Warning{
			// The two zones are never joined in ANY profile (§9.1), so this is
			// critical regardless of the deployment posture.
			Code: model.WarnSharedInterface, Message: msg, Severity: model.SeverityCritical,
		})
	}
	if rt.scopeChecked && !rt.theaterScopeAllowlistOnly && scopeAssertionApplies(cfg.Profile) {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnDNSLeak,
			Message:  "The theater-side reachable scope is wider than the allowlist: " + rt.scopeReason,
			Severity: profileSeverity,
		})
	}
	if rt.dnsChecked && !rt.dnsContained {
		msg := "DNS crosses a zone boundary: " + rt.dnsReason
		if rt.unreadable != "" {
			msg = "DNS containment could not be confirmed: " + rt.unreadable
		}
		warnings = append(warnings, model.Warning{
			Code: model.WarnDNSLeak, Message: msg, Severity: profileSeverity,
		})
	} else if rt.dnsViaDefaultRoute {
		// An internet-facing resolver is loud but not blocking: a booth admin
		// network legitimately has one, and refusing to leave setup over it would
		// be a false alarm on a real install (§9.3 wants the warning, §9.4 wants
		// no internet dependence).
		warnings = append(warnings, model.Warning{
			Code:     model.WarnDNSLeak,
			Message:  "A configured nameserver is reachable only through a default route (an internet-facing resolver); the appliance is meant to be venue-local",
			Severity: model.SeverityWarning,
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
	if !sshCorrect {
		warnings = append(warnings, model.Warning{
			Code:     model.WarnSSHEnabled,
			Message:  sshMessage(cfg.Profile),
			Severity: sshSeverity,
		})
	}

	rep.Warnings = warnings
	rep.Pass = !hasCritical(warnings)
	return rep
}

// RefusesSetupCompletion reports whether this report must BLOCK leaving setup
// (§9.3: "Any failure flips a loud dashboard warning and, for the locked
// profiles, can refuse to leave setup"). Only the locked (production) profiles
// block, and only on a critical failure — a warning-severity finding is loud on
// the dashboard but does not strand an installer mid-wizard.
//
// An unknown profile blocks conservatively: an unrecognized posture is not a
// licence to skip the gate.
func (r Report) RefusesSetupCompletion(p model.FirewallProfile) bool {
	if r.Pass {
		return false
	}
	return p.Production() || !knownProfile(p)
}

// BlockingCodes lists the warning codes that make this report fail, so a refusal
// can name the offending conditions instead of a bare "not allowed".
func (r Report) BlockingCodes() []string {
	var out []string
	seen := make(map[string]bool)
	for _, w := range r.Warnings {
		if w.Severity != model.SeverityCritical || seen[w.Code] {
			continue
		}
		seen[w.Code] = true
		out = append(out, w.Code)
	}
	return out
}

// forwardingCheck is the outcome of the forwarding sweep.
type forwardingCheck struct {
	disabledV4 bool
	disabledV6 bool
	// enabled names the knobs that read non-zero or could not be read, shortened
	// to the sysctl-style suffix (e.g. "ipv6/conf/eth0/forwarding").
	enabled []string
}

// ForwardingKnobs enumerates every forwarding sysctl under procSysNet that must
// read "0" for the appliance to be a non-router (§9.1, §9.3), split by family.
//
// The global knobs are NOT the whole story: forwarding is also a per-device
// property, so `net.ipv6.conf.eth0.forwarding=1` forwards on that device while
// `conf/all/forwarding` still reads 0, and IPv4 has the same per-device knob. An
// interface created after boot inherits `conf/default/forwarding`. Enumerating at
// runtime is the only way to cover interfaces that did not exist at build time;
// the privileged helper uses the same list when re-asserting the posture.
func ForwardingKnobs(procSysNet string) (v4, v6 []string) {
	if procSysNet == "" {
		procSysNet = DefaultPaths().ProcSysNet
	}
	v4 = append(v4, filepath.Join(procSysNet, "ipv4", "ip_forward"))
	v4 = append(v4, confForwardingKnobs(filepath.Join(procSysNet, "ipv4", "conf"))...)
	v6 = append(v6, filepath.Join(procSysNet, "ipv6", "conf", "all", "forwarding"))
	v6 = append(v6, confForwardingKnobs(filepath.Join(procSysNet, "ipv6", "conf"))...)
	return dedupe(v4), dedupe(v6)
}

// confForwardingKnobs lists <conf>/<iface>/forwarding for every entry present,
// including "all" and "default", in a deterministic order.
func confForwardingKnobs(confDir string) []string {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return nil // no per-device knobs to check (family off, or no such tree)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(confDir, e.Name(), "forwarding"))
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// checkForwarding asserts EVERY forwarding knob of both families reads "0".
//
// The canonical global knobs keep their D17 missing-file semantics: a missing
// IPv4 ip_forward (which essentially always exists) is suspicious and fails loud,
// while a missing IPv6 forwarding knob means IPv6 is disabled at the kernel —
// forwarding is impossible, the SECURE state. Per-device knobs are enumerated
// from the live tree, so a missing one simply no longer exists.
func checkForwarding(procSysNet string) forwardingCheck {
	v4Paths, v6Paths := ForwardingKnobs(procSysNet)
	canonicalV4 := filepath.Join(procSysNet, "ipv4", "ip_forward")

	c := forwardingCheck{disabledV4: true, disabledV6: true}
	for _, path := range v4Paths {
		if !forwardingDisabled(path, path != canonicalV4) {
			c.disabledV4 = false
			c.enabled = append(c.enabled, knobLabel(procSysNet, path))
		}
	}
	for _, path := range v6Paths {
		if !forwardingDisabled(path, true) {
			c.disabledV6 = false
			c.enabled = append(c.enabled, knobLabel(procSysNet, path))
		}
	}
	return c
}

// knobLabel shortens a knob path to its sysctl-style tail for warning messages.
func knobLabel(procSysNet, path string) string {
	if rel, err := filepath.Rel(procSysNet, path); err == nil {
		return rel
	}
	return path
}

// forwardingMessage composes the single forwarding warning from both halves: the
// kernel knobs and the nftables forward chain.
func forwardingMessage(c forwardingCheck, forwardAccepts int) string {
	var parts []string
	if len(c.enabled) > 0 {
		shown := c.enabled
		if len(shown) > 4 {
			shown = shown[:4]
		}
		parts = append(parts, "forwarding is enabled (or cannot be confirmed disabled) at "+strings.Join(shown, ", "))
		if len(c.enabled) > len(shown) {
			parts[len(parts)-1] += " and others"
		}
	} else if !c.disabledV4 || !c.disabledV6 {
		parts = append(parts, "forwarding cannot be confirmed disabled")
	}
	if forwardAccepts > 0 {
		parts = append(parts, "the nftables forward chain carries accept rules")
	}
	return strings.ToUpper(parts[0][:1]) + parts[0][1:] + joinRest(parts) +
		"; the appliance must never route between interfaces"
}

func joinRest(parts []string) string {
	if len(parts) < 2 {
		return ""
	}
	return " and " + strings.Join(parts[1:], " and ")
}

// firewallObservation records how the firewall fact was obtained, so the report
// can distinguish "proven default-deny" from "somebody said so".
type firewallObservation struct {
	active         bool
	introspected   bool // the posture was OBSERVED
	attempted      bool // a reader was supplied
	forwardAccepts int
	reason         string
}

// observeFirewall introspects the loaded ruleset when a reader is configured and
// otherwise falls back to the caller's (unproven) assertion.
func observeFirewall(cfg Config) firewallObservation {
	if cfg.Ruleset == nil {
		return firewallObservation{active: cfg.FirewallActive}
	}
	st, err := cfg.Ruleset()
	if err != nil {
		// Cannot confirm → fail loud. Reading the ruleset is not gated by B5
		// (that gates APPLYING), so an unreadable ruleset is a real finding.
		return firewallObservation{attempted: true, reason: err.Error()}
	}
	obs := firewallObservation{
		attempted:      true,
		introspected:   true,
		forwardAccepts: st.ForwardAccepts(),
		active:         st.DefaultDeny() && st.ForwardAccepts() == 0,
	}
	switch {
	case !st.TablePresent:
		obs.reason = "the " + TableFamily + " " + TableName + " table is not loaded"
	case !obs.active:
		obs.reason = "the loaded ruleset is not default-deny on every chain"
	}
	return obs
}

// message renders the firewall warning text for the way the fact was obtained.
func (o firewallObservation) message() string {
	switch {
	case o.attempted && !o.introspected:
		return "The default-deny firewall could not be confirmed by introspection: " + o.reason
	case o.introspected:
		return "The default-deny firewall is not active: " + o.reason
	default:
		return "The default-deny firewall is not active"
	}
}

// forwardingDisabled reports whether the sysctl file at path reads exactly "0"
// (forwarding off). missingMeansDisabled controls the absent-file case: for the
// IPv6 forwarding knobs and the per-device knobs a missing file means the knob
// does not exist (family disabled, or the device is gone) → secure; for IPv4
// ip_forward a missing file is suspicious, so pass false to fail loud. Other read
// errors always fail loud.
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

// knownProfile reports whether the profile is one of the four shipping postures.
func knownProfile(p model.FirewallProfile) bool {
	_, ok := deploy.Get(p)
	return ok
}

// scopeAssertionApplies reports whether "theater-side reachable scope ==
// allowlist only" is asserted for this profile. §9.2 grants Lab "broader
// discovery allowed", so a Lab bench is not nagged about it; every other profile
// (including an unknown one) is allowlist-only.
func scopeAssertionApplies(p model.FirewallProfile) bool {
	if prof, ok := deploy.Get(p); ok {
		return prof.TheaterAllowlistOnly
	}
	return true
}

// checkSSH routes the SSH assertion through the profile's policy (§9.3 "SSH state
// correct for the profile", §9.2 table). It consumes deploy.Profile.AllowLocalSSH
// rather than treating "SSH is on" as universally wrong:
//
//   - Lab permits local SSH, so SSH-on is CORRECT there and must not raise a
//     false alarm on a bench;
//   - Booth Diagnostic has SSH "disabled by default" — a deviation is loud but
//     not blocking;
//   - Patron-Facing and Low-Cost Replacement require it disabled, and §9.3 lets a
//     locked profile refuse to leave setup, so the finding is CRITICAL there.
//
// An unknown profile is treated as locked (never fail open).
func checkSSH(p model.FirewallProfile, enabled bool) (correct bool, severity string) {
	if !enabled {
		return true, ""
	}
	if prof, ok := deploy.Get(p); ok {
		if prof.AllowLocalSSH {
			return true, "" // §9.2: Lab — "local SSH ok"
		}
		if !p.Production() {
			return false, model.SeverityWarning // §9.2: "disabled by default"
		}
	}
	return false, model.SeverityCritical
}

// sshMessage names the profile's expectation so the operator knows why it is a
// finding here and not on the bench.
func sshMessage(p model.FirewallProfile) string {
	if p.Production() {
		return "SSH is enabled; this profile requires SSH disabled — support mode is the only re-entry path"
	}
	return "SSH is enabled; this profile disables SSH by default"
}

// scanBridges reports whether any netdev under sysClassNet is a bridge, and
// whether the scan could be performed at all. A bridge netdev is marked by a
// "bridge" subdirectory (e.g. /sys/class/net/br0/bridge). Entries are symlinks;
// os.Stat follows them to the real bridge path.
//
// An unreadable or missing /sys/class/net returns confirmed=false — "unknown",
// which the caller turns into a loud warning. Reporting "no bridges present"
// because we could not look would fail open on guardrail #2, and contradict the
// fail-loud stance this package takes for a missing ip_forward (D17).
func scanBridges(sysClassNet string) (present, confirmed bool) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(sysClassNet, e.Name(), "bridge"))
		if err == nil && info.IsDir() {
			return true, true
		}
	}
	return false, true
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
