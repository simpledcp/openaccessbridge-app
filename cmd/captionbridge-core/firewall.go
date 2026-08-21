package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

// firewallClient is core's half of the loopback link to the privileged helper
// (§3.5). It does three things the dashboard depends on:
//
//   - APPLIES the profile-derived ruleset through the helper (§9.1), rather than
//     leaving the generated rules as documentation;
//   - VERIFIES the posture through the helper, which runs as root and can read
//     the loaded nftables table, so "firewall active" is a measurement (§9.3);
//   - reports the helper's own liveness (§3.5).
//
// When the helper is unreachable, core still runs the pass LOCALLY so the
// dashboard gets a measured posture rather than nothing — and the unreachable
// helper is reported as its own fault, never as "the firewall is down".
type firewallClient struct {
	sockPath string
	log      *logging.Logger
	// localPaths and localRuleset are the fallback pass's introspection edges.
	localPaths   verify.Paths
	localRuleset verify.RulesetReader

	mu sync.Mutex
	// appliedKey is the ruleset config last accepted by the helper, so an
	// unchanged configuration is not re-applied every cycle.
	appliedKey string
	applied    bool
	// rulesetVersion identifies the ruleset actually installed, for the §10.8
	// record: "<profile>@<short digest>".
	rulesetVersion string
	// applyErr is the helper's reason for refusing the derived ruleset. It is a
	// loud condition on its own: what is loaded is then not what is configured.
	applyErr string
	report   verify.Report
	alive    bool
	lastSeen *time.Time
	detail   string
}

func newFirewallClient(sockPath string, log *logging.Logger) *firewallClient {
	return &firewallClient{
		sockPath: sockPath, log: log,
		localPaths: verify.DefaultPaths(),
		// Reading the ruleset needs no root and no --allow-apply: B5 gates
		// APPLYING rules, not observing them (§9.3).
		localRuleset: verify.NFTRulesetReader(),
	}
}

// ensureApplied sends the ruleset config to the helper when it differs from the
// one already accepted. The helper answers with whether rules were really
// installed (it is a dry-run without root + --allow-apply, BLOCKER B5), and that
// answer — not the fact that a helper is running — is what core records.
func (c *firewallClient) ensureApplied(cfg firewall.Config) {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return
	}
	key := digest(payload)
	c.mu.Lock()
	unchanged := key == c.appliedKey
	c.mu.Unlock()
	if unchanged {
		return
	}

	resp, err := c.request(ipc.Message{Type: ipc.TypeFirewallApply, ID: "apply", Payload: payload})
	if err != nil {
		c.noteDown()
		return
	}
	var res struct {
		Applied bool   `json:"applied"`
		Ruleset string `json:"ruleset"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(resp.Payload, &res)
	c.noteUp()

	c.mu.Lock()
	defer c.mu.Unlock()
	if res.Error != "" {
		// A ruleset the helper will not generate is a configuration fault. Say so
		// and do not record the key, so the next cycle tries again.
		c.applied, c.appliedKey, c.applyErr = false, "", res.Error
		// The helper's reason can quote operator-entered configuration (an
		// interface name, an allowlist entry), so the LOG gets a class and the
		// operator-facing warning gets the detail (§9.4: no operator free text in
		// logs).
		c.log.Error(logging.EvVerificationFailed,
			logging.Field("phase", "firewall_apply_refused"),
			logging.Field("reason", "ruleset_rejected"))
		return
	}
	c.appliedKey, c.applied, c.applyErr = key, res.Applied, ""
	c.rulesetVersion = string(cfg.Profile) + "@" + digest([]byte(res.Ruleset))[:8]
	c.log.Info(logging.EvFirewallProfileApplied,
		logging.Field("profile", string(cfg.Profile)),
		logging.Field("applied", res.Applied),
		logging.Field("ruleset_version", c.rulesetVersion))
}

// verifyPosture runs a §9.3 verification pass, preferring the privileged helper
// (which can read the loaded ruleset even where core cannot) and falling back to
// a local pass when the helper is unreachable.
func (c *firewallClient) verifyPosture(facts model.PostureFacts) verify.Report {
	payload, err := json.Marshal(facts)
	if err != nil {
		return verify.Report{}
	}
	resp, err := c.request(ipc.Message{Type: ipc.TypeFirewallVerify, ID: "verify", Payload: payload})
	if err == nil {
		var rep verify.Report
		if jsonErr := json.Unmarshal(resp.Payload, &rep); jsonErr == nil {
			c.noteUp()
			c.mu.Lock()
			c.report = rep
			c.mu.Unlock()
			return rep
		}
	}
	c.noteDown()

	// Fallback: measure locally. On an appliance core cannot read the ruleset
	// (that is the helper's privilege), and NFTRulesetReader then returns an
	// error — which verify treats as "cannot confirm", a loud failure, never a
	// pass (D17). That is the correct answer when the helper is gone.
	rep := verify.Run(verify.Config{
		Paths:                    c.localPaths,
		Ruleset:                  c.localRuleset,
		Profile:                  facts.Profile,
		TheaterIface:             facts.TheaterIface,
		ClientIface:              facts.ClientIface,
		TheaterAllowlist:         facts.TheaterAllowlist,
		AdminReachableFromPatron: facts.AdminReachableFromPatron,
		DefaultCredentials:       facts.DefaultCredentials,
		SSHEnabledOutsideSupport: facts.SSHEnabledOutsideSupport,
		// SSH is measurable without privilege, so the fallback pass answers this
		// row exactly as well as the helper's does.
		SSH: verify.ProcSSHReader(verify.DefaultSSHPort),
	})
	c.mu.Lock()
	c.report = rep
	c.mu.Unlock()
	return rep
}

func (c *firewallClient) request(m ipc.Message) (ipc.Message, error) {
	cli, err := ipc.Dial(c.sockPath)
	if err != nil {
		return ipc.Message{}, err
	}
	defer cli.Close()
	return cli.Request(m)
}

func (c *firewallClient) noteUp() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive, c.lastSeen, c.detail = true, &now, ""
}

func (c *firewallClient) noteDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		c.log.Warn(logging.EvProcessUnhealthy,
			logging.Field("process", model.ProcessFirewall),
			logging.Field("reason", "loopback_request_failed"))
	}
	// Forget the applied configuration: a helper that restarted has no idea what
	// it was asked to install.
	c.alive, c.appliedKey, c.detail = false, "", "request_failed"
}

// warnings reports the firewall-side conditions core observed directly, as
// opposed to those the verification pass measures for itself.
func (c *firewallClient) warnings() []model.Warning {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applyErr == "" {
		return nil
	}
	return []model.Warning{{
		Code:     model.WarnFirewallApplyRefused,
		Message:  "The firewall helper refused this profile's ruleset, so the loaded rules are not the configured ones: " + c.applyErr,
		Severity: model.SeverityCritical,
	}}
}

func (c *firewallClient) lastReport() verify.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report
}

func (c *firewallClient) liveness() model.ProcessLiveness {
	c.mu.Lock()
	defer c.mu.Unlock()
	return model.ProcessLiveness{
		Name: model.ProcessFirewall, Alive: c.alive, LastSeen: c.lastSeen, Detail: c.detail,
	}
}

// version returns the §10.8 firewall_profile_version — the profile plus a digest
// of the ruleset actually installed, so a tech can tell two revisions of the same
// profile apart. Empty until a ruleset has been accepted.
func (c *firewallClient) version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.applied {
		return ""
	}
	return c.rulesetVersion
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
