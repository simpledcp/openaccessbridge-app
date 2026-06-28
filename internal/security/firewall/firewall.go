// Package firewall generates the nftables ruleset for CaptionBridge's deployment
// profiles (buildplan.md §9.1). It only GENERATES rule text — it never touches the
// kernel, needs no root, and applies nothing; a separate privileged helper (§3.5)
// is responsible for installing what this package emits.
//
// The ruleset is default-deny on every chain. The forward chain carries no accept
// rules at all (the two zones are never joined), kernel forwarding is disabled for
// IPv4 and IPv6 (see SysctlSettings), and the single theater-side rule depends on
// which side opens the ACSP TCP connection (Variant A vs B, §9.1). Nothing here
// ever logs caption text or audio.
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
	Profile         model.FirewallProfile
	TheaterIface    string
	ClientIface     string
	CinemaServerIP  string
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
}

// SysctlSettings returns the kernel knobs that must be set alongside the ruleset to
// disable packet forwarding for IPv4 and IPv6 (§9.1: "Kernel forwarding disabled
// for IPv4 and IPv6"). Both must read "0" for the appliance to be a non-router.
func SysctlSettings() map[string]string {
	return map[string]string{
		"net.ipv4.ip_forward":          "0",
		"net.ipv6.conf.all.forwarding": "0",
	}
}

// Generate returns a complete nft script for "table inet captionbridge" implementing
// the profile in cfg per §9.1. It validates cfg and returns an error rather than
// emitting a rule that would be useless or a hole (e.g. the wrong ACSP variant).
func Generate(cfg Config) (string, error) {
	serverIP, ipKw, err := validate(cfg)
	if err != nil {
		return "", err
	}

	var b strings.Builder
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
		// match on destination port (our listener), not source port (§9.1).
		fmt.Fprintf(&b, "    iif %q %s saddr %s tcp dport %d accept    # ACSP (server-initiated, Variant B)\n",
			cfg.TheaterIface, ipKw, serverIP, cfg.LocalACSPPort)
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
		// Variant A: allow the outbound connection to the server's ACSP port.
		fmt.Fprintf(&b, "    oif %q %s daddr %s tcp dport %d accept    # ACSP (acs-initiated, Variant A)\n",
			cfg.TheaterIface, ipKw, serverIP, cfg.ACSPPort)
	}
	b.WriteString("  }\n")

	b.WriteString("}\n")
	return b.String(), nil
}

// validate checks cfg and returns the parsed server IP plus the nft address-family
// keyword ("ip" for v4, "ip6" for v6) used in the theater-side rule.
func validate(cfg Config) (net.IP, string, error) {
	if strings.TrimSpace(cfg.TheaterIface) == "" {
		return nil, "", errors.New("firewall: TheaterIface is required")
	}
	if strings.TrimSpace(cfg.ClientIface) == "" {
		return nil, "", errors.New("firewall: ClientIface is required")
	}
	if cfg.TheaterIface == cfg.ClientIface {
		return nil, "", fmt.Errorf("firewall: theater and client interfaces must differ (both %q); the zones are never joined", cfg.TheaterIface)
	}
	ip := net.ParseIP(strings.TrimSpace(cfg.CinemaServerIP))
	if ip == nil {
		return nil, "", fmt.Errorf("firewall: CinemaServerIP %q is not a valid IP address", cfg.CinemaServerIP)
	}
	ipKw := "ip"
	if ip.To4() == nil {
		ipKw = "ip6"
	}
	switch cfg.Initiation {
	case model.ACSInitiated:
		if !validPort(cfg.ACSPPort) {
			return nil, "", fmt.Errorf("firewall: Variant A (acs-initiated) requires a valid ACSPPort, got %d", cfg.ACSPPort)
		}
	case model.ServerInitiated:
		if !validPort(cfg.LocalACSPPort) {
			return nil, "", fmt.Errorf("firewall: Variant B (server-initiated) requires a valid LocalACSPPort, got %d", cfg.LocalACSPPort)
		}
	default:
		return nil, "", fmt.Errorf("firewall: unknown ACSP initiation %q (must be acs_initiated or server_initiated)", cfg.Initiation)
	}
	for _, p := range cfg.PatronWebPorts {
		if !validPort(p) {
			return nil, "", fmt.Errorf("firewall: invalid patron web port %d", p)
		}
	}
	if cfg.WebRTCPortRange != "" {
		if err := validateRange(cfg.WebRTCPortRange); err != nil {
			return nil, "", err
		}
	}
	if cfg.AdminOnClientIface && adminAllowed(cfg) && !validPort(adminPort(cfg)) {
		return nil, "", fmt.Errorf("firewall: invalid admin port %d", adminPort(cfg))
	}
	return ip, ipKw, nil
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
