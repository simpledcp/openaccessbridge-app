package firewall

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func replaceCfg() Config {
	return Config{
		Profile: model.ProfilePatronFacing, TheaterIface: "eth0", ClientIface: "wlan0",
		CinemaServerIP: "10.0.0.5", ACSPPort: 4170, Initiation: model.ACSInitiated,
		PatronWebPorts: []int{80}, WebRTCPortRange: "10000-20000",
	}
}

// The generated script must REPLACE the table, not add to it.
//
// `nft -f` on a table definition has implicit `add` semantics: applied at a table
// that already exists, it appends every rule again and leaves the previous ones
// in place. After a configuration change that means the OLD rules keep
// accepting — the previous cinema server's address after it is corrected, or the
// admin-on-patron-interface rule after a switch to a production profile. The new
// posture would be applied, the old one would still be live, and introspection
// would find the expected rules present and report the posture verified.
func TestTheRulesetReplacesTheTableRatherThanAppendingToIt(t *testing.T) {
	script, err := Generate(replaceCfg())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if len(lines) < 3 {
		t.Fatalf("script is only %d lines", len(lines))
	}
	// The bare table line must come first so the delete cannot fail on a fresh
	// boot, and the delete must precede the definition.
	if strings.TrimSpace(lines[0]) != "table inet captionbridge" {
		t.Fatalf("first line is %q; it must create the table so the delete is safe", lines[0])
	}
	if strings.TrimSpace(lines[1]) != "delete table inet captionbridge" {
		t.Fatalf("second line is %q; without a delete, applying appends to whatever is loaded", lines[1])
	}
	if !strings.Contains(lines[2], "table inet captionbridge {") {
		t.Fatalf("third line is %q; the definition must follow the delete", lines[2])
	}
	// Exactly one delete: a second one would fail the transaction.
	if n := strings.Count(script, "delete table"); n != 1 {
		t.Fatalf("%d delete-table lines; want exactly 1", n)
	}
}

// The replacement must be ATOMIC. `nft -f` applies a whole file as one
// transaction, so the delete and the new definition land together and there is no
// instant in which the appliance has no table — which, with the chains gone,
// means no default-deny either.
func TestTheReplacementIsOneTransaction(t *testing.T) {
	script, err := Generate(replaceCfg())
	if err != nil {
		t.Fatal(err)
	}
	// A flush/commit split across separate invocations is what this guards
	// against; everything must be in the single emitted script.
	for _, forbidden := range []string{"nft ", "\n\n\n"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("the script contains %q: it must be one file applied in one transaction", forbidden)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(script), "}") {
		t.Fatal("the script does not end with the table definition")
	}
}

// A stricter posture applied over a looser one must not leave the looser rule
// behind. This is the concrete case: Booth Diagnostic permits the admin UI on the
// patron interface, Patron-Facing never does (§8.4).
func TestSwitchingToAStricterProfileDoesNotLeaveTheOldRuleLive(t *testing.T) {
	loose := replaceCfg()
	loose.Profile = model.ProfileBoothDiagnostic
	loose.AdminOnClientIface = true
	looseScript, err := Generate(loose)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(looseScript, "admin/setup UI") {
		t.Fatal("the diagnostic profile did not emit the admin rule this test is about")
	}

	strict := replaceCfg() // patron-facing, admin never permitted
	strict.AdminOnClientIface = true
	strictScript, err := Generate(strict)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strictScript, "admin/setup UI") {
		t.Fatal("a production profile emitted an admin rule on the patron interface")
	}
	// And applying the strict script must remove what the loose one installed,
	// which is exactly what the delete line is for.
	if !strings.Contains(strictScript, "delete table inet captionbridge") {
		t.Fatal("the stricter ruleset does not clear the table it is replacing: the admin rule would survive")
	}
}
