// Package firewall generates the nftables ruleset for CaptionBridge's deployment
// profiles (buildplan.md §9.1). It only GENERATES rule text — it never touches the
// kernel, needs no root, and applies nothing; a separate privileged helper (§3.5)
// is responsible for installing what this package emits.
//
// The ruleset is default-deny on every chain. The forward chain carries no accept
// rules at all (the two zones are never joined), kernel forwarding is disabled for
// IPv4 and IPv6 (see SysctlSettings), and the theater-side rule is limited to the
// configured allowlist and depends on which side opens the ACSP TCP connection
// (Variant A vs B, §9.1). Nothing here ever logs caption text or audio.
package firewall

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/simpledcp/captionbridge/internal/model"
)

// DefaultAdminPort is the admin/setup UI port opened on the client interface when
// a non-production profile permits admin on the patron network (§9.2). It is used
// only when Config.AdminPort is left zero.
const DefaultAdminPort = 8443

// Config is the input to Generate. The two interfaces name the never-joined zones:
// TheaterIface faces the cinema server / media block, ClientIface faces patron
// display devices (and, on non-production profiles, the setup/admin UI). The
// theater-side rule is selected by Initiation (§9.1 Variant A/B).
type Config struct {
	Profile      model.FirewallProfile
	TheaterIface string
	ClientIface  string
	// CinemaServerIP is the single-address form of the theater allowlist, kept as a
	// compatibility alias for configuration written before the allowlist became
	// plural. Prefer CinemaServerIPs; when both are set the two are unioned.
	CinemaServerIP string
	// CinemaServerIPs is the theater-side allowlist (§9.1 "the configured cinema
	// server/media-block IP(s)", §9.2 "allowlisted server/IMB IPs only"). A booth
	// where the ACSP endpoint and the media block (or a redundant/secondary server)
	// are distinct hosts needs more than one entry. Families may be mixed; Generate
	// emits one rule per family because the nft match keyword differs (ip vs ip6).
	CinemaServerIPs []string
	ACSPPort        int                  // Variant A: remote server ACSP port (dport on output)
	LocalACSPPort   int                  // Variant B: CaptionBridge's local ACSP listener port
	Initiation      model.ACSPInitiation // ACSInitiated (A) or ServerInitiated (B)
	WebRTCPortRange string               // e.g. "10000-20000"; omitted if empty
	PatronWebPorts  []int                // e.g. {80, 443}; omitted if empty
	// AdminOnClientIface requests an admin/setup rule on the client interface. It is
	// honored ONLY when the profile permits it (Production profiles never do, §8.4).
	AdminOnClientIface bool
	// AdminPort is the admin/setup UI port; defaults to DefaultAdminPort when zero.
	AdminPort int
	// SupportEgress is the §9.4 support window, present only while a support
	// session is open. Nil means no window, which is the default and the state
	// the appliance spends its life in.
	SupportEgress *SupportEgress
}

// SupportEgress is the ONE outbound destination a support session may reach
// (§9.4 "outbound-only, time-limited, and audited").
//
// Every field is required and there is no default, deliberately. A support
// window with a missing address would have to be rendered either as a bare
// `oif <iface> accept` — an unrestricted egress hole opened by a support switch —
// or as nothing at all, silently. Generate refuses instead.
type SupportEgress struct {
	// Iface is the interface the support endpoint is reachable on. It is named
	// rather than assumed: on this appliance the theater side and the patron side
	// are both possible, and guessing would put support traffic on the patron
	// LAN.
	Iface string
	// Host is a single literal IP. Not a hostname: resolving one would need DNS
	// from the appliance, which §9.1's DNS-leak checks exist to prevent, and a
	// name that resolves differently later is a rule that quietly changes what it
	// permits.
	Host string
	// Port is the single destination port.
	Port int
}

// SysctlSettings returns the kernel knobs that must be set alongside the ruleset to
// disable packet forwarding for IPv4 and IPv6 (§9.1: "Kernel forwarding disabled
// for IPv4 and IPv6"). Every one must read "0" for the appliance to be a non-router.
//
// The GLOBAL knobs alone are NOT sufficient: forwarding is also a per-device
// property (net.ipv{4,6}.conf.<iface>.forwarding — an IPv6 per-interface enable
// forwards on that device while conf.all.forwarding still reads 0), and an
// interface that appears after boot inherits conf.default.forwarding. The
// `default` knobs are written here to cover interfaces that do not exist yet; the
// per-interface knobs cannot be enumerated statically, so verify.ForwardingKnobs
// enumerates them at runtime and the verification pass asserts every one (§9.3).
func SysctlSettings() map[string]string {
	return map[string]string{
		"net.ipv4.ip_forward":              "0",
		"net.ipv4.conf.all.forwarding":     "0",
		"net.ipv4.conf.default.forwarding": "0",
		"net.ipv6.conf.all.forwarding":     "0",
		"net.ipv6.conf.default.forwarding": "0",
	}
}

// Generate returns a complete nft script for "table inet captionbridge" implementing
// the profile in cfg per §9.1. It validates cfg and returns an error rather than
// emitting a rule that would be useless or a hole (e.g. the wrong ACSP variant).
func Generate(cfg Config) (string, error) {
	allow, err := validate(cfg)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// REPLACE the table, never add to it.
	//
	// `nft -f` on a table definition is ADDITIVE: `add rule` semantics are
	// implicit, so feeding this script at a table that already exists appends
	// every rule again and leaves whatever was there before. That is not a
	// cosmetic problem. Re-applying after a configuration change would keep the
	// OLD rules alive beside the new ones — the previous cinema server's allow
	// rule after the address is corrected, or the admin-on-patron-interface rule
	// after a switch from Booth Diagnostic to a production profile. The stricter
	// posture would be applied and the looser one would go on accepting packets,
	// which is the precise shape of a hole nobody would see: the dashboard reads
	// the table, finds the expected rules present, and reports the posture
	// verified.
	//
	// The two lines below are the standard idiom. The bare `table` line creates
	// it if it is absent, so the `delete` cannot fail on a fresh boot, and both
	// land inside the same `nft -f` transaction as the definition that follows —
	// so the appliance is never momentarily unprotected between them.
	b.WriteString("table inet captionbridge\n")
	b.WriteString("delete table inet captionbridge\n")
	b.WriteString("table inet captionbridge {\n")

	// --- input chain -------------------------------------------------------
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iif \"lo\" accept\n")
	if len(cfg.PatronWebPorts) > 0 {
		fmt.Fprintf(&b, "    iif %q tcp dport %s accept    # patron web UI\n",
			cfg.ClientIface, portSet(cfg.PatronWebPorts))
	}
	if cfg.WebRTCPortRange != "" {
		fmt.Fprintf(&b, "    iif %q udp dport %s accept    # WebRTC media\n",
			cfg.ClientIface, cfg.WebRTCPortRange)
	}
	if adminAllowed(cfg) {
		fmt.Fprintf(&b, "    iif %q tcp dport %d accept    # admin/setup UI (non-production profile)\n",
			cfg.ClientIface, adminPort(cfg))
	}
	if cfg.Initiation == model.ServerInitiated {
		// Variant B: the media block opens the connection to our local listener;
		// match on destination port (our listener), not source port (§9.1). One rule
		// per address family — a single rule cannot mix ip and ip6 matches.
		for _, fam := range allow.families() {
			fmt.Fprintf(&b, "    iif %q %s saddr %s tcp dport %d accept    # ACSP (server-initiated, Variant B)\n",
				cfg.TheaterIface, fam.kw, fam.literal(), cfg.LocalACSPPort)
		}
	}
	// Variant A needs no explicit inbound rule: replies are admitted by the
	// ct state established,related rule above. We deliberately emit none.
	b.WriteString("    # everything else dropped\n")
	b.WriteString("  }\n")

	// --- forward chain -----------------------------------------------------
	// NEVER accept: the theater and patron zones are never joined (§9.1).
	b.WriteString("  chain forward {\n")
	b.WriteString("    type filter hook forward priority 0; policy drop;  # NEVER accept — zones are never joined\n")
	b.WriteString("  }\n")

	// --- output chain ------------------------------------------------------
	b.WriteString("  chain output {\n")
	b.WriteString("    type filter hook output priority 0; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    oif \"lo\" accept\n")
	if cfg.Initiation == model.ACSInitiated {
		// Variant A: allow the outbound connection to the server's ACSP port. One
		// rule per address family — a single rule cannot mix ip and ip6 matches.
		for _, fam := range allow.families() {
			fmt.Fprintf(&b, "    oif %q %s daddr %s tcp dport %d accept    # ACSP (acs-initiated, Variant A)\n",
				cfg.TheaterIface, fam.kw, fam.literal(), cfg.ACSPPort)
		}
	}
	if sup := cfg.SupportEgress; sup != nil {
		// §9.4's support window. OUTPUT only, one address, one port, and no
		// matching input rule anywhere — the return traffic is admitted by the
		// established/related rule at the top of the input chain, exactly as
		// Variant A's replies are. An inbound accept here would turn "outbound
		// only" into a listening port on an appliance whose SSH is disabled.
		fmt.Fprintf(&b, "    oif %q %s daddr %s tcp dport %d accept    # support session (§9.4, time-limited)\n",
			sup.Iface, ipKeyword(sup.Host), sup.Host, sup.Port)
	}
	b.WriteString("  }\n")

	b.WriteString("}\n")
	return b.String(), nil
}

// Allowlist returns the configured theater-side allowlist entries as written: the
// union of the deprecated scalar CinemaServerIP and CinemaServerIPs, de-duplicated
// with order preserved. It does not validate the entries; Generate does.
func (cfg Config) Allowlist() []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(cfg.CinemaServerIP)
	for _, s := range cfg.CinemaServerIPs {
		add(s)
	}
	return out
}

// allowlist is the validated theater allowlist split by address family, because
// the nft match keyword differs ("ip" vs "ip6") and one rule cannot carry both.
type allowlist struct {
	v4 []net.IP
	v6 []net.IP
}

// family is one address family's share of the allowlist plus its nft keyword.
type family struct {
	kw    string
	addrs []net.IP
}

// families returns the non-empty families in a stable order (v4 then v6) so the
// generated ruleset is deterministic.
func (a allowlist) families() []family {
	var out []family
	if len(a.v4) > 0 {
		out = append(out, family{kw: "ip", addrs: a.v4})
	}
	if len(a.v6) > 0 {
		out = append(out, family{kw: "ip6", addrs: a.v6})
	}
	return out
}

// literal renders a family's addresses as an nft match operand: a bare address for
// a single entry, an anonymous set "{ a, b }" for several.
func (f family) literal() string {
	if len(f.addrs) == 1 {
		return f.addrs[0].String()
	}
	parts := make([]string, len(f.addrs))
	for i, ip := range f.addrs {
		parts[i] = ip.String()
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// validate checks cfg and returns the parsed theater allowlist, split by address
// family and ready for rule emission.
func validate(cfg Config) (allowlist, error) {
	var none allowlist
	if strings.TrimSpace(cfg.TheaterIface) == "" {
		return none, errors.New("firewall: TheaterIface is required")
	}
	if strings.TrimSpace(cfg.ClientIface) == "" {
		return none, errors.New("firewall: ClientIface is required")
	}
	if cfg.TheaterIface == cfg.ClientIface {
		return none, fmt.Errorf("firewall: theater and client interfaces must differ (both %q); the zones are never joined", cfg.TheaterIface)
	}
	// Interface names are emitted into the ROOT-applied nftables ruleset, so they
	// are validated against the kernel IFNAMSIZ limit and a safe charset before
	// they reach a privileged command (defense-in-depth).
	if err := validIface(cfg.TheaterIface); err != nil {
		return none, fmt.Errorf("firewall: TheaterIface %w", err)
	}
	if err := validIface(cfg.ClientIface); err != nil {
		return none, fmt.Errorf("firewall: ClientIface %w", err)
	}
	// §9.4's support window is validated to the same standard as everything else
	// that reaches the root-applied ruleset. A partially configured window is
	// REFUSED rather than rendered: the only two ways to render one are a bare
	// interface accept, which is an unrestricted egress hole opened by a support
	// switch, and silence, which would report a window as open while permitting
	// nothing.
	if sup := cfg.SupportEgress; sup != nil {
		if err := validIface(sup.Iface); err != nil {
			return none, fmt.Errorf("firewall: support egress interface %w", err)
		}
		if net.ParseIP(sup.Host) == nil {
			return none, fmt.Errorf("firewall: support endpoint %q is not a literal IP address", sup.Host)
		}
		if sup.Port < 1 || sup.Port > 65535 {
			return none, fmt.Errorf("firewall: support endpoint port %d is out of range", sup.Port)
		}
	}
	entries := cfg.Allowlist()
	if len(entries) == 0 {
		return none, errors.New("firewall: the theater allowlist is empty; at least one cinema server / media-block IP is required")
	}
	var allow allowlist
	for _, e := range entries {
		ip := net.ParseIP(e)
		if ip == nil {
			return none, fmt.Errorf("firewall: cinema server allowlist entry %q is not a valid IP address", e)
		}
		if ip.To4() != nil {
			allow.v4 = append(allow.v4, ip)
		} else {
			allow.v6 = append(allow.v6, ip)
		}
	}
	switch cfg.Initiation {
	case model.ACSInitiated:
		if !validPort(cfg.ACSPPort) {
			return none, fmt.Errorf("firewall: Variant A (acs-initiated) requires a valid ACSPPort, got %d", cfg.ACSPPort)
		}
	case model.ServerInitiated:
		if !validPort(cfg.LocalACSPPort) {
			return none, fmt.Errorf("firewall: Variant B (server-initiated) requires a valid LocalACSPPort, got %d", cfg.LocalACSPPort)
		}
	default:
		return none, fmt.Errorf("firewall: unknown ACSP initiation %q (must be acs_initiated or server_initiated)", cfg.Initiation)
	}
	for _, p := range cfg.PatronWebPorts {
		if !validPort(p) {
			return none, fmt.Errorf("firewall: invalid patron web port %d", p)
		}
	}
	if cfg.WebRTCPortRange != "" {
		if err := validateRange(cfg.WebRTCPortRange); err != nil {
			return none, err
		}
	}
	if cfg.AdminOnClientIface && adminAllowed(cfg) && !validPort(adminPort(cfg)) {
		return none, fmt.Errorf("firewall: invalid admin port %d", adminPort(cfg))
	}
	return allow, nil
}

// adminAllowed reports whether an admin rule should be emitted on the client iface:
// only when requested AND permitted by the profile. Production profiles never
// expose admin on the patron network (§8.4), regardless of the request.
func adminAllowed(cfg Config) bool {
	return cfg.AdminOnClientIface && cfg.Profile.AdminAllowedOnPatronNet()
}

func adminPort(cfg Config) int {
	if cfg.AdminPort == 0 {
		return DefaultAdminPort
	}
	return cfg.AdminPort
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// validIface validates a Linux interface name: non-empty, at most 15 chars
// (IFNAMSIZ-1), and limited to [A-Za-z0-9._-] (no whitespace, slash, quote, or
// control characters that could corrupt or escape the generated ruleset).
func validIface(name string) error {
	if len(name) == 0 || len(name) > 15 {
		return fmt.Errorf("%q must be 1–15 characters", name)
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("%q contains an invalid character", name)
		}
	}
	return nil
}

// ipKeyword is the nft match keyword for an address's family. One rule cannot
// carry both "ip" and "ip6" matches.
func ipKeyword(host string) string {
	if strings.Contains(host, ":") {
		return "ip6"
	}
	return "ip"
}

// validateRange checks an nft port-range literal like "10000-20000".
func validateRange(r string) error {
	lo, hi, ok := strings.Cut(r, "-")
	if !ok {
		return fmt.Errorf("firewall: WebRTCPortRange %q must be of the form \"low-high\"", r)
	}
	loN, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil || !validPort(loN) {
		return fmt.Errorf("firewall: WebRTCPortRange %q has an invalid low port", r)
	}
	hiN, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil || !validPort(hiN) {
		return fmt.Errorf("firewall: WebRTCPortRange %q has an invalid high port", r)
	}
	if loN >= hiN {
		return fmt.Errorf("firewall: WebRTCPortRange %q low port must be less than high port", r)
	}
	return nil
}

// portSet renders ports as an nft anonymous set, e.g. "{ 80, 443 }".
func portSet(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}
