package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The nftables table CaptionBridge owns (§9.1). Verification asserts the posture
// of THIS table; anything else on the box is not ours to claim.
const (
	TableFamily = "inet"
	TableName   = "captionbridge"
)

// The three base chains §9.1 defines. All three must be policy drop, and the
// forward chain must carry no accept rules at all.
const (
	ChainInput   = "input"
	ChainForward = "forward"
	ChainOutput  = "output"
)

// ChainStatus is one chain as OBSERVED in the loaded ruleset.
type ChainStatus struct {
	Present     bool `json:"present"`
	PolicyDrop  bool `json:"policy_drop"`
	AcceptRules int  `json:"accept_rules"`
}

// RulesetStatus is the loaded nftables ruleset as observed by introspection. It
// is a MEASUREMENT, never an assertion: it exists so §9.3's "default-deny active
// with no forward-accepts" can be proven from the kernel's own view of the table
// instead of inferred from a flag or from a helper process answering a ping.
type RulesetStatus struct {
	// TablePresent is false when the table is confirmed ABSENT (nft ran and said
	// so) — that is a loud failure, not an inconclusive result. An inconclusive
	// read is reported as an error from the RulesetReader instead.
	TablePresent bool                   `json:"table_present"`
	Chains       map[string]ChainStatus `json:"chains"`
}

// Chain returns the named chain's status (zero value when the chain is absent).
func (s RulesetStatus) Chain(name string) ChainStatus { return s.Chains[name] }

// DefaultDeny reports whether the table is loaded and all three base chains exist
// with policy drop (§9.1 "Default-deny firewall, nftables").
func (s RulesetStatus) DefaultDeny() bool {
	if !s.TablePresent {
		return false
	}
	for _, name := range []string{ChainInput, ChainForward, ChainOutput} {
		c := s.Chain(name)
		if !c.Present || !c.PolicyDrop {
			return false
		}
	}
	return true
}

// ForwardAccepts returns the number of accept rules in the forward chain. §9.1 is
// absolute about this one: "No forward-chain accepts at all."
func (s RulesetStatus) ForwardAccepts() int { return s.Chain(ChainForward).AcceptRules }

// RulesetReader observes the loaded ruleset. It returns an error only when the
// posture CANNOT BE DETERMINED (nft missing, not permitted, unparsable) — which
// the verification pass treats as a loud failure, never as a pass. A confirmed
// "the table is not loaded" is a RulesetStatus with TablePresent false and a nil
// error. Readers are injectable so tests run against captured nft JSON.
type RulesetReader func() (RulesetStatus, error)

// StaticRulesetReader returns a reader that always yields the given observation.
// It is for tests and for callers that have already read the ruleset.
func StaticRulesetReader(s RulesetStatus, err error) RulesetReader {
	return func() (RulesetStatus, error) { return s, err }
}

// nftTimeout bounds the introspection command so a wedged nft cannot stall the
// periodic runtime verification loop (§9.3 "periodically at runtime").
const nftTimeout = 5 * time.Second

// NFTRulesetReader returns a RulesetReader that introspects the live ruleset with
// `nft -j list table inet captionbridge`. Listing needs no ruleset change and no
// --allow-apply gate: BLOCKER B5 gates APPLYING rules, not reading them. On a dev
// host without root, nft reports "Operation not permitted" and the reader returns
// an error — "cannot confirm", which must fail loud (D17 stance), never pass.
func NFTRulesetReader() RulesetReader {
	return func() (RulesetStatus, error) {
		bin, err := exec.LookPath("nft")
		if err != nil {
			return RulesetStatus{}, errors.New("nft is not installed; the ruleset cannot be introspected")
		}
		ctx, cancel := context.WithTimeout(context.Background(), nftTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-j", "list", "table", TableFamily, TableName)
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			stderr := ""
			if errors.As(err, &ee) {
				stderr = string(ee.Stderr)
			}
			// nft distinguishes "the table does not exist" (a CONFIRMED absence —
			// the firewall is genuinely not loaded) from "I could not look"
			// (permission denied, timeout) which is inconclusive.
			if tableAbsent(stderr) {
				return RulesetStatus{TablePresent: false, Chains: map[string]ChainStatus{}}, nil
			}
			return RulesetStatus{}, fmt.Errorf("nft list failed: %s", firstLine(stderr, err))
		}
		return ParseNFTRuleset(out)
	}
}

// tableAbsent recognizes nft's "no such table" error text.
func tableAbsent(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such file or directory") || strings.Contains(s, "does not exist")
}

// firstLine renders a short, single-line reason for a warning message. nft's
// stderr carries no caption text or audio — only rule/table diagnostics — but it
// is still trimmed so a dashboard warning stays readable.
func firstLine(stderr string, err error) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.TrimSpace(s)
}

// nft's JSON output (`-j`) is a single object holding a list of elements, each of
// which is a one-key object: metainfo, table, chain, rule, set, …
type nftDoc struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type nftTable struct {
	Family string `json:"family"`
	Name   string `json:"name"`
}

type nftChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

type nftRule struct {
	Family string            `json:"family"`
	Table  string            `json:"table"`
	Chain  string            `json:"chain"`
	Expr   []json.RawMessage `json:"expr"`
}

// ParseNFTRuleset parses `nft -j list table inet captionbridge` output into an
// observation: which chains exist, whether each is policy drop, and how many
// rules in each chain end in an accept verdict. It ignores elements belonging to
// other tables so a shared box cannot make our table look compliant.
func ParseNFTRuleset(data []byte) (RulesetStatus, error) {
	var doc nftDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return RulesetStatus{}, fmt.Errorf("nft JSON could not be parsed: %w", err)
	}
	st := RulesetStatus{Chains: map[string]ChainStatus{}}
	for _, elem := range doc.Nftables {
		if raw, ok := elem["table"]; ok {
			var t nftTable
			if err := json.Unmarshal(raw, &t); err != nil {
				return RulesetStatus{}, fmt.Errorf("nft table element: %w", err)
			}
			if t.Family == TableFamily && t.Name == TableName {
				st.TablePresent = true
			}
			continue
		}
		if raw, ok := elem["chain"]; ok {
			var c nftChain
			if err := json.Unmarshal(raw, &c); err != nil {
				return RulesetStatus{}, fmt.Errorf("nft chain element: %w", err)
			}
			if c.Family != TableFamily || c.Table != TableName {
				continue
			}
			cur := st.Chains[c.Name]
			cur.Present = true
			// A base chain carries an explicit policy; a regular chain has none,
			// which is NOT default-deny for a hooked chain we require to drop.
			cur.PolicyDrop = c.Policy == "drop"
			st.Chains[c.Name] = cur
			continue
		}
		if raw, ok := elem["rule"]; ok {
			var r nftRule
			if err := json.Unmarshal(raw, &r); err != nil {
				return RulesetStatus{}, fmt.Errorf("nft rule element: %w", err)
			}
			if r.Family != TableFamily || r.Table != TableName {
				continue
			}
			if !ruleAccepts(r.Expr) {
				continue
			}
			cur := st.Chains[r.Chain]
			cur.Present = true
			cur.AcceptRules++
			st.Chains[r.Chain] = cur
		}
	}
	if !st.TablePresent && len(st.Chains) > 0 {
		// Chains for our table without the table element itself would mean the
		// output was truncated or hand-assembled; refuse to call it a pass.
		return RulesetStatus{}, errors.New("nft output names our chains but not the table; refusing to interpret it")
	}
	return st, nil
}

// ruleAccepts reports whether a rule's expression list ends in an accept verdict.
// nft renders verdicts as single-key objects ({"accept": null}), so a rule that
// merely MATCHES on the word accept is not miscounted.
func ruleAccepts(expr []json.RawMessage) bool {
	for _, e := range expr {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e, &obj); err != nil {
			continue
		}
		if _, ok := obj["accept"]; ok {
			return true
		}
	}
	return false
}
