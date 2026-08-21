// Command captionbridge-firewall is the privileged network helper (§3.5). It is
// the ONLY component that needs root, kept small and auditable: it generates and
// applies the nftables ruleset and verifies forwarding/isolation by
// self-introspection, answering core over a loopback Unix socket.
//
// SAFETY: applying rules / toggling sysctls is gated behind BOTH running as root
// AND an explicit --allow-apply flag, so the helper never mutates a developer
// host's live firewall by accident. Without it, apply is a dry-run that returns
// the generated ruleset for inspection. READING the loaded ruleset is not gated —
// BLOCKER B5 is about changing kernel state, not observing it — so the posture is
// measured on every pass either way (§9.3).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

// watchdogInterval is how often §3.3's runtime watchdog re-checks the posture.
const watchdogInterval = 30 * time.Second

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	allowApply := flag.Bool("allow-apply", false, "actually apply nftables rules + sysctls (requires root); off = dry-run")
	sockGroup := flag.String("socket-group", "captionbridge", "group allowed to reach this helper's socket (core runs unprivileged; empty = owner only)")
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentFirewall, JSON: true})
	log.Info(logging.EvProcessStarted,
		logging.Field("version", buildinfo.Version),
		logging.Field("root", os.Geteuid() == 0),
		logging.Field("apply_enabled", *allowApply && os.Geteuid() == 0))

	h := newHelper(log, *allowApply && os.Geteuid() == 0)

	// This helper runs as root while core runs unprivileged, so an owner-only
	// socket would be unreachable from core (§3.5). Hand the socket to the shared
	// group and admit only that group — permissions AND peer credentials, since
	// the privileged component must be able to attribute every command.
	opts := socketOptions(*sockGroup, log)
	sockPath := filepath.Join(*sockDir, ipc.SocketFirewall)
	srv, err := ipc.ListenWithOptions(sockPath, h.handle, opts)
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	defer srv.Close()

	// Runtime re-verification: §9.3 asserts the posture continuously, not once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runtimeVerifyLoop(ctx)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

type helper struct {
	log        *logging.Logger
	allowApply bool

	// paths, ruleset, applyRules and applySysctls are the injectable edges of the
	// helper: production wires them to the real kernel, the tests to fixtures, so
	// the watchdog's re-assert can be exercised without root (§9.3 "all
	// filesystem roots and the ruleset reader are injectable").
	paths        verify.Paths
	ruleset      verify.RulesetReader
	applyRules   func(script string) error
	applySysctls func() error

	mu sync.Mutex
	// lastScript is the ruleset last generated from core's configuration. The
	// watchdog re-applies THIS on a flip rather than re-deriving one, so a
	// re-assert can never install a posture core did not ask for (§3.3).
	lastScript  string
	lastProfile model.FirewallProfile
	// lastFacts are the caller-supplied facts from the most recent verify
	// request. Carrying them into the watchdog is what keeps the profile,
	// credential and SSH assertions from silently checking a zero value.
	lastFacts model.PostureFacts
}

func newHelper(log *logging.Logger, allowApply bool) *helper {
	return &helper{
		log:        log,
		allowApply: allowApply,
		paths:      verify.DefaultPaths(),
		// Read the LOADED ruleset on every pass: §9.3's posture is proven, never
		// asserted. Listing needs no ruleset change and no --allow-apply gate.
		ruleset:      verify.NFTRulesetReader(),
		applyRules:   applyRuleset,
		applySysctls: applySysctls,
	}
}

// socketOptions resolves the shared socket group. On the appliance the group
// exists (created by packaging) and core reaches the helper through it. On a dev
// host it usually does not — degrade to the owner-only posture and say so
// plainly rather than failing to start or silently widening access.
func socketOptions(group string, log *logging.Logger) ipc.ListenOptions {
	if group == "" {
		return ipc.ListenOptions{}
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		log.Warn(logging.EvStateTransition,
			logging.Field("socket_group", group),
			logging.Field("result", "not_found_owner_only"))
		return ipc.ListenOptions{}
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil || gid == 0 {
		log.Warn(logging.EvStateTransition,
			logging.Field("socket_group", group),
			logging.Field("result", "unusable_gid_owner_only"))
		return ipc.ListenOptions{}
	}
	return ipc.ListenOptions{
		GroupGID:  gid,
		Mode:      0o660,
		DirMode:   0o750,
		AllowPeer: ipc.AllowOwnerOrGroup(gid),
	}
}

func (h *helper) handle(m ipc.Message) *ipc.Message {
	switch m.Type {
	case ipc.TypePing:
		return &ipc.Message{Type: ipc.TypePong, ID: m.ID}
	case ipc.TypeFirewallApply:
		return h.apply(m)
	case ipc.TypeFirewallVerify:
		return h.verify(m)
	default:
		return &ipc.Message{Type: "unsupported", ID: m.ID}
	}
}

func (h *helper) apply(m ipc.Message) *ipc.Message {
	var cfg firewall.Config
	if err := json.Unmarshal(m.Payload, &cfg); err != nil {
		return errResp(m, "bad firewall config")
	}
	script, err := firewall.Generate(cfg)
	if err != nil {
		return errResp(m, err.Error())
	}
	applied := false
	if h.allowApply {
		if err := h.applyRules(script); err != nil {
			h.log.Error(logging.EvVerificationFailed, logging.Field("error", err.Error()))
			return errResp(m, "apply failed: "+err.Error())
		}
		if err := h.applySysctls(); err != nil {
			h.log.Error(logging.EvVerificationFailed, logging.Field("error", err.Error()))
			return errResp(m, "sysctl apply failed: "+err.Error())
		}
		applied = true
	}
	// Remember what core asked for even in dry-run: the watchdog re-asserts the
	// last requested posture, and a helper that is later granted apply rights
	// must not have forgotten it.
	h.mu.Lock()
	h.lastScript, h.lastProfile = script, cfg.Profile
	h.mu.Unlock()

	h.log.Info(logging.EvFirewallProfileApplied,
		logging.Field("profile", string(cfg.Profile)),
		logging.Field("initiation", string(cfg.Initiation)),
		logging.Field("applied", applied))
	payload, _ := json.Marshal(map[string]any{"applied": applied, "ruleset": script})
	return &ipc.Message{Type: ipc.TypeFirewallResult, ID: m.ID, Payload: payload}
}

func (h *helper) verify(m ipc.Message) *ipc.Message {
	var f model.PostureFacts
	if len(m.Payload) > 0 {
		_ = json.Unmarshal(m.Payload, &f)
	}
	h.mu.Lock()
	h.lastFacts = f
	h.mu.Unlock()

	rep := verify.Run(h.verifyConfig(f))
	if rep.Pass {
		h.log.Info(logging.EvVerificationPassed)
	} else {
		h.log.Warn(logging.EvVerificationFailed, logging.Field("warning_count", len(rep.Warnings)))
		if !rep.ForwardingDisabledV4 || !rep.ForwardingDisabledV6 {
			h.log.Warn(logging.EvForwardingEnabledDetected)
		}
	}
	payload, _ := json.Marshal(rep)
	return &ipc.Message{Type: ipc.TypeFirewallResult, ID: m.ID, Payload: payload}
}

// verifyConfig builds a verification pass from the caller-supplied facts. Both
// call sites — the request handler and the watchdog — go through here, so the
// watchdog can never run a weaker pass than the one core asked for.
func (h *helper) verifyConfig(f model.PostureFacts) verify.Config {
	return verify.Config{
		Paths:                    h.paths,
		Ruleset:                  h.ruleset,
		Profile:                  f.Profile,
		TheaterIface:             f.TheaterIface,
		ClientIface:              f.ClientIface,
		TheaterAllowlist:         f.TheaterAllowlist,
		AdminReachableFromPatron: f.AdminReachableFromPatron,
		DefaultCredentials:       f.DefaultCredentials,
		SSHEnabledOutsideSupport: f.SSHEnabledOutsideSupport,
		// §9.3's SSH row, MEASURED. Reading a listening socket needs no
		// privilege, so unlike the ruleset this is not the helper's special
		// ability — but it belongs in every pass, and the watchdog runs through
		// here too so it can never check less than core asked for.
		SSH: verify.ProcSSHReader(verify.DefaultSSHPort),
	}
}

func (h *helper) runtimeVerifyLoop(ctx context.Context) {
	t := time.NewTicker(watchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.watchdogPass()
		}
	}
}

// watchdogPass is one iteration of §3.3's runtime watchdog: "A runtime watchdog
// re-asserts these and the dashboard screams if any flip."
//
// Detecting the flip is only half of that sentence. When the helper is permitted
// to apply, it re-asserts the sysctls and the last requested ruleset and then
// re-measures; when it is not, it says so at error level rather than logging a
// warning that reads as if something was done about it. Either way the flip
// stands in the report core polls, so the dashboard screams.
func (h *helper) watchdogPass() verify.Report {
	h.mu.Lock()
	facts, script := h.lastFacts, h.lastScript
	h.mu.Unlock()

	rep := verify.Run(h.verifyConfig(facts))
	if !postureFlipped(rep) {
		return rep
	}
	if !rep.ForwardingDisabledV4 || !rep.ForwardingDisabledV6 {
		h.log.Warn(logging.EvForwardingEnabledDetected,
			logging.Field("source", "runtime_watchdog"),
			logging.Field("knobs", strings.Join(rep.ForwardingKnobsEnabled, ",")))
	}
	if !h.allowApply {
		// Loud, and honest about what did NOT happen: this build cannot restore
		// the posture, so somebody has to (guardrail #7).
		h.log.Error(logging.EvVerificationFailed,
			logging.Field("source", "runtime_watchdog"),
			logging.Field("reason", "posture_flipped_reassert_not_permitted"))
		return rep
	}

	reasserted := []string{}
	if err := h.applySysctls(); err != nil {
		h.log.Error(logging.EvVerificationFailed,
			logging.Field("source", "runtime_watchdog"),
			logging.Field("error", err.Error()))
	} else {
		reasserted = append(reasserted, "sysctls")
	}
	if script != "" && (!rep.FirewallActive || rep.ForwardAccepts > 0) {
		if err := h.applyRules(script); err != nil {
			h.log.Error(logging.EvVerificationFailed,
				logging.Field("source", "runtime_watchdog"),
				logging.Field("error", err.Error()))
		} else {
			reasserted = append(reasserted, "ruleset")
		}
	}
	h.log.Warn(logging.EvFirewallProfileApplied,
		logging.Field("source", "runtime_watchdog"),
		logging.Field("profile", string(h.profile())),
		logging.Field("reasserted", strings.Join(reasserted, ",")))

	// Re-measure: a re-assert that did not take is a worse failure than one that
	// was never attempted, and must not be reported as a fix.
	after := verify.Run(h.verifyConfig(facts))
	if postureFlipped(after) {
		h.log.Error(logging.EvVerificationFailed,
			logging.Field("source", "runtime_watchdog"),
			logging.Field("reason", "reassert_did_not_restore_posture"))
	} else {
		h.log.Info(logging.EvVerificationPassed, logging.Field("source", "runtime_watchdog"))
	}
	return after
}

func (h *helper) profile() model.FirewallProfile {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastProfile != "" {
		return h.lastProfile
	}
	return h.lastFacts.Profile
}

// postureFlipped reports whether the kernel-level guarantees this helper exists
// to hold have moved: forwarding back on, a forward-chain accept, or the
// default-deny table gone (§3.3, §9.1).
func postureFlipped(rep verify.Report) bool {
	return !rep.ForwardingDisabledV4 || !rep.ForwardingDisabledV6 ||
		rep.ForwardAccepts > 0 || !rep.FirewallActive
}

// applyRuleset pipes the generated nft script into `nft -f -`.
func applyRuleset(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// applySysctls disables IPv4 + IPv6 forwarding (guardrail #2).
func applySysctls() error {
	for k, v := range firewall.SysctlSettings() {
		path := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
		if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func errResp(m ipc.Message, reason string) *ipc.Message {
	payload, _ := json.Marshal(map[string]string{"error": reason})
	return &ipc.Message{Type: ipc.TypeFirewallResult, ID: m.ID, Payload: payload}
}
