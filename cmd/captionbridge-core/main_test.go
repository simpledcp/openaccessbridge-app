package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/deploy"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/readiness"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
	"github.com/simpledcp/captionbridge/internal/sim"
)

// --- fixtures -------------------------------------------------------------

// fakeHelper is an in-process stand-in for one of the supervised helpers,
// listening on a real loopback socket so core's client half is exercised over
// the actual IPC seam rather than a mock (§3.5).
type fakeHelper struct {
	mu       sync.Mutex
	received []ipc.Message
	srv      *ipc.Server
}

func startFakeHelper(t *testing.T, dir, name string, h func(ipc.Message) *ipc.Message) *fakeHelper {
	t.Helper()
	f := &fakeHelper{}
	srv, err := ipc.Listen(filepath.Join(dir, name), func(m ipc.Message) *ipc.Message {
		f.mu.Lock()
		f.received = append(f.received, m)
		f.mu.Unlock()
		return h(m)
	})
	if err != nil {
		t.Fatalf("fake helper %s: %v", name, err)
	}
	f.srv = srv
	t.Cleanup(func() { srv.Close() })
	return f
}

func (f *fakeHelper) payloadFor(typ string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.received {
		if m.Type == typ {
			return m.Payload
		}
	}
	return nil
}

func (f *fakeHelper) sawType(typ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.received {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// cleanReport is a passing §9.3 verification report as a helper would return it.
func cleanReport() verify.Report {
	return verify.Report{
		ForwardingDisabledV4: true, ForwardingDisabledV6: true, BridgesConfirmed: true,
		FirewallActive: true, FirewallIntrospected: true, AdminIsolated: true,
		NoDefaultCreds: true, SSHCorrect: true, Pass: true,
	}
}

func reply(m ipc.Message, v any) *ipc.Message {
	payload, _ := json.Marshal(v)
	return &ipc.Message{Type: ipc.TypeFirewallResult, ID: m.ID, Payload: payload}
}

// testStore opens a temp config store with an auditorium profile.
func testStore(t *testing.T, prof model.AuditoriumProfile) *config.SQLiteStore {
	t.Helper()
	store, err := config.OpenSQLite(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if prof.AuditoriumID != "" {
		if err := store.PutAuditoriumProfile(prof); err != nil {
			t.Fatalf("PutAuditoriumProfile: %v", err)
		}
	}
	_ = store.PutSetting(config.SettingTheaterInterface, "eth0")
	_ = store.PutSetting(config.SettingClientInterface, "wlan0")
	return store
}

func newSupervisor(t *testing.T, store *config.SQLiteStore, dir string) *supervisor {
	t.Helper()
	sup, _ := newSupervisorWithFetcher(t, store, dir)
	return sup
}

// newSupervisorWithFetcher also hands back the content fetcher, so a test can
// serve a caption asset to the real ACSP machine.
func newSupervisorWithFetcher(t *testing.T, store *config.SQLiteStore, dir string) (*supervisor, *resolver.MemoryFetcher) {
	t.Helper()
	log := logging.Nop()
	hub := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 3", MaxClients: 25})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	t.Cleanup(ctrl.Close)

	cw := readiness.NewCaptionWarnings(0)
	fetcher := &resolver.MemoryFetcher{Files: map[string][]byte{}}
	res := resolver.NewPluggable(fetcher, nil, log)
	loader := newCaptionLoader(resolver.NewPipeline(res, resolver.NewCache(1<<20), log), 1, cw)
	aud, _ := auditoriumProfile(store)
	opts := captionMachineOptions(store, log, aud, cw)
	opts.Controller, opts.Resolver, opts.Loader = ctrl, res, loader

	return &supervisor{
		store: store, log: log, profile: model.ProfileBoothDiagnostic,
		deployed:   mustProfile(model.ProfileBoothDiagnostic),
		patronBind: "0.0.0.0:8080", adminBind: "127.0.0.1:8090",
		media:    newMediaClient(filepath.Join(dir, ipc.SocketMedia), log),
		firewall: newFirewallClient(filepath.Join(dir, ipc.SocketFirewall), log),
		machine:  acsp.New(opts), hub: hub, ctrl: ctrl, captionWarnings: cw,
	}, fetcher
}

func mustProfile(name model.FirewallProfile) deploy.Profile {
	p, _ := deploy.Resolve(name)
	return p
}

func hasCode(ws []model.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func codes(ws []model.Warning) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Code
	}
	return out
}

// --- tests ----------------------------------------------------------------

// TestFirewallPostureIsMeasuredNotPinged: the dashboard's firewall verdict must
// come from a verification pass, not from "a helper process answered". A helper
// that is up and reports an INACTIVE firewall must read as inactive (§9.3).
func TestFirewallPostureIsMeasuredNotPinged(t *testing.T) {
	dir := t.TempDir()
	rep := cleanReport()
	rep.FirewallActive, rep.Pass = false, false
	rep.Warnings = []model.Warning{{Code: model.WarnFirewallInactive, Severity: model.SeverityCritical}}
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message {
		if m.Type == ipc.TypePing {
			return &ipc.Message{Type: ipc.TypePong, ID: m.ID}
		}
		return reply(m, rep)
	})
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15"})
	sup := newSupervisor(t, store, dir)

	sup.reconcile(context.Background())
	st := sup.status()
	if st.FirewallActive {
		t.Errorf("a reachable helper reporting an inactive firewall must not read as active")
	}
	if !st.FirewallIntrospected {
		t.Errorf("the pass was an introspection; the dashboard should say so")
	}
	if !hasCode(st.Warnings, model.WarnFirewallInactive) {
		t.Errorf("expected the helper's warning to reach the dashboard, got %v", codes(st.Warnings))
	}
}

// TestFirewallIsAppliedThroughTheHelper: §9.1's ruleset is generated from stored
// configuration and INSTALLED through the privileged helper — the generated rules
// are not documentation.
func TestFirewallIsAppliedThroughTheHelper(t *testing.T) {
	dir := t.TempDir()
	fw := startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message {
		if m.Type == ipc.TypeFirewallApply {
			return reply(m, map[string]any{"applied": true, "ruleset": "table inet captionbridge {}"})
		}
		return reply(m, cleanReport())
	})
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15, 10.10.20.16", ACSPPort: 4170,
	})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	payload := fw.payloadFor(ipc.TypeFirewallApply)
	if payload == nil {
		t.Fatalf("core never asked the helper to apply a ruleset")
	}
	var cfg firewall.Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatalf("apply payload: %v", err)
	}
	if cfg.Profile != model.ProfileBoothDiagnostic {
		t.Errorf("profile = %q, want the active one", cfg.Profile)
	}
	if len(cfg.CinemaServerIPs) != 2 {
		t.Errorf("the stored allowlist must reach the ruleset, got %v", cfg.CinemaServerIPs)
	}
	if cfg.TheaterIface != "eth0" || cfg.ClientIface != "wlan0" {
		t.Errorf("the configured interfaces must reach the ruleset, got %q/%q", cfg.TheaterIface, cfg.ClientIface)
	}

	// §10.8: the installed ruleset gets a recorded revision.
	v, _ := store.GetSystemConfig()
	if v.FirewallProfileVersion == "" {
		t.Errorf("an applied ruleset must record a firewall_profile_version (§10.8)")
	}
}

// TestIncompleteConfigurationIsNotApplied: a fresh appliance has no allowlist;
// core must not send a half-formed ruleset.
func TestIncompleteConfigurationIsNotApplied(t *testing.T) {
	dir := t.TempDir()
	fw := startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message {
		return reply(m, cleanReport())
	})
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"}) // no server IP
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	if fw.sawType(ipc.TypeFirewallApply) {
		t.Errorf("an incomplete configuration must not be applied")
	}
}

// TestMediaGetsTheStoredInputProfile: §10.5's input_device_id and channel_mapping
// are configuration the media process consumes; core is what delivers them.
func TestMediaGetsTheStoredInputProfile(t *testing.T) {
	dir := t.TempDir()
	media := startFakeHelper(t, dir, ipc.SocketMedia, func(m ipc.Message) *ipc.Message {
		switch m.Type {
		case ipc.TypeMediaConfigure:
			payload, _ := json.Marshal(model.MediaCommandResult{OK: true})
			return &ipc.Message{Type: m.Type, ID: m.ID, Payload: payload}
		default:
			payload, _ := json.Marshal([]model.AudioStreamState{})
			return &ipc.Message{Type: ipc.TypeMediaStatus, ID: m.ID, Payload: payload}
		}
	})
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message { return reply(m, cleanReport()) })

	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", InputDeviceID: "usb-XYZ-interface-00",
		ChannelMapping: "stereo:L=HI,R=VI-N",
	})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	payload := media.payloadFor(ipc.TypeMediaConfigure)
	if payload == nil {
		t.Fatalf("core never delivered the auditorium input profile to the media process")
	}
	var got model.AuditoriumProfile
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("configure payload: %v", err)
	}
	if got.InputDeviceID != "usb-XYZ-interface-00" || got.ChannelMapping != "stereo:L=HI,R=VI-N" {
		t.Errorf("the delivered profile is not the stored one: %+v", got)
	}
}

// TestMediaRefusalIsLoud: if the media process will not run the configured input,
// the booth has to be told — silence here means "audio looks fine" while the
// configured device is not the one running (guardrail #7).
func TestMediaRefusalIsLoud(t *testing.T) {
	dir := t.TempDir()
	startFakeHelper(t, dir, ipc.SocketMedia, func(m ipc.Message) *ipc.Message {
		if m.Type == ipc.TypeMediaConfigure {
			payload, _ := json.Marshal(model.MediaCommandResult{Error: "no capture backend (B3)"})
			return &ipc.Message{Type: m.Type, ID: m.ID, Payload: payload}
		}
		payload, _ := json.Marshal([]model.AudioStreamState{})
		return &ipc.Message{Type: ipc.TypeMediaStatus, ID: m.ID, Payload: payload}
	})
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message { return reply(m, cleanReport()) })
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", ChannelMapping: "mono:HI"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	st := sup.status()
	if !hasCode(st.Warnings, model.WarnAudioInputProfileRejected) {
		t.Errorf("a refused input profile must be a loud warning, got %v", codes(st.Warnings))
	}
}

// TestDeadHelpersAreNamed: §3.5 — core reports each supervised process's
// liveness. A crashed media helper must be distinguishable from silent inputs.
func TestDeadHelpersAreNamed(t *testing.T) {
	dir := t.TempDir() // nothing listening
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	st := sup.status()
	byName := map[string]model.ProcessLiveness{}
	for _, p := range st.Processes {
		byName[p.Name] = p
	}
	if len(byName) != 3 {
		t.Fatalf("all three supervised processes must be reported, got %v", st.Processes)
	}
	if !byName[model.ProcessCore].Alive {
		t.Errorf("core is answering the request; it is alive by construction")
	}
	if byName[model.ProcessMedia].Alive || byName[model.ProcessFirewall].Alive {
		t.Errorf("unreachable helpers must not be reported alive: %+v", st.Processes)
	}
	if byName[model.ProcessMedia].LastSeen != nil {
		t.Errorf("a helper that never answered has no last-seen time")
	}
	if !hasCode(st.Warnings, model.WarnProcessUnreachable) {
		t.Errorf("expected a process-unreachable warning, got %v", codes(st.Warnings))
	}
	// And the firewall claim falls back to a local pass, which cannot read the
	// ruleset in this environment: unproven, therefore not "active".
	if st.FirewallActive {
		t.Errorf("with no helper and no readable ruleset, the firewall must not read as active")
	}
}

// TestWrongRoomGuardIsArmedFromTheStoredProfile: §8.5's pinned identity has to
// come from the auditorium profile and its mismatch has to reach the dashboard.
// Both halves were missing: no fingerprint was passed and no warning channel
// existed.
func TestWrongRoomGuardIsArmedFromTheStoredProfile(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15",
		ExpectedServerFingerprint: "pinned-server-id", IdentityStrength: model.IdentityComposite,
	})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	// A server that is NOT the pinned one announces itself.
	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("some-other-server", "Auditorium 9", "different-fingerprint"))

	st := sup.status()
	if !hasCode(st.Warnings, model.WarnServerIdentityMismatch) {
		t.Fatalf("a wrong-room mismatch must reach the dashboard, got %v", codes(st.Warnings))
	}
	if st.AllowedServer != "10.10.20.15" {
		t.Errorf("the dashboard's allowed server must be the CONFIGURED one, got %q", st.AllowedServer)
	}
	if st.ConnectedServer != "some-other-server" {
		t.Errorf("the connected server is reported separately, got %q", st.ConnectedServer)
	}
	if st.IdentityStrength != model.IdentityComposite {
		t.Errorf("§10.5 requires the identity strength to be labelled, got %q", st.IdentityStrength)
	}
}

// TestMatchingServerRaisesNothing: the guard must not cry wolf on the right room.
func TestMatchingServerRaisesNothing(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", ExpectedServerFingerprint: "pinned-server-id",
	})
	sup := newSupervisor(t, store, dir)
	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("the-right-server", "Auditorium 3", "pinned-server-id"))
	if st := sup.status(); hasCode(st.Warnings, model.WarnServerIdentityMismatch) {
		t.Errorf("the pinned server must not raise a mismatch, got %v", codes(st.Warnings))
	}
}

// TestCaptionLeaseWarningReachesTheDashboardAndClears: the §5.2 lease warning is
// raised by the machine through OnWarning and cleared by a renewal — the whole
// point of wiring it to a SET rather than a log line.
func TestCaptionLeaseWarningReachesTheDashboardAndClears(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)

	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("srv", "Auditorium 3", ""))
	sup.machine.Handle(sim.GetNewLease(200)) // 200 ms lease
	time.Sleep(170 * time.Millisecond)       // inside the 25% warn fraction
	sup.machine.Tick()

	if st := sup.status(); !hasCode(st.Warnings, model.WarnCaptionLeaseNearExpiry) {
		t.Fatalf("a lease close to expiry must reach the dashboard, got %v", codes(st.Warnings))
	}
	sup.machine.Handle(sim.Status(30000)) // the server renews
	if st := sup.status(); hasCode(st.Warnings, model.WarnCaptionLeaseNearExpiry) {
		t.Errorf("a renewed lease must clear the warning, got %v", codes(st.Warnings))
	}
}

// TestCaptionSettingsAreConfiguration: the lease warn fraction and fetch timeout
// come from configuration; their defaults must not be hardcoded by omission.
func TestCaptionSettingsAreConfiguration(t *testing.T) {
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", ExpectedServerFingerprint: "fp"})
	_ = store.PutSetting(config.SettingLeaseWarnFraction, "0.5")
	_ = store.PutSetting(config.SettingAssetFetchTimeoutMS, "1500")

	opts := captionMachineOptions(store, logging.Nop(), mustAud(t, store), readiness.NewCaptionWarnings(0))
	if opts.WarnFraction != 0.5 {
		t.Errorf("WarnFraction = %v, want the configured 0.5", opts.WarnFraction)
	}
	if opts.FetchTimeout != 1500*time.Millisecond {
		t.Errorf("FetchTimeout = %v, want the configured 1.5s", opts.FetchTimeout)
	}
	if opts.ExpectedFingerprint != "fp" {
		t.Errorf("the pinned identity must come from the profile, got %q", opts.ExpectedFingerprint)
	}
	if opts.OnWarning == nil || opts.OnState == nil {
		t.Errorf("the caption warning channel must be wired")
	}

	// An out-of-range value falls back to the default rather than arming a
	// nonsense threshold.
	_ = store.PutSetting(config.SettingLeaseWarnFraction, "17")
	if got := leaseWarnFraction(store, logging.Nop()); got != readiness.DefaultLeaseWarnFraction {
		t.Errorf("an out-of-range fraction must fall back, got %v", got)
	}
}

// TestReadinessRecordComesFromARealPass: §10.8's last-good / last-failed fields
// must record a check that ACTUALLY RAN, with a redacted, code-only reason.
func TestReadinessRecordComesFromARealPass(t *testing.T) {
	dir := t.TempDir()
	rep := cleanReport()
	rep.ForwardingDisabledV4, rep.Pass = false, false
	rep.Warnings = []model.Warning{{Code: model.WarnForwardingEnabled, Severity: model.SeverityCritical, Message: "IPv4 forwarding is enabled"}}
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message { return reply(m, rep) })

	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	v, err := store.GetSystemConfig()
	if err != nil {
		t.Fatalf("GetSystemConfig: %v", err)
	}
	if v.LastReadinessCheckFail == nil {
		t.Fatalf("a failing pass must be recorded (§10.8)")
	}
	if v.LastReadinessCheckOK != nil {
		t.Errorf("nothing passed; last_readiness_check_ok must not be stamped")
	}
	reason := v.LastReadinessCheckFail.Reason
	if !strings.Contains(reason, model.WarnForwardingEnabled) {
		t.Errorf("the reason must name the failing condition, got %q", reason)
	}
	if strings.Contains(reason, " ") {
		// Codes only: the operator-facing prose is not persisted (§9.4).
		t.Errorf("the reason must be §8.3 codes, not prose, got %q", reason)
	}
	// The record reaches the dashboard (§10.8 "so a tech can see … before
	// changing anything").
	if st := sup.status(); st.Versions.LastReadinessCheckFail == nil {
		t.Errorf("the §10.8 record must be surfaced on the dashboard")
	}
}

// TestPassingCheckStampsTheGoodTimestamp.
func TestPassingCheckStampsTheGoodTimestamp(t *testing.T) {
	dir := t.TempDir()
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message { return reply(m, cleanReport()) })
	startFakeHelper(t, dir, ipc.SocketMedia, func(m ipc.Message) *ipc.Message {
		payload, _ := json.Marshal([]model.AudioStreamState{{
			StreamType: model.StreamHI, Status: model.QualUsable, DeviceConnected: true,
			InputDeviceID: "usb-XYZ-interface-00", EncoderState: model.EncoderStateReadyNoTransport,
		}})
		return &ipc.Message{Type: ipc.TypeMediaStatus, ID: m.ID, Payload: payload}
	})
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	sup.reconcile(context.Background())

	v, _ := store.GetSystemConfig()
	if v.LastReadinessCheckOK == nil {
		t.Fatalf("a clean pass must stamp last_readiness_check_ok")
	}
	if v.LastReadinessCheckFail != nil {
		t.Errorf("nothing failed; no failure should be recorded")
	}
}

// TestLastQualificationIsWrittenBack: §10.5's last_qualification was never
// written by anything. It is the WORST of the streams — a healthy HI feed does
// not excuse a failed VI-N one.
func TestLastQualificationIsWrittenBack(t *testing.T) {
	dir := t.TempDir()
	startFakeHelper(t, dir, ipc.SocketFirewall, func(m ipc.Message) *ipc.Message { return reply(m, cleanReport()) })
	startFakeHelper(t, dir, ipc.SocketMedia, func(m ipc.Message) *ipc.Message {
		if m.Type == ipc.TypeMediaConfigure {
			payload, _ := json.Marshal(model.MediaCommandResult{OK: true})
			return &ipc.Message{Type: m.Type, ID: m.ID, Payload: payload}
		}
		payload, _ := json.Marshal([]model.AudioStreamState{
			{StreamType: model.StreamHI, Status: model.QualExcellent, InputDeviceID: "usb-XYZ-interface-00", DeviceConnected: true},
			{StreamType: model.StreamVIN, Status: model.QualMarginal, InputDeviceID: "usb-XYZ-interface-00", DeviceConnected: true},
		})
		return &ipc.Message{Type: ipc.TypeMediaStatus, ID: m.ID, Payload: payload}
	})
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID: "screen-3", InputDeviceID: "usb-XYZ-interface-00", ChannelMapping: "stereo:L=HI,R=VI-N",
	})
	sup := newSupervisor(t, store, dir)

	// The pass that REBUILDS the media plan records nothing: whatever the streams
	// report a moment after being restarted describes the restart, not the input.
	sup.reconcile(context.Background())
	if p, _, _ := store.GetAuditoriumProfile("screen-3"); p.LastQualification != "" {
		t.Errorf("a verdict from the reconfiguration itself must not be persisted, got %q", p.LastQualification)
	}

	// The next pass reads a settled verdict and records it.
	sup.reconcile(context.Background())
	got, ok, _ := store.GetAuditoriumProfile("screen-3")
	if !ok {
		t.Fatal("profile disappeared")
	}
	if got.LastQualification != model.QualMarginal {
		t.Errorf("last_qualification = %q, want the worst stream's verdict", got.LastQualification)
	}
	v, _ := store.GetSystemConfig()
	if v.AudioDeviceCompat != "usb-XYZ-interface-00" {
		t.Errorf("§10.8 audio_device_compat_profile = %q, want the device in play", v.AudioDeviceCompat)
	}
}

// TestClientCapReachesTheDashboard: D11's cap is only enforced if the fan-out has
// it, and only visible if the dashboard reports it (§8.2).
func TestClientCapReachesTheDashboard(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)
	st := sup.status()
	if st.MaxConcurrentClients != mustProfile(model.ProfileBoothDiagnostic).MaxConcurrentClients {
		t.Errorf("max clients = %d, want the profile's cap", st.MaxConcurrentClients)
	}
	if st.ConnectedCaptionClients != 0 {
		t.Errorf("no subscribers yet, got %d", st.ConnectedCaptionClients)
	}
	sub, err := sup.hub.Subscribe("en")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sup.hub.Unsubscribe(sub)
	if st := sup.status(); st.ConnectedCaptionClients != 1 {
		t.Errorf("connected clients = %d, want 1", st.ConnectedCaptionClients)
	}
}

// TestParseDegradationBecomesAWarning: a whole reel losing its cues is critical;
// a lesser class is a warning. Neither may be dropped (guardrail #7).
func TestParseDegradationBecomesAWarning(t *testing.T) {
	w := parseDegradationWarning(resolver.ParseWarning{
		ReelNumber: 2, Language: "en", Classes: []string{"missing_editrate_defaulted_24"}, Count: 1,
	})
	if w.Code != model.WarnCaptionParseDegraded || w.Severity != model.SeverityWarning {
		t.Errorf("unexpected warning %+v", w)
	}
	lost := parseDegradationWarning(resolver.ParseWarning{
		ReelNumber: 2, Language: "en", Classes: []string{"all_cues_before_start_time"}, Count: 9,
	})
	if lost.Severity != model.SeverityCritical {
		t.Errorf("losing every cue in a reel is critical, got %q", lost.Severity)
	}
}

func mustAud(t *testing.T, store *config.SQLiteStore) model.AuditoriumProfile {
	t.Helper()
	aud, ok := auditoriumProfile(store)
	if !ok {
		t.Fatal("expected a stored auditorium profile")
	}
	return aud
}

// degradedTimedText is a timed-text asset whose <StartTime> origin falls after
// every cue, so the parser drops them all: the reel will show no captions at all
// (§5.4 all_cues_before_start_time).
const degradedTimedText = `<?xml version="1.0"?><SubtitleReel xmlns="urn:sim:428">` +
	`<Language>en</Language><EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate>` +
	`<StartTime>00:00:09:000</StartTime><SubtitleList><Font>` +
	`<Subtitle TimeIn="00:00:01:000" TimeOut="00:00:04:000"><Text>Line</Text></Subtitle>` +
	`</Font></SubtitleList></SubtitleReel>`

// TestParseDegradationSurvivesTheLoadThatRaisedIt drives the REAL machine and
// loader over the real §5.2 sequence in which Set RPL precedes the first Update
// Timeline — the order a server uses when it pushes the RPL before playback
// starts.
//
// The parse warning is raised from inside the loader, part-way through handling
// Set RPL, before the machine publishes the state describing the loaded show.
// A warning set that stamps the raise with the PREVIOUS observed state files it
// under the previous playout ID and drops it on the very next observation, so
// "this reel will show no captions" never reaches the dashboard at all.
func TestParseDegradationSurvivesTheLoadThatRaisedIt(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup, fetcher := newSupervisorWithFetcher(t, store, dir)
	fetcher.Files["tt_en.xml"] = []byte(degradedTimedText)
	rplBytes := sim.RPL("show-1", []sim.ReelSpec{{Number: 1, DurationMS: 60000, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
	}}})

	sup.machine.Connected()
	sup.machine.Handle(sim.Announce("srv", "Auditorium 3", ""))
	sup.machine.Handle(sim.GetNewLease(30000))
	sup.machine.Handle(sim.SetRPLInline(rplBytes))
	sup.machine.Handle(sim.SetOutputMode(true))
	sup.machine.Handle(sim.UpdateTimeline(0, "show-1", acsp.PlayPlaying))

	st := sup.status()
	if !hasCode(st.Warnings, model.WarnCaptionParseDegraded) {
		t.Fatalf("a reel that lost every cue must reach the dashboard, got %v", codes(st.Warnings))
	}
	// And it is a CONDITION, not a one-shot: a second poll still reports it.
	if st2 := sup.status(); !hasCode(st2.Warnings, model.WarnCaptionParseDegraded) {
		t.Errorf("the condition is still true; it must still be reported, got %v", codes(st2.Warnings))
	}

	// A clean show clears it — by MEASUREMENT (the next window load found nothing
	// degraded), not by the playout ID having moved on.
	fetcher.Files["tt_clean.xml"] = sim.TimedText("en", []sim.CueSpec{{StartMS: 1000, EndMS: 4000, Text: "Fine"}})
	clean := sim.RPL("show-2", []sim.ReelSpec{{Number: 1, DurationMS: 60000, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_clean.xml"},
	}}})
	sup.machine.Handle(sim.SetRPLInline(clean))
	// The new RPL chains after the first on the composition timeline, so the
	// server has to move the playhead into it for it to load.
	sup.machine.Handle(sim.UpdateTimeline(60000, "show-2", acsp.PlayPlaying))
	if st3 := sup.status(); hasCode(st3.Warnings, model.WarnCaptionParseDegraded) {
		t.Errorf("a clean load must clear the degradation warning, got %v", codes(st3.Warnings))
	}
}
