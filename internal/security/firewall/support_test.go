package firewall

import (
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

func supportCfg(sup *SupportEgress) Config {
	return Config{
		Profile: model.ProfilePatronFacing, TheaterIface: "eth0", ClientIface: "wlan0",
		CinemaServerIP: "10.0.0.5", ACSPPort: 4170, Initiation: model.ACSInitiated,
		SupportEgress: sup,
	}
}

// §9.4: support mode is "outbound-only". The window must appear in the OUTPUT
// chain and nowhere else — an inbound accept would turn it into a listening port
// on an appliance whose SSH is deliberately disabled, which is the opposite of
// what §9.4 is for.
func TestTheSupportWindowIsOutboundOnly(t *testing.T) {
	script, err := Generate(supportCfg(&SupportEgress{Iface: "eth0", Host: "203.0.113.10", Port: 443}))
	if err != nil {
		t.Fatal(err)
	}
	output := chainOf(t, script, "output")
	if !strings.Contains(output, "daddr 203.0.113.10 tcp dport 443 accept") {
		t.Fatalf("the support window is not in the output chain:\n%s", output)
	}
	input := stripComments(chainOf(t, script, "input"))
	if strings.Contains(input, "203.0.113.10") || strings.Contains(input, "support") {
		t.Fatalf("a support rule appeared in the INPUT chain:\n%s", input)
	}
	// Comments are stripped first: the forward chain's own comment contains the
	// word "accept" (as "NEVER accept"), and matching that would make this test
	// pass or fail on prose rather than on rules.
	forward := stripComments(chainOf(t, script, "forward"))
	if strings.Contains(forward, "accept") {
		t.Fatalf("the forward chain gained an accept:\n%s", forward)
	}
}

// The window is one address and one port. A bare interface accept would be an
// unrestricted egress hole opened by a support switch — the single most dangerous
// way to implement this feature.
func TestTheSupportWindowIsNeverABareAccept(t *testing.T) {
	script, err := Generate(supportCfg(&SupportEgress{Iface: "eth0", Host: "203.0.113.10", Port: 443}))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "support session") {
			continue
		}
		if !strings.Contains(line, "daddr") || !strings.Contains(line, "dport") {
			t.Fatalf("the support rule is not scoped to an address and port: %q", line)
		}
	}
}

// A partially configured window is refused, never rendered. Rendering it would
// have to be either a hole or silence, and silence would report an open window
// that permits nothing.
func TestAnIncompleteSupportWindowIsRefused(t *testing.T) {
	for name, sup := range map[string]*SupportEgress{
		"no interface": {Host: "203.0.113.10", Port: 443},
		"no host":      {Iface: "eth0", Port: 443},
		"hostname":     {Iface: "eth0", Host: "support.example.com", Port: 443},
		"no port":      {Iface: "eth0", Host: "203.0.113.10"},
		"port 0":       {Iface: "eth0", Host: "203.0.113.10", Port: 0},
		"port too big": {Iface: "eth0", Host: "203.0.113.10", Port: 70000},
	} {
		if _, err := Generate(supportCfg(sup)); err == nil {
			t.Errorf("%s: an incomplete support window was rendered", name)
		}
	}
}

// With no session open there must be no window at all. This is the state the
// appliance spends its life in, and it is the one worth pinning.
func TestNoSupportSessionMeansNoEgressRule(t *testing.T) {
	script, err := Generate(supportCfg(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "support") {
		t.Fatalf("a support rule exists with no session open:\n%s", script)
	}
}

// An IPv6 endpoint needs the ip6 match keyword; one rule cannot carry both.
func TestAnIPv6SupportEndpointUsesTheIPv6Keyword(t *testing.T) {
	script, err := Generate(supportCfg(&SupportEgress{Iface: "eth0", Host: "2001:db8::5", Port: 8443}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "ip6 daddr 2001:db8::5") {
		t.Fatalf("the IPv6 support endpoint did not use the ip6 keyword:\n%s", script)
	}
}

// stripComments removes everything after a # on each line, so assertions are
// about rules rather than about the words used to describe them.
func stripComments(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// chainOf returns the text of one named chain.
func chainOf(t *testing.T, script, name string) string {
	t.Helper()
	marker := "chain " + name + " {"
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("no %s chain in the script", name)
	}
	rest := script[i:]
	j := strings.Index(rest, "\n  }")
	if j < 0 {
		t.Fatalf("unterminated %s chain", name)
	}
	return rest[:j]
}
