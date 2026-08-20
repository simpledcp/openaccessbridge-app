package verify

import (
	"errors"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// nftJSON assembles a captured-shape `nft -j list table inet captionbridge`
// document from per-chain descriptions, so the fixtures stay readable.
type chainFixture struct {
	name    string
	policy  string
	accepts int
	drops   int
}

func nftJSON(table bool, chains ...chainFixture) string {
	var b strings.Builder
	b.WriteString(`{"nftables":[{"metainfo":{"version":"1.0.9","json_schema_version":1}}`)
	if table {
		b.WriteString(`,{"table":{"family":"inet","name":"captionbridge","handle":1}}`)
	}
	for _, c := range chains {
		b.WriteString(`,{"chain":{"family":"inet","table":"captionbridge","handle":1,"name":"` + c.name +
			`","type":"filter","hook":"` + c.name + `","prio":0`)
		if c.policy != "" {
			b.WriteString(`,"policy":"` + c.policy + `"`)
		}
		b.WriteString(`}}`)
		for i := 0; i < c.accepts; i++ {
			b.WriteString(`,{"rule":{"family":"inet","table":"captionbridge","chain":"` + c.name +
				`","handle":4,"expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}]}}`)
		}
		for i := 0; i < c.drops; i++ {
			b.WriteString(`,{"rule":{"family":"inet","table":"captionbridge","chain":"` + c.name +
				`","handle":5,"expr":[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"wlan0"}},{"drop":null}]}}`)
		}
	}
	b.WriteString(`]}`)
	return b.String()
}

// compliant is the ruleset firewall.Generate produces: three base chains, all
// policy drop, accepts only in input/output, none in forward.
func compliant() string {
	return nftJSON(true,
		chainFixture{name: "input", policy: "drop", accepts: 4},
		chainFixture{name: "forward", policy: "drop"},
		chainFixture{name: "output", policy: "drop", accepts: 3},
	)
}

func TestParseNFTRuleset(t *testing.T) {
	cases := []struct {
		name           string
		json           string
		wantErr        bool
		wantDefaultDen bool
		wantFwdAccepts int
	}{
		{
			name: "compliant ruleset", json: compliant(),
			wantDefaultDen: true, wantFwdAccepts: 0,
		},
		{
			name: "forward chain accepts", json: nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "drop", accepts: 1},
				chainFixture{name: "output", policy: "drop"}),
			wantDefaultDen: true, wantFwdAccepts: 1,
		},
		{
			name: "forward policy accept", json: nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "accept"},
				chainFixture{name: "output", policy: "drop"}),
			wantDefaultDen: false,
		},
		{
			name: "input policy accept", json: nftJSON(true,
				chainFixture{name: "input", policy: "accept"},
				chainFixture{name: "forward", policy: "drop"},
				chainFixture{name: "output", policy: "drop"}),
			wantDefaultDen: false,
		},
		{
			name: "chain missing a policy is not default-deny", json: nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward"},
				chainFixture{name: "output", policy: "drop"}),
			wantDefaultDen: false,
		},
		{
			name: "output chain absent", json: nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "drop"}),
			wantDefaultDen: false,
		},
		{
			name: "table absent", json: `{"nftables":[{"metainfo":{"version":"1.0.9"}}]}`,
			wantDefaultDen: false,
		},
		{
			name: "drop rules are not accepts", json: nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "drop", drops: 2},
				chainFixture{name: "output", policy: "drop"}),
			wantDefaultDen: true, wantFwdAccepts: 0,
		},
		{name: "not JSON", json: "nft: command not found", wantErr: true},
		{
			name: "chains without their table", json: nftJSON(false,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "drop"},
				chainFixture{name: "output", policy: "drop"}),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := ParseNFTRuleset([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNFTRuleset: %v", err)
			}
			if st.DefaultDeny() != tc.wantDefaultDen {
				t.Errorf("DefaultDeny() = %v, want %v", st.DefaultDeny(), tc.wantDefaultDen)
			}
			if st.ForwardAccepts() != tc.wantFwdAccepts {
				t.Errorf("ForwardAccepts() = %d, want %d", st.ForwardAccepts(), tc.wantFwdAccepts)
			}
		})
	}
}

// TestParseNFTRuleset_IgnoresOtherTables: a compliant-looking chain belonging to
// another table must not make OUR table look loaded or default-deny.
func TestParseNFTRuleset_IgnoresOtherTables(t *testing.T) {
	doc := `{"nftables":[
	  {"table":{"family":"inet","name":"filter","handle":1}},
	  {"chain":{"family":"inet","table":"filter","name":"forward","policy":"drop"}},
	  {"rule":{"family":"inet","table":"filter","chain":"forward","expr":[{"accept":null}]}}
	]}`
	st, err := ParseNFTRuleset([]byte(doc))
	if err != nil {
		t.Fatalf("ParseNFTRuleset: %v", err)
	}
	if st.TablePresent {
		t.Error("another table must not be reported as ours")
	}
	if st.DefaultDeny() {
		t.Error("another table's drop policy must not count as our default-deny")
	}
	if st.ForwardAccepts() != 0 {
		t.Errorf("another table's accept rule was counted: %d", st.ForwardAccepts())
	}
}

// TestRun_FirewallIntrospection is the core of §9.3's "default-deny active with
// no forward-accepts": the pass must PROVE the posture from the loaded ruleset,
// not accept a caller's assertion.
func TestRun_FirewallIntrospection(t *testing.T) {
	cases := []struct {
		name string
		// asserted is the (untrustworthy) caller-supplied fact; every case sets it
		// true so a regression back to trusting it is caught.
		reader        RulesetReader
		wantActive    bool
		wantIntrospec bool
		wantPass      bool
		wantCodes     []string
	}{
		{
			name:       "observed compliant ruleset",
			reader:     parseReader(t, compliant()),
			wantActive: true, wantIntrospec: true, wantPass: true, wantCodes: nil,
		},
		{
			name:       "table not loaded",
			reader:     StaticRulesetReader(RulesetStatus{Chains: map[string]ChainStatus{}}, nil),
			wantActive: false, wantIntrospec: true, wantPass: false,
			wantCodes: []string{model.WarnFirewallInactive},
		},
		{
			name: "forward chain accepts",
			reader: parseReader(t, nftJSON(true,
				chainFixture{name: "input", policy: "drop"},
				chainFixture{name: "forward", policy: "drop", accepts: 1},
				chainFixture{name: "output", policy: "drop"})),
			wantActive: false, wantIntrospec: true, wantPass: false,
			// A forward-chain accept is the appliance routing between zones, so it
			// raises the forwarding warning as well as the firewall one.
			wantCodes: []string{model.WarnForwardingEnabled, model.WarnFirewallInactive},
		},
		{
			name:       "cannot confirm fails loud",
			reader:     StaticRulesetReader(RulesetStatus{}, errors.New("nft list failed: Operation not permitted")),
			wantActive: false, wantIntrospec: false, wantPass: false,
			wantCodes: []string{model.WarnFirewallInactive},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Run(Config{
				Paths:   fixture(t, "0", "0", false),
				Profile: model.ProfilePatronFacing,
				// The caller asserts the firewall is active; introspection must
				// override it in every direction.
				FirewallActive: true,
				Ruleset:        tc.reader,
			})
			if rep.FirewallActive != tc.wantActive {
				t.Errorf("FirewallActive = %v, want %v", rep.FirewallActive, tc.wantActive)
			}
			if rep.FirewallIntrospected != tc.wantIntrospec {
				t.Errorf("FirewallIntrospected = %v, want %v", rep.FirewallIntrospected, tc.wantIntrospec)
			}
			if rep.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (warnings %v)", rep.Pass, tc.wantPass, codes(rep.Warnings))
			}
			if got := codes(rep.Warnings); !equalStrings(got, tc.wantCodes) {
				t.Errorf("warning codes = %v, want %v", got, tc.wantCodes)
			}
		})
	}
}

// TestRun_NoReaderLeavesFirewallUnproven documents the fallback: without a reader
// the caller's assertion is used, and the report says plainly that the posture
// was NOT introspected so a dashboard cannot present it as verified.
func TestRun_NoReaderLeavesFirewallUnproven(t *testing.T) {
	rep := Run(Config{Paths: fixture(t, "0", "0", false), Profile: model.ProfileLab, FirewallActive: true})
	if !rep.FirewallActive {
		t.Error("the caller-supplied fact should still be honored when no reader is configured")
	}
	if rep.FirewallIntrospected {
		t.Error("FirewallIntrospected must be false when nothing read the ruleset")
	}
}

// TestNFTRulesetReader_OnThisHost exercises the real reader. Reading the ruleset
// is NOT gated by BLOCKER B5 (that gates applying), but an unprivileged host
// still cannot open the netlink socket — and the contract there is an error
// ("cannot confirm"), never a silent pass.
func TestNFTRulesetReader_OnThisHost(t *testing.T) {
	st, err := NFTRulesetReader()()
	if err != nil {
		if st.DefaultDeny() {
			t.Fatalf("a failed read must never report a default-deny ruleset: %+v", st)
		}
		t.Logf("ruleset not introspectable on this host (expected without root): %v", err)
		return
	}
	// If it did run, the observation must be self-consistent.
	if st.TablePresent && st.Chains == nil {
		t.Fatalf("table present but no chains parsed: %+v", st)
	}
	t.Logf("observed ruleset: table_present=%v default_deny=%v forward_accepts=%d",
		st.TablePresent, st.DefaultDeny(), st.ForwardAccepts())
}

// parseReader builds a RulesetReader over a captured nft JSON document.
func parseReader(t *testing.T, doc string) RulesetReader {
	t.Helper()
	st, err := ParseNFTRuleset([]byte(doc))
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return StaticRulesetReader(st, nil)
}
