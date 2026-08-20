package firewall

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// baseVariantA is a valid acs-initiated config (Variant A) used as a starting
// point for most tests.
func baseVariantA() Config {
	return Config{
		Profile:         model.ProfilePatronFacing,
		TheaterIface:    "eth0",
		ClientIface:     "wlan0",
		CinemaServerIP:  "192.0.2.10",
		ACSPPort:        4170,
		Initiation:      model.ACSInitiated,
		WebRTCPortRange: "10000-20000",
		PatronWebPorts:  []int{80, 443},
	}
}

// baseVariantB is a valid server-initiated config (Variant B).
func baseVariantB() Config {
	c := baseVariantA()
	c.Initiation = model.ServerInitiated
	c.ACSPPort = 0
	c.LocalACSPPort = 4171
	return c
}

// chainBody returns the lines inside the named chain (between "chain <name> {" and
// its closing "}"), trimmed.
func chainBody(t *testing.T, script, name string) []string {
	t.Helper()
	lines := strings.Split(script, "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, "chain "+name+" {") {
			start = i + 1
			break
		}
	}
	if start == -1 {
		t.Fatalf("chain %q not found in script:\n%s", name, script)
	}
	var body []string
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "}" {
			return body
		}
		body = append(body, strings.TrimSpace(lines[i]))
	}
	t.Fatalf("chain %q not terminated", name)
	return nil
}

func countAccept(lines []string) int {
	n := 0
	for _, ln := range lines {
		// Count only rule lines that actually accept (ignore comments).
		code := ln
		if i := strings.Index(code, "#"); i >= 0 {
			code = code[:i]
		}
		if strings.Contains(code, "accept") {
			n++
		}
	}
	return n
}

func TestVariantA_OutputRuleAndNoInboundSport(t *testing.T) {
	script, err := Generate(baseVariantA())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out := chainBody(t, script, "output")
	wantOut := `oif "eth0" ip daddr 192.0.2.10 tcp dport 4170 accept`
	if !containsRule(out, wantOut) {
		t.Fatalf("Variant A output chain missing daddr rule %q; got:\n%s", wantOut, strings.Join(out, "\n"))
	}

	// No explicit inbound sport rule — conntrack admits the replies.
	if strings.Contains(script, "sport") {
		t.Fatalf("Variant A must not emit an inbound sport rule; script:\n%s", script)
	}
	in := chainBody(t, script, "input")
	for _, ln := range in {
		if strings.Contains(ln, "saddr") {
			t.Fatalf("Variant A must not emit an inbound saddr/theater rule, got %q", ln)
		}
	}
}

func TestVariantB_InputRule(t *testing.T) {
	script, err := Generate(baseVariantB())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	in := chainBody(t, script, "input")
	want := `iif "eth0" ip saddr 192.0.2.10 tcp dport 4171 accept`
	if !containsRule(in, want) {
		t.Fatalf("Variant B input chain missing saddr rule %q; got:\n%s", want, strings.Join(in, "\n"))
	}
	// Variant B needs no extra output rule beyond lo/established.
	out := chainBody(t, script, "output")
	for _, ln := range out {
		if strings.Contains(ln, "daddr") {
			t.Fatalf("Variant B must not emit an extra output daddr rule, got %q", ln)
		}
	}
}

func TestForwardChain_PolicyDropZeroAccept(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"variantA", baseVariantA()},
		{"variantB", baseVariantB()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, err := Generate(tc.cfg)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			fwd := chainBody(t, script, "forward")
			joined := strings.Join(fwd, "\n")
			if !strings.Contains(joined, "policy drop") {
				t.Fatalf("forward chain missing policy drop; got:\n%s", joined)
			}
			if !strings.Contains(joined, "NEVER accept") {
				t.Fatalf("forward chain missing NEVER accept comment; got:\n%s", joined)
			}
			if n := countAccept(fwd); n != 0 {
				t.Fatalf("forward chain must have ZERO accept rules, found %d in:\n%s", n, joined)
			}
		})
	}
}

func TestAllChainsDefaultDeny(t *testing.T) {
	script, err := Generate(baseVariantA())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, ch := range []string{"input", "forward", "output"} {
		body := strings.Join(chainBody(t, script, ch), "\n")
		if !strings.Contains(body, "policy drop") {
			t.Fatalf("chain %q is not default-deny:\n%s", ch, body)
		}
	}
	// Hook priorities present (sanity that the chains are real base chains).
	for _, hook := range []string{"hook input", "hook forward", "hook output"} {
		if !strings.Contains(script, hook) {
			t.Fatalf("missing %q in script:\n%s", hook, script)
		}
	}
}

func TestClientIface_WebPortsAndWebRTC(t *testing.T) {
	script, err := Generate(baseVariantA())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	in := chainBody(t, script, "input")
	if !containsRule(in, `iif "wlan0" tcp dport { 80, 443 } accept`) {
		t.Fatalf("missing patron web ports rule on client iface; got:\n%s", strings.Join(in, "\n"))
	}
	if !containsRule(in, `iif "wlan0" udp dport 10000-20000 accept`) {
		t.Fatalf("missing WebRTC range rule on client iface; got:\n%s", strings.Join(in, "\n"))
	}
}

func TestAdminRule_ProfileGating(t *testing.T) {
	for _, tc := range []struct {
		name      string
		profile   model.FirewallProfile
		request   bool
		wantAdmin bool
	}{
		{"production_requested_denied", model.ProfilePatronFacing, true, false},
		{"production_lowcost_requested_denied", model.ProfileLowCostReplacement, true, false},
		{"lab_requested_allowed", model.ProfileLab, true, true},
		{"booth_requested_allowed", model.ProfileBoothDiagnostic, true, true},
		{"lab_not_requested", model.ProfileLab, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVariantA()
			cfg.Profile = tc.profile
			cfg.AdminOnClientIface = tc.request
			script, err := Generate(cfg)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			in := chainBody(t, script, "input")
			hasAdmin := false
			for _, ln := range in {
				if strings.Contains(ln, "admin/setup") {
					hasAdmin = true
				}
			}
			if hasAdmin != tc.wantAdmin {
				t.Fatalf("admin rule present=%v, want %v; input chain:\n%s", hasAdmin, tc.wantAdmin, strings.Join(in, "\n"))
			}
			// The admin rule, when present, must be on the client interface only.
			if tc.wantAdmin {
				want := `iif "wlan0" tcp dport 8443 accept`
				if !containsRule(in, want) {
					t.Fatalf("admin rule missing/incorrect; want %q in:\n%s", want, strings.Join(in, "\n"))
				}
			}
		})
	}
}

func TestAdminRule_CustomPort(t *testing.T) {
	cfg := baseVariantA()
	cfg.Profile = model.ProfileLab
	cfg.AdminOnClientIface = true
	cfg.AdminPort = 9443
	script, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	in := chainBody(t, script, "input")
	if !containsRule(in, `iif "wlan0" tcp dport 9443 accept`) {
		t.Fatalf("custom admin port not honored; got:\n%s", strings.Join(in, "\n"))
	}
}

// TestSysctlSettings_DisablesForwarding: the global knobs are not enough. IPv6
// forwarding is per-device (conf.<iface>.forwarding forwards on that device while
// conf.all reads 0) and a NIC appearing after boot inherits conf.default, so the
// `all` and `default` knobs of BOTH families must be written too (§9.1, §9.3).
func TestSysctlSettings_DisablesForwarding(t *testing.T) {
	s := SysctlSettings()
	for _, k := range []string{
		"net.ipv4.ip_forward",
		"net.ipv4.conf.all.forwarding",
		"net.ipv4.conf.default.forwarding",
		"net.ipv6.conf.all.forwarding",
		"net.ipv6.conf.default.forwarding",
	} {
		v, ok := s[k]
		if !ok {
			t.Fatalf("SysctlSettings missing key %q", k)
		}
		if v != "0" {
			t.Fatalf("SysctlSettings[%q] = %q, want \"0\"", k, v)
		}
	}
	// Nothing else may be in here: this map is written by a root helper.
	if len(s) != 5 {
		t.Fatalf("SysctlSettings has %d keys, want exactly the 5 forwarding knobs: %v", len(s), s)
	}
}

func TestGenerate_IPv6Server(t *testing.T) {
	cfg := baseVariantA()
	cfg.CinemaServerIP = "2001:db8::10"
	script, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := chainBody(t, script, "output")
	if !containsRule(out, `oif "eth0" ip6 daddr 2001:db8::10 tcp dport 4170 accept`) {
		t.Fatalf("IPv6 server should use ip6 keyword; got:\n%s", strings.Join(out, "\n"))
	}
}

func TestGenerate_OmitsOptionalRules(t *testing.T) {
	cfg := baseVariantB()
	cfg.WebRTCPortRange = ""
	cfg.PatronWebPorts = nil
	script, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(script, "udp dport") {
		t.Fatalf("empty WebRTCPortRange must omit the udp rule:\n%s", script)
	}
	if strings.Contains(script, "tcp dport {") {
		t.Fatalf("empty PatronWebPorts must omit the web ports rule:\n%s", script)
	}
}

func TestGenerate_ValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing theater iface", func(c *Config) { c.TheaterIface = "" }},
		{"missing client iface", func(c *Config) { c.ClientIface = "" }},
		{"same interfaces", func(c *Config) { c.ClientIface = c.TheaterIface }},
		{"missing server ip", func(c *Config) { c.CinemaServerIP = "" }},
		{"bad server ip", func(c *Config) { c.CinemaServerIP = "not-an-ip" }},
		{"variantA missing port", func(c *Config) { c.ACSPPort = 0 }},
		{"variantA bad port", func(c *Config) { c.ACSPPort = 70000 }},
		{"unknown initiation", func(c *Config) { c.Initiation = model.ACSPInitiation("bogus") }},
		{"bad patron port", func(c *Config) { c.PatronWebPorts = []int{80, 0} }},
		{"bad webrtc range form", func(c *Config) { c.WebRTCPortRange = "10000" }},
		{"webrtc lo>=hi", func(c *Config) { c.WebRTCPortRange = "20000-10000" }},
		{"bad webrtc port", func(c *Config) { c.WebRTCPortRange = "0-20000" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVariantA()
			tc.mutate(&cfg)
			if _, err := Generate(cfg); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestGenerate_VariantBMissingLocalPort(t *testing.T) {
	cfg := baseVariantB()
	cfg.LocalACSPPort = 0
	if _, err := Generate(cfg); err == nil {
		t.Fatalf("Variant B without LocalACSPPort should error")
	}
}

// containsRule reports whether any line contains the wanted rule substring.
func containsRule(lines []string, want string) bool {
	for _, ln := range lines {
		if strings.Contains(ln, want) {
			return true
		}
	}
	return false
}

// TestInvalidInterfaceNamesRejected guards the defense-in-depth iface validation
// (adversarial review finding): names that violate IFNAMSIZ or the safe charset
// must be rejected before reaching the root-applied ruleset.
func TestInvalidInterfaceNamesRejected(t *testing.T) {
	base := Config{
		Profile: model.ProfileBoothDiagnostic, ClientIface: "wlan0",
		CinemaServerIP: "10.10.20.15", ACSPPort: 4170, Initiation: model.ACSInitiated,
		PatronWebPorts: []int{80},
	}
	bad := []string{"this-name-is-way-too-long", "eth 0", "eth0;drop", "eth\"0", "eth/0", ""}
	for _, name := range bad {
		c := base
		c.TheaterIface = name
		if _, err := Generate(c); err == nil {
			t.Errorf("expected rejection of theater iface %q", name)
		}
	}
	// A normal name still works.
	c := base
	c.TheaterIface = "eth0"
	if _, err := Generate(c); err != nil {
		t.Errorf("valid iface rejected: %v", err)
	}
}
