package invariants

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/readiness"
	"github.com/simpledcp/openaccessbridge/internal/security/firewall"
	"github.com/simpledcp/openaccessbridge/internal/security/verify"
)

// repoRoot resolves the repository root relative to this test's package dir.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// Guardrail 2: no IP forwarding; default-deny firewall; NO forward-chain accepts.
func TestInvariant_NoForwardingDefaultDeny(t *testing.T) {
	// Sysctls disable BOTH v4 and v6 forwarding.
	s := firewall.SysctlSettings()
	if s["net.ipv4.ip_forward"] != "0" || s["net.ipv6.conf.all.forwarding"] != "0" {
		t.Fatalf("forwarding sysctls must both be 0: %v", s)
	}

	// Every profile + both initiation variants produce a default-deny ruleset whose
	// forward chain has policy drop and ZERO accept rules.
	profiles := []model.FirewallProfile{
		model.ProfileLab, model.ProfileBoothDiagnostic, model.ProfilePatronFacing, model.ProfileLowCostReplacement,
	}
	for _, p := range profiles {
		for _, init := range []model.ACSPInitiation{model.ACSInitiated, model.ServerInitiated} {
			cfg := firewall.Config{
				Profile: p, TheaterIface: "eth0", ClientIface: "wlan0",
				CinemaServerIP: "10.10.20.15", ACSPPort: 4170, LocalACSPPort: 4170,
				Initiation: init, WebRTCPortRange: "10000-20000", PatronWebPorts: []int{80, 443},
			}
			script, err := firewall.Generate(cfg)
			if err != nil {
				t.Fatalf("Generate(%s,%s): %v", p, init, err)
			}
			fwd := forwardChain(script)
			if !strings.Contains(fwd, "policy drop") {
				t.Errorf("%s/%s: forward chain not default-deny:\n%s", p, init, fwd)
			}
			// Check for accept RULES, ignoring any "# NEVER accept" comment text.
			for _, line := range strings.Split(fwd, "\n") {
				code := line
				if i := strings.IndexByte(code, '#'); i >= 0 {
					code = code[:i]
				}
				if strings.Contains(code, "accept") {
					t.Errorf("%s/%s: forward chain must have NO accept rules: %q", p, init, line)
				}
			}
		}
	}
}

// forwardChain extracts the `chain forward { ... }` block from an nft script.
func forwardChain(script string) string {
	i := strings.Index(script, "chain forward")
	if i < 0 {
		return ""
	}
	rest := script[i:]
	depth := 0
	start := strings.IndexByte(rest, '{')
	if start < 0 {
		return rest
	}
	for j := start; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:j+1]
			}
		}
	}
	return rest
}

// Guardrail 2 + §9.3: verify treats enabled forwarding as a CRITICAL failure.
func TestInvariant_ForwardingEnabledIsCritical(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	sys := filepath.Join(dir, "sys")
	mkdir(t, filepath.Join(proc, "ipv4"))
	mkdir(t, filepath.Join(proc, "ipv6", "conf", "all"))
	mkdir(t, sys)
	write(t, filepath.Join(proc, "ipv4", "ip_forward"), "1") // forwarding ON
	write(t, filepath.Join(proc, "ipv6", "conf", "all", "forwarding"), "0")

	rep := verify.Run(verify.Config{
		Paths:          verify.Paths{ProcSysNet: proc, SysClassNet: sys},
		Profile:        model.ProfilePatronFacing,
		FirewallActive: true,
	})
	if rep.Pass {
		t.Fatalf("forwarding enabled must fail verification")
	}
	if !hasCriticalCode(rep.Warnings, model.WarnForwardingEnabled) {
		t.Fatalf("expected a critical forwarding warning, got %v", rep.Warnings)
	}
}

// Guardrail 4: caption and audio readiness are INDEPENDENT.
func TestInvariant_IndependentReadiness(t *testing.T) {
	clean := verify.Report{ForwardingDisabledV4: true, ForwardingDisabledV6: true, FirewallActive: true, AdminIsolated: true, NoDefaultCreds: true, SSHCorrect: true, Pass: true}
	// Captions ready, audio dead.
	r1 := readiness.Assemble(readiness.Inputs{
		Link:   model.CaptionLinkState{State: model.StatePlaying, LeaseActive: true, RPLLoaded: true},
		Audio:  []model.AudioStreamState{{StreamType: model.StreamVIN, Status: model.QualFailed, Silence: true, DeviceConnected: true, Reasons: []string{"silent during playback"}}},
		Verify: clean})
	if !r1.CaptionReady || r1.AudioReady {
		t.Fatalf("expected captions ready + audio not ready, got caption=%v audio=%v", r1.CaptionReady, r1.AudioReady)
	}
	// Audio ready, captions down.
	r2 := readiness.Assemble(readiness.Inputs{
		Link:   model.CaptionLinkState{State: model.StateDisconnected},
		Audio:  []model.AudioStreamState{{StreamType: model.StreamHI, Status: model.QualExcellent, DeviceConnected: true}},
		Verify: clean})
	if r2.CaptionReady || !r2.AudioReady {
		t.Fatalf("expected audio ready + captions not ready, got caption=%v audio=%v", r2.CaptionReady, r2.AudioReady)
	}
}

// Guardrail 7: no silent degradation — a failed feed surfaces a CRITICAL warning.
func TestInvariant_NoSilentDegradation(t *testing.T) {
	clean := verify.Report{ForwardingDisabledV4: true, ForwardingDisabledV6: true, FirewallActive: true, AdminIsolated: true, NoDefaultCreds: true, SSHCorrect: true, Pass: true}
	r := readiness.Assemble(readiness.Inputs{
		Link:   model.CaptionLinkState{State: model.StatePlaying, LeaseActive: true, RPLLoaded: true},
		Audio:  []model.AudioStreamState{{StreamType: model.StreamHI, Status: model.QualFailed, Silence: true, DeviceConnected: true, Reasons: []string{"configured input is silent during playback"}}},
		Verify: clean})
	if !r.HasCritical() {
		t.Fatalf("a silent HI feed during playback must raise a critical warning, got %v", r.Warnings)
	}
}

// Guardrail 6: nothing sensitive in logs — caption text is redacted.
func TestInvariant_NoContentInLogs(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(logging.Options{Component: "core", Writer: &buf, JSON: true})
	secret := "A SECRET CAPTION LINE THAT MUST NOT APPEAR"
	l.Info(logging.EvCueVisibleNoText, logging.Field("cue_text", secret), logging.Field("text", secret))
	if strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("caption text leaked into logs: %s", buf.String())
	}
}

// Guardrail 5: the patron caption fan-out message carries the redaction-aware
// shape; the fan-out never exposes raw server traffic, only the minimal cue.
func TestInvariant_FanoutMinimalCue(t *testing.T) {
	h := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 1"})
	sub, _ := h.Subscribe("en")
	h.PublishCue("en", true, model.Cue{Text: "hello", ResourceID: "r1", StartMS: 1000})
	m := sub.Peek()
	// The message is a state snapshot (cue + status), not raw ST 430-10/-11 traffic.
	if m.Type != "state" || m.Language != "en" {
		t.Fatalf("unexpected fan-out message shape: %+v", m)
	}
}

// Guardrail 3 + §3.5: three supervised processes with privilege separation, and
// core Wants (not Requires) media so a media crash never drops captions.
func TestInvariant_ThreeProcessSeparation(t *testing.T) {
	root := repoRoot(t)
	for _, bin := range []string{"openaccessbridge-core", "openaccessbridge-media", "openaccessbridge-firewall"} {
		if _, err := os.Stat(filepath.Join(root, "cmd", bin, "main.go")); err != nil {
			t.Errorf("missing process %s: %v", bin, err)
		}
		if _, err := os.Stat(filepath.Join(root, "deploy", "systemd", bin+".service")); err != nil {
			t.Errorf("missing systemd unit for %s: %v", bin, err)
		}
	}
	core := readFile(t, filepath.Join(root, "deploy", "systemd", "openaccessbridge-core.service"))
	if !strings.Contains(core, "Wants=openaccessbridge-media.service") {
		t.Errorf("core must Want (not Require) media so a media crash can't drop captions")
	}
	fw := readFile(t, filepath.Join(root, "deploy", "systemd", "openaccessbridge-firewall.service"))
	if !strings.Contains(fw, "CAP_NET_ADMIN") {
		t.Errorf("firewall helper must hold CAP_NET_ADMIN (the only privileged component)")
	}
	// Core dumps disabled everywhere (a crash dump is how content leaks, §9.4).
	for _, unit := range []string{"openaccessbridge-core.service", "openaccessbridge-media.service"} {
		u := readFile(t, filepath.Join(root, "deploy", "systemd", unit))
		if !strings.Contains(u, "LimitCORE=0") {
			t.Errorf("%s must set LimitCORE=0 (no content-leaking core dumps)", unit)
		}
	}
}

// §3.5: privilege separation is only real if the loopback channel between the
// privileged helper and the unprivileged core actually WORKS as deployed. The
// firewall helper runs as root and core as the openaccessbridge user, so an
// owner-only socket would be unreachable and the dashboard would report a
// permanent "Firewall INACTIVE" — a separation that silently disables the
// security surface it exists to prove.
func TestInvariant_LoopbackSocketReachableAcrossPrivilege(t *testing.T) {
	root := repoRoot(t)
	units := []string{"openaccessbridge-core.service", "openaccessbridge-media.service", "openaccessbridge-firewall.service"}

	// All three share one tmpfs runtime dir, so it must survive any single unit
	// stopping and be group-traversable.
	for _, unit := range units {
		u := readFile(t, filepath.Join(root, "deploy", "systemd", unit))
		if !strings.Contains(u, "Group=openaccessbridge") {
			t.Errorf("%s must run with the shared Group=openaccessbridge so the loopback sockets are mutually reachable", unit)
		}
		if !strings.Contains(u, "RuntimeDirectoryPreserve=yes") {
			t.Errorf("%s must set RuntimeDirectoryPreserve=yes; otherwise stopping it deletes the runtime dir the other two are using", unit)
		}
		if strings.Contains(u, "RuntimeDirectoryMode=0700") {
			t.Errorf("%s uses RuntimeDirectoryMode=0700, which blocks the shared group from reaching the sockets", unit)
		}
	}

	// The firewall helper is the one that runs as root; it must NOT declare a
	// User= (that would drop the privileges it exists to hold) but must hand its
	// socket to the shared group.
	fw := readFile(t, filepath.Join(root, "deploy", "systemd", "openaccessbridge-firewall.service"))
	if strings.Contains(fw, "User=") {
		t.Error("firewall helper must run as root (no User=) — it is the only privileged component")
	}
	core := readFile(t, filepath.Join(root, "deploy", "systemd", "openaccessbridge-core.service"))
	if !strings.Contains(core, "User=openaccessbridge") {
		t.Error("core must run unprivileged (User=openaccessbridge)")
	}

	// And the helper must actually widen the socket rather than relying on the
	// owner-only default, while still attributing its callers.
	main := readFile(t, filepath.Join(root, "cmd", "openaccessbridge-firewall", "main.go"))
	if !strings.Contains(main, "ipc.ListenWithOptions") {
		t.Error("firewall helper must bind its socket with explicit group ownership (ipc.ListenWithOptions)")
	}
	if !strings.Contains(main, "AllowPeer") {
		t.Error("firewall helper must admit callers by peer credentials, not permissions alone")
	}
}

// Guardrail 5: no content persisted — the config schema has no content-bearing
// object, and the runtime dir is tmpfs in the unit files.
func TestInvariant_NoContentPersistence(t *testing.T) {
	root := repoRoot(t)
	for _, unit := range []string{"openaccessbridge-core.service", "openaccessbridge-media.service", "openaccessbridge-firewall.service"} {
		u := readFile(t, filepath.Join(root, "deploy", "systemd", unit))
		if !strings.Contains(u, "RuntimeDirectory=openaccessbridge") {
			t.Errorf("%s must use a tmpfs RuntimeDirectory for transient state", unit)
		}
	}
	// The config store's no-content-columns guard is asserted in internal/config;
	// here we assert the schema source itself names no content object.
	schema := readFile(t, filepath.Join(root, "internal", "config", "sqlite.go"))
	for _, forbidden := range []string{"cue_text", "caption_text TEXT", "audio_data", "transcript"} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("config schema must not contain a content object %q", forbidden)
		}
	}
}

// --- helpers ---

func hasCriticalCode(ws []model.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code && w.Severity == model.SeverityCritical {
			return true
		}
	}
	return false
}
func mkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
}
func write(t *testing.T, p, c string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
