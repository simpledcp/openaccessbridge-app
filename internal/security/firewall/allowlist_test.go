package firewall

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// TestAllowlist_MultipleIPsOneFamily covers the booth where the ACSP endpoint and
// the media block (or a redundant server) are distinct hosts (§9.1 "the configured
// cinema server/media-block IP(s)"): one rule, an nft anonymous set, no widening.
func TestAllowlist_MultipleIPsOneFamily(t *testing.T) {
	cases := []struct {
		name      string
		init      model.ACSPInitiation
		chain     string
		wantRule  string
		wantCount int
	}{
		{
			name: "variantA_output_set", init: model.ACSInitiated, chain: "output",
			wantRule:  `oif "eth0" ip daddr { 192.0.2.10, 192.0.2.11, 192.0.2.12 } tcp dport 4170 accept`,
			wantCount: 1,
		},
		{
			name: "variantB_input_set", init: model.ServerInitiated, chain: "input",
			wantRule:  `iif "eth0" ip saddr { 192.0.2.10, 192.0.2.11, 192.0.2.12 } tcp dport 4171 accept`,
			wantCount: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseVariantA()
			cfg.Initiation = tc.init
			cfg.LocalACSPPort = 4171
			cfg.CinemaServerIP = ""
			cfg.CinemaServerIPs = []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"}
			script, err := Generate(cfg)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := chainBody(t, script, tc.chain)
			if !containsRule(body, tc.wantRule) {
				t.Fatalf("missing rule %q; %s chain:\n%s", tc.wantRule, tc.chain, strings.Join(body, "\n"))
			}
			// Exactly one theater rule — the allowlist collapses into one set, it
			// does not multiply into per-address rules.
			n := 0
			for _, ln := range body {
				if strings.Contains(ln, "192.0.2.") {
					n++
				}
			}
			if n != tc.wantCount {
				t.Fatalf("theater rules = %d, want %d:\n%s", n, tc.wantCount, strings.Join(body, "\n"))
			}
		})
	}
}

// TestAllowlist_MixedFamiliesSplitIntoTwoRules pins the bug a plural allowlist
// exposes: the nft address-family keyword differs, so v4 and v6 entries cannot
// share one rule. Each family gets its own rule, in a deterministic order.
func TestAllowlist_MixedFamiliesSplitIntoTwoRules(t *testing.T) {
	cfg := baseVariantA()
	cfg.CinemaServerIP = ""
	cfg.CinemaServerIPs = []string{"192.0.2.10", "2001:db8::10", "192.0.2.11"}
	script, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := chainBody(t, script, "output")
	wantV4 := `oif "eth0" ip daddr { 192.0.2.10, 192.0.2.11 } tcp dport 4170 accept`
	wantV6 := `oif "eth0" ip6 daddr 2001:db8::10 tcp dport 4170 accept`
	if !containsRule(out, wantV4) {
		t.Errorf("missing IPv4 rule %q; got:\n%s", wantV4, strings.Join(out, "\n"))
	}
	if !containsRule(out, wantV6) {
		t.Errorf("missing IPv6 rule %q; got:\n%s", wantV6, strings.Join(out, "\n"))
	}
	// A single rule must never mix families (nft would reject it).
	for _, ln := range out {
		if strings.Contains(ln, "2001:db8") && strings.Contains(ln, "192.0.2") {
			t.Errorf("one rule mixes address families: %q", ln)
		}
		if strings.Contains(ln, "ip6 daddr") && strings.Contains(ln, "192.0.2") {
			t.Errorf("ip6 rule carries an IPv4 address: %q", ln)
		}
	}
}

// TestAllowlist_ScalarAliasUnion keeps configuration written before the allowlist
// became plural working, and unions it with the plural field without duplicates.
func TestAllowlist_ScalarAliasUnion(t *testing.T) {
	cases := []struct {
		name   string
		scalar string
		plural []string
		want   []string
	}{
		{"scalar only", "192.0.2.10", nil, []string{"192.0.2.10"}},
		{"plural only", "", []string{"192.0.2.10", "192.0.2.11"}, []string{"192.0.2.10", "192.0.2.11"}},
		{"union", "192.0.2.10", []string{"192.0.2.11"}, []string{"192.0.2.10", "192.0.2.11"}},
		{"duplicate collapses", "192.0.2.10", []string{"192.0.2.10"}, []string{"192.0.2.10"}},
		{"whitespace trimmed", " 192.0.2.10 ", []string{"  "}, []string{"192.0.2.10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{CinemaServerIP: tc.scalar, CinemaServerIPs: tc.plural}
			got := cfg.Allowlist()
			if len(got) != len(tc.want) {
				t.Fatalf("Allowlist() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Allowlist() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestAllowlist_ValidationErrors: an empty allowlist and any invalid entry are
// refused rather than emitting a useless or over-wide theater rule.
func TestAllowlist_ValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty allowlist", func(c *Config) { c.CinemaServerIP = ""; c.CinemaServerIPs = nil }},
		{"empty plural entries", func(c *Config) { c.CinemaServerIP = ""; c.CinemaServerIPs = []string{" ", ""} }},
		{"one bad entry", func(c *Config) {
			c.CinemaServerIP = ""
			c.CinemaServerIPs = []string{"192.0.2.10", "not-an-ip"}
		}},
		{"cidr not allowed", func(c *Config) {
			c.CinemaServerIP = ""
			c.CinemaServerIPs = []string{"192.0.2.0/24"}
		}},
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

// TestForwardChain_StaysEmptyWithMultiIPAllowlist re-asserts guardrail #2 for the
// new rule shape: however many theater addresses are allowlisted, the forward
// chain still carries zero accepts.
func TestForwardChain_StaysEmptyWithMultiIPAllowlist(t *testing.T) {
	for _, init := range []model.ACSPInitiation{model.ACSInitiated, model.ServerInitiated} {
		cfg := baseVariantA()
		cfg.Initiation = init
		cfg.LocalACSPPort = 4171
		cfg.CinemaServerIPs = []string{"192.0.2.11", "2001:db8::10"}
		script, err := Generate(cfg)
		if err != nil {
			t.Fatalf("Generate(%s): %v", init, err)
		}
		if n := countAccept(chainBody(t, script, "forward")); n != 0 {
			t.Errorf("%s: forward chain must have zero accept rules, got %d", init, n)
		}
	}
}

// TestParseAllowlist covers the stored-string form: several IPs in one scalar
// setting, separated by commas and/or whitespace.
func TestParseAllowlist(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"192.0.2.10", []string{"192.0.2.10"}},
		{"192.0.2.10,192.0.2.11", []string{"192.0.2.10", "192.0.2.11"}},
		{"192.0.2.10, 192.0.2.11 ; 2001:db8::10", []string{"192.0.2.10", "192.0.2.11", "2001:db8::10"}},
		{"192.0.2.10 192.0.2.10", []string{"192.0.2.10"}},
	}
	for _, tc := range cases {
		got := ParseAllowlist(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseAllowlist(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseAllowlist(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}
