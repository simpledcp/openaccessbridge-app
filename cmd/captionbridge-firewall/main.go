// Command captionbridge-firewall is the privileged network helper (§3.5). It is
// the ONLY component that needs root, kept small and auditable: it generates and
// applies the nftables ruleset and verifies forwarding/isolation by
// self-introspection, answering core over a loopback Unix socket.
//
// SAFETY: applying rules / toggling sysctls is gated behind BOTH running as root
// AND an explicit --allow-apply flag, so the helper never mutates a developer
// host's live firewall by accident. Without it, apply is a dry-run that returns
// the generated ruleset for inspection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	allowApply := flag.Bool("allow-apply", false, "actually apply nftables rules + sysctls (requires root); off = dry-run")
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentFirewall, JSON: true})
	log.Info(logging.EvProcessStarted,
		logging.Field("version", buildinfo.Version),
		logging.Field("root", os.Geteuid() == 0),
		logging.Field("apply_enabled", *allowApply && os.Geteuid() == 0))

	h := &helper{log: log, allowApply: *allowApply && os.Geteuid() == 0}

	sockPath := filepath.Join(*sockDir, ipc.SocketFirewall)
	srv, err := ipc.Listen(sockPath, h.handle)
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
}

// verifyFacts is the caller-supplied half of a verification pass (§9.3): facts
// about the appliance's own firewall/admin/cred/ssh state that the kernel does not
// expose.
type verifyFacts struct {
	Profile                  model.FirewallProfile `json:"profile"`
	FirewallActive           bool                  `json:"firewall_active"`
	AdminReachableFromPatron bool                  `json:"admin_reachable_from_patron"`
	DefaultCredentials       bool                  `json:"default_credentials"`
	SSHEnabledOutsideSupport bool                  `json:"ssh_enabled_outside_support"`
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
		if err := applyRuleset(script); err != nil {
			h.log.Error(logging.EvVerificationFailed, logging.Field("error", err.Error()))
			return errResp(m, "apply failed: "+err.Error())
		}
		if err := applySysctls(); err != nil {
			h.log.Error(logging.EvVerificationFailed, logging.Field("error", err.Error()))
			return errResp(m, "sysctl apply failed: "+err.Error())
		}
		applied = true
	}
	h.log.Info(logging.EvFirewallProfileApplied,
		logging.Field("profile", string(cfg.Profile)),
		logging.Field("initiation", string(cfg.Initiation)),
		logging.Field("applied", applied))
	payload, _ := json.Marshal(map[string]any{"applied": applied, "ruleset": script})
	return &ipc.Message{Type: ipc.TypeFirewallResult, ID: m.ID, Payload: payload}
}

func (h *helper) verify(m ipc.Message) *ipc.Message {
	var f verifyFacts
	if len(m.Payload) > 0 {
		_ = json.Unmarshal(m.Payload, &f)
	}
	rep := verify.Run(verify.Config{
		Paths:                    verify.DefaultPaths(),
		Profile:                  f.Profile,
		FirewallActive:           f.FirewallActive,
		AdminReachableFromPatron: f.AdminReachableFromPatron,
		DefaultCredentials:       f.DefaultCredentials,
		SSHEnabledOutsideSupport: f.SSHEnabledOutsideSupport,
	})
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

func (h *helper) runtimeVerifyLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rep := verify.Run(verify.Config{Paths: verify.DefaultPaths(), FirewallActive: h.allowApply})
			if !rep.ForwardingDisabledV4 || !rep.ForwardingDisabledV6 {
				h.log.Warn(logging.EvForwardingEnabledDetected, logging.Field("source", "runtime_watchdog"))
			}
		}
	}
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
