package main

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/caption"
	"github.com/simpledcp/openaccessbridge/internal/caption/acsp"
	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/caption/resolver"
	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/deploy"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/logging/debugcapture"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/readiness"
	"github.com/simpledcp/openaccessbridge/internal/security/firewall"
	"github.com/simpledcp/openaccessbridge/internal/security/support"
	"github.com/simpledcp/openaccessbridge/internal/security/verify"
	"github.com/simpledcp/openaccessbridge/internal/web"
)

const (
	// mediaPollInterval is how often the per-stream audio state is refreshed.
	mediaPollInterval = 2 * time.Second
	// postureInterval is how often the full reconciliation runs: firewall apply,
	// §9.3 verification, media configuration, and the §10.8 record.
	postureInterval = 15 * time.Second
	// readinessRecordThrottle bounds how often an UNCHANGED readiness outcome is
	// rewritten to the config store (§10.8). A changed outcome is written at once.
	readinessRecordThrottle = time.Minute
)

// supervisor is core's supervision half (§3.5): it reconciles the stored
// configuration with the two helper processes, runs the §9.3 verification, keeps
// the §10.8 versioning record honest, and assembles the dashboard verdict.
// captionStack is everything the caption path needs, constructed together so no
// caller can wire a subset. See the note on supervisor.captionStack.
type captionStack struct {
	machine *acsp.Machine
	hub     *fanout.Hub
	// res is the locator resolver, held so §5.3's unresolved-locator tally can
	// reach the dashboard. It is the concrete type rather than resolver.Resolver
	// because the accumulation is not part of the fetch interface.
	res *resolver.Pluggable
	// ctrl is the caption controller. The dashboard reads §8.2's timeline row
	// from it: the reference timing engine is what the server's transport state
	// actually drives, so it is a measurement rather than an inference.
	ctrl            *caption.Controller
	captionWarnings *readiness.CaptionWarnings
}

// newCaptionStack builds the caption path: warnings sink, resolver, loader,
// controller and ACSP machine, wired to each other and to the stored auditorium
// profile.
//
// It exists so the binary and the tests drive the SAME wiring. Every hook here
// is one a previous audit found assigned in main() alone and therefore invisible
// to the suite; the constructor is what makes "assigned in one place only"
// impossible rather than merely discouraged.
func newCaptionStack(store *config.SQLiteStore, log *logging.Logger, hub *fanout.Hub,
	ctrl *caption.Controller, fetcher resolver.MediaBlockFetcher, cacheBytes int64) captionStack {
	warnings := readiness.NewCaptionWarnings(leaseWarnFraction(store, log))
	res := resolver.NewPluggable(fetcher, nil, log)
	loader := newCaptionLoader(resolver.NewPipeline(res, resolver.NewCache(cacheBytes), log), 1, warnings)
	aud, _ := auditoriumProfile(store)
	opts := captionMachineOptions(store, log, aud, warnings)
	opts.Controller, opts.Resolver, opts.Loader = ctrl, res, loader
	return captionStack{
		machine: acsp.New(opts), hub: hub, res: res, ctrl: ctrl, captionWarnings: warnings,
	}
}

type supervisor struct {
	store    *config.SQLiteStore
	log      *logging.Logger
	profile  model.FirewallProfile
	deployed deploy.Profile

	patronBind string
	adminBind  string

	media    *mediaClient
	firewall *firewallClient

	// The caption path travels as ONE value, and that is a correction rather
	// than tidiness.
	//
	// These five were five independent fields assigned at two call sites — main()
	// and the test fixture — and the fixture was free to assign a field main()
	// forgot, or the reverse. A mutation proved it: deleting the resolver from
	// main()'s supervisor literal broke NOTHING, because the fixture supplied its
	// own. That is the same defect as the §8.5 screen name and the §12.4 profile
	// flags — a dependency wired in one place and silently missing in the other,
	// where every individual layer is correct and the feature does nothing.
	//
	// One constructor, one struct, both callers: a field that is not set is not
	// set for anybody, and the tests notice.
	captionStack

	// debugCapture owns §9.4's bounded content capture. Its warnings are §8.3's
	// "debug capture / support mode active" alert, which had no producer.
	debugCapture *debugcapture.Manager

	// support owns the §9.4 session: the only re-entry path once SSH is off, and
	// therefore the one whose window must never outlive its deadline.
	support *support.Manager
	// supportChanged carries the manager's open/close signal into the loop, so
	// the firewall rule is added and removed at the moment the window does,
	// rather than at the next scheduled posture pass.
	supportChanged chan struct{}

	mu sync.Mutex
	// network and versions are refreshed by reconcile and read by status, which
	// runs on the web goroutine.
	network readiness.Network
	// support is §8.2's "Support session" row, read from the config store by
	// reconcile. status must not touch the store (an admin page load cannot be
	// allowed to stall on it), so it reads this cache.
	supportActive model.Tri
	versions      model.SystemConfigVersion
	lastRecordAt  time.Time
	lastRecordOK  bool
	lastReason    string
	// persistedAudioCompat is the device identity last written to the §10.8
	// record, so a newly qualified device is persisted at once rather than
	// waiting for the readiness outcome to change.
	persistedAudioCompat string
}

// loop runs the two cadences: a fast audio-state poll and the slower full
// reconciliation. Both run on this one goroutine, so nothing else touches the
// config store from here.
func (s *supervisor) loop(ctx context.Context) {
	poll := time.NewTicker(mediaPollInterval)
	defer poll.Stop()
	posture := time.NewTicker(postureInterval)
	defer posture.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			s.media.poll()
		case <-posture.C:
			s.reconcile(ctx)
		case <-s.supportChanged:
			// A support window just opened or closed. Reconcile at once: the
			// ruleset must gain or lose its egress rule when the window does.
			s.reconcile(ctx)
		}
	}
}

// reconcile brings the running system in line with the stored configuration and
// re-measures the posture. It is what makes the persisted §10.5 profile and the
// §9.1 firewall rules take effect instead of sitting in the database.
func (s *supervisor) reconcile(ctx context.Context) {
	_ = ctx
	aud, hasProfile := auditoriumProfile(s.store)

	// 1. Firewall: derive the ruleset from the stored configuration and have the
	//    privileged helper install it (§9.1). Incomplete configuration is skipped
	//    with a reason rather than sent as a half-formed ruleset.
	theater, _, _ := s.store.GetSetting(config.SettingTheaterInterface)
	client, _, _ := s.store.GetSetting(config.SettingClientInterface)
	allowlist := firewall.ParseAllowlist(aud.CinemaServerIP)
	adminOnClient := web.AdminReachableFromPatron(s.patronBind, s.adminBind)
	if theater != "" && client != "" && len(allowlist) > 0 {
		s.firewall.ensureApplied(s.derivedFirewallConfig(theater, client, aud, adminOnClient))
	} else {
		s.log.Info(logging.EvStateTransition,
			logging.Field("phase", "firewall_apply_awaiting_setup"),
			logging.Field("have_interfaces", theater != "" && client != ""),
			logging.Field("have_allowlist", len(allowlist) > 0))
	}

	// 2. Verification (§9.3). The facts carry the CONFIGURED interfaces only: an
	//    invented default would have the pass assert separation for interfaces
	//    this appliance does not use.
	hasCred, _ := s.store.HasAnyAdminCredential()
	hasDefault, _ := s.store.HasDefaultCredential()
	sshOn, _, _ := s.store.GetSetting(config.SettingSSHEnabled)
	rep := s.firewall.verifyPosture(model.PostureFacts{
		Profile:                  s.profile,
		TheaterIface:             theater,
		ClientIface:              client,
		TheaterAllowlist:         allowlist,
		AdminReachableFromPatron: adminOnClient,
		DefaultCredentials:       !hasCred || hasDefault,
		SSHEnabledOutsideSupport: sshOn == "true",
	})

	// 3. Media: confine the WebRTC peer's ICE gathering to the §9.1 client
	//    interface, push the §10.5 input profile, then read back the stream
	//    states. The scope goes first: it costs one loopback round trip and it
	//    decides which addresses the next patron to join is told to try, so it
	//    should not wait behind an audio reconfiguration (D27).
	s.media.applyPatronScope(client)
	reconfigured := s.media.configure(aud, hasProfile)
	s.media.poll()

	// 4. Dashboard facts + the §10.8 record.
	s.mu.Lock()
	s.network = readiness.Network{
		TheaterInterface: settingOr(s.store, config.SettingTheaterInterface, "eth0"),
		ClientInterface:  settingOr(s.store, config.SettingClientInterface, "wlan0"),
		// The pinned server is the CONFIGURED one (§8.2/§10.5). Showing the
		// connected server's own claim here would make a wrong-room mismatch
		// invisible: it can never contradict itself (§8.5).
		AllowedServer:    aud.CinemaServerIP,
		IdentityStrength: aud.IdentityStrength,
	}
	// §8.2 "Support session", read from the manager that owns the window rather
	// than from a stored flag. A persisted boolean cannot express a deadline, and
	// a support switch that is only ever written and never expires is the hole
	// this feature exists to avoid.
	_, sessionOpen := s.support.Current()
	s.supportActive = model.TriOf(sessionOpen)
	s.mu.Unlock()

	if !reconfigured {
		// A verdict read microseconds after the streams were rebuilt describes the
		// restart, not the auditorium's input. Persisting it would write a fault
		// this appliance caused itself into §10.5's last_qualification, which is a
		// field a tech reads as "how the input measured" (§6.4 step 9).
		s.recordAudioQualification(aud, hasProfile)
	}
	s.recordReadiness(s.buildStatus(rep), rep)
}

// derivedFirewallConfig is the ruleset this appliance's configuration and live
// state imply. It is a method rather than an expression inside reconcile so a
// test can ask what would be applied without a firewall helper to apply it to —
// the §9.4 support window is a rule that must appear and disappear with the
// window, and that is exactly the kind of wiring that is easy to leave out.
func (s *supervisor) derivedFirewallConfig(theater, client string, aud model.AuditoriumProfile, adminOnClient bool) firewall.Config {
	return firewall.ConfigFromInputs(firewall.Inputs{
		Profile: s.profile, TheaterIface: theater, ClientIface: client,
		Auditorium: aud, AdminOnClientIface: adminOnClient,
		// §9.4: nil unless a session is open right now. Egress() re-checks the
		// deadline, so a window whose time has passed contributes no rule even if
		// this pass beat the manager's expiry timer to it.
		SupportEgress: s.supportEgress(),
	})
}

// status is the dashboard snapshot (§10.6). It reads cached measurements and the
// live caption-link state; it never performs IPC or writes to the store, so an
// admin page load cannot stall on a wedged helper.
func (s *supervisor) status() model.SystemReadiness {
	return s.buildStatus(s.firewall.lastReport())
}

func (s *supervisor) buildStatus(rep verify.Report) model.SystemReadiness {
	link := s.machine.State()
	// Re-observe before snapshotting so the clearing rules run against the
	// freshest link state (a renewed lease clears the near-expiry warning).
	s.captionWarnings.Observe(link)

	s.mu.Lock()
	net := s.network
	versions := s.versions
	support := s.supportActive
	s.mu.Unlock()
	net.ConnectedServer = link.Server
	// §8.5: the OUTCOME of the wrong-room guard, not just the strength of the pin.
	// "Nothing pinned" and "pinned but the server answered nothing" must not
	// render like a match (§10.5).
	net.IdentityCheck = string(s.machine.Identity())

	cueVisible, cueKnown := s.hub.CueVisible()
	return readiness.Assemble(readiness.Inputs{
		Link: link,
		// §5.3's unresolved-locator evidence. The resolver has been counting these
		// since it was written and nothing read the count, so a venue whose server
		// emits a locator form we have no handler for produced a silent, invisible
		// tally. This is the reader.
		UnresolvedLocators: s.unresolvedLocators(),
		Audio:              s.media.audio(),
		Verify:             rep,
		Network:            net,
		CaptionWarnings:    s.captionWarnings.Snapshot(),
		// §8.3: an ACTIVE capture must be visibly indicated for as long as it
		// runs. The manager already produced these warnings and nothing consumed
		// them, so the alert could not appear however loudly it was configured.
		HelperWarnings: append(append(s.media.warnings(), s.firewall.warnings()...),
			s.debugCaptureWarnings()...),
		Deploy: readiness.Deploy{
			Profile: s.deployed.Name, Title: s.deployed.Title,
			WarningBanner: s.deployed.WarningBanner,
			// §12.4's operational requirements, and the evidence for each. They
			// were flags nothing read.
			RequireCalibration:   s.deployed.RequireCalibration,
			RequireSignalHealth:  s.deployed.RequireSignalHealth,
			RequiresPhase9Review: s.deployed.RequiresPhase9Review,
			CalibrationRecorded:  s.calibrationRecorded(),
			Phase9ReviewRecorded: s.phase9Recorded(),
			// §8.4's actual condition, measured from the two bind addresses this
			// process is serving on. verify.Report.AdminIsolated cannot stand in
			// for it: it answers "does this meet the profile's requirement", and
			// Lab and Booth Diagnostic impose none.
			AdminOnPatronNet: model.TriOf(web.AdminReachableFromPatron(s.patronBind, s.adminBind)),
		},
		// D27: the media peer's own account of what it offered the last patron.
		// It is a MEASUREMENT taken in the other process, so core passes it
		// through rather than deriving it from the interface it configured.
		ICEScope:      s.media.iceScope(),
		Timeline:      s.ctrl.Timeline(),
		CueVisible:    triIfKnown(cueVisible, cueKnown),
		DriftBudgetMS: s.ctrl.WarnBudgetMS(),
		DriftMeasured: model.TriOf(s.ctrl.DriftMeasured()),
		// §8.2 "Discovery mode". This build runs no discovery/advertisement
		// responder in any of the three processes — grep for mDNS/SSDP and there
		// is none — so "off" is a fact about the code, not an assumption about
		// the network. Any host-level responder (avahi and friends) is outside
		// the appliance and outside what this row claims.
		Discovery: model.TriNo,
		Support:   support,
		Processes: []model.ProcessLiveness{
			// Core reports itself alive by construction: it is the reporter, and
			// a process that could answer this request is running (§3.5).
			{Name: model.ProcessCore, Alive: true},
			s.media.liveness(),
			s.firewall.liveness(),
		},
		Clients:  readiness.Clients{Caption: s.hub.ClientCount(), Max: s.hub.MaxClients()},
		Versions: versions,
	})
}

// triIfKnown keeps an unanswerable question unanswered. A "no" that really means
// "nothing could tell" is the fabrication this whole surface exists to avoid.
func triIfKnown(v, known bool) model.Tri {
	if !known {
		return model.TriUnknown
	}
	return model.TriOf(v)
}

// recordAudioQualification writes the §10.5 last_qualification back to the
// auditorium profile and records the input device actually in play in the §10.8
// record. Both were write-only fields: nothing ever wrote the first and nothing
// ever wrote the second.
func (s *supervisor) recordAudioQualification(aud model.AuditoriumProfile, hasProfile bool) {
	states := s.media.audio()
	if len(states) == 0 {
		return
	}
	// The auditorium carries ONE verdict, so it is the worst of the streams: an
	// HI feed that qualifies does not make a failed VI-N feed acceptable (§6.3).
	worst := states[0].Status
	device := states[0].InputDeviceID
	for _, st := range states {
		if st.Status == "" {
			continue // not yet judged; a missing verdict is not a good one
		}
		if worst == "" || st.Status.Rank() > worst.Rank() {
			worst, device = st.Status, st.InputDeviceID
		}
	}
	if worst == "" {
		return
	}
	if hasProfile && aud.LastQualification != worst {
		aud.LastQualification = worst
		if err := s.store.PutAuditoriumProfile(aud); err != nil {
			s.log.Warn(logging.EvProcessUnhealthy,
				logging.Field("phase", "persist_last_qualification"),
				logging.Field("error", "write_failed"))
		}
	}
	s.mu.Lock()
	// §10.8 audio_device_compat_profile: the stable identifier of the input in
	// play. There is no per-device compatibility revision to record yet — that
	// wants real hardware qualification data (BLOCKER B3) — so this is the
	// device, not an invented "@rev".
	s.versions.AudioDeviceCompat = device
	s.mu.Unlock()
}

// recordReadiness persists the §10.8 last-good / last-failed readiness check.
//
// The old behavior stamped last_readiness_check_ok when the operator clicked
// Finish, with no check having run — the one field §10.8 exists for, fabricated.
// This records the outcome of a pass that ACTUALLY RAN: the verification result
// plus the composite warning set.
func (s *supervisor) recordReadiness(r model.SystemReadiness, rep verify.Report) {
	ok := rep.Pass && !r.HasCritical()
	reason := ""
	if !ok {
		reason = failureReason(r.Warnings)
	}

	fwVersion := s.firewall.version()
	s.mu.Lock()
	// A changed OUTCOME, or a changed revision, is written at once; an unchanged
	// outcome is rewritten only every so often so "last good check" keeps meaning
	// "recently" without a store write every cycle.
	changed := ok != s.lastRecordOK || reason != s.lastReason ||
		fwVersion != s.versions.FirewallProfileVersion && fwVersion != ""
	audioCompat := s.versions.AudioDeviceCompat
	changed = changed || audioCompat != s.persistedAudioCompat
	stale := time.Since(s.lastRecordAt) >= readinessRecordThrottle
	if !changed && !stale {
		s.mu.Unlock()
		return
	}
	s.lastRecordOK, s.lastReason, s.lastRecordAt = ok, reason, time.Now()
	s.mu.Unlock()

	v, err := s.store.GetSystemConfig()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if ok {
		v.LastReadinessCheckOK = &now
	} else {
		v.LastReadinessCheckFail = &model.ReadinessCheckResult{At: now, OK: false, Reason: reason}
	}
	if fwVersion != "" {
		v.FirewallProfileVersion = fwVersion
	}
	// §10.8's remaining two compatibility revisions. They were declared on the
	// record and never written, so "what changed since this last worked?" — the
	// question §10.8 exists to answer — could only be half-answered: a tech
	// comparing a working appliance with a broken one saw the schema and the
	// firewall and nothing about the hardware or the server family.
	//
	// Both are EMPTY when unknown rather than filled with a plausible default. A
	// revision string nobody set would compare equal between two appliances that
	// differ, which is worse than an obviously absent field.
	if hw := s.hardwareProfileVersion(); hw != "" {
		v.HardwareProfileVersion = hw
	}
	if sf := s.serverFamilyCompat(); sf != "" {
		v.ServerFamilyCompat = sf
	}
	s.mu.Lock()
	if s.versions.AudioDeviceCompat != "" {
		v.AudioDeviceCompat = s.versions.AudioDeviceCompat
	}
	s.mu.Unlock()
	if v.UpdateChannel == "" {
		v.UpdateChannel = "stable"
	}
	if err := s.store.PutSystemConfig(v); err != nil {
		s.log.Warn(logging.EvProcessUnhealthy,
			logging.Field("phase", "persist_readiness_result"),
			logging.Field("error", "write_failed"))
		return
	}
	s.mu.Lock()
	s.versions, s.persistedAudioCompat = v, v.AudioDeviceCompat
	s.mu.Unlock()
}

// failureReason renders why a readiness check failed as a list of §8.3 CODES.
// Codes only: the warning messages are operator-facing prose and the reason is
// persisted, so nothing free-form or content-bearing goes into the store (§9.4).
func failureReason(ws []model.Warning) string {
	seen := map[string]bool{}
	var critical, warning []string
	for _, w := range ws {
		if w.Code == "" || seen[w.Code] {
			continue
		}
		seen[w.Code] = true
		if w.Severity == model.SeverityCritical {
			critical = append(critical, w.Code)
			continue
		}
		warning = append(warning, w.Code)
	}
	sort.Strings(critical)
	sort.Strings(warning)
	all := append(critical, warning...)
	if len(all) == 0 {
		return "verification_failed"
	}
	if len(all) > 6 {
		all = all[:6]
	}
	return strings.Join(all, ",")
}

// debugCaptureWarnings is §8.3's alert for a running capture, or nothing.
func (s *supervisor) debugCaptureWarnings() []model.Warning {
	if s.debugCapture == nil {
		return nil
	}
	return s.debugCapture.Warnings()
}

// calibrationRecorded reports whether a §6.4 calibration that actually MEASURED
// an input is on file for this auditorium (§12.4).
//
// The evidence is §10.5's last_qualification, and it is good evidence precisely
// because the calibration lane refuses to write it for anything else: a run
// against the synthetic stand-in, or one that measured nothing, leaves it empty.
// That refusal is what makes this checkable at all.
func (s *supervisor) calibrationRecorded() bool {
	aud, ok := auditoriumProfile(s.store)
	if !ok {
		return false
	}
	return strings.TrimSpace(string(aud.LastQualification)) != ""
}

// phase9Recorded reports whether the §12.4 / §1.3 operational and legal review
// has been recorded. It is a RECORD, never a check: nothing on this appliance
// can verify that a review happened.
func (s *supervisor) phase9Recorded() bool {
	v, ok, _ := s.store.GetSetting(config.SettingPhase9Review)
	return ok && strings.TrimSpace(v) != ""
}

// hardwareProfileVersion is §10.8's hardware revision: the platform this build
// runs on plus the input devices it is actually serving from.
//
// It is built from OBSERVED devices, not from configuration, because the
// question it answers is "did the hardware change" — and a stored device id that
// no longer matches what is plugged in is precisely the case a tech is chasing.
// Synthetic stand-ins are marked as such, so a record from a bench appliance can
// never be mistaken for one from a booth (B3/B4).
func (s *supervisor) hardwareProfileVersion() string {
	devices := map[string]bool{}
	for _, a := range s.media.audio() {
		if id := strings.TrimSpace(a.InputDeviceID); id != "" {
			devices[id] = true
		}
	}
	if len(devices) == 0 {
		// Nothing observed: an empty revision is the honest answer, and it makes
		// the record obviously incomplete rather than falsely comparable.
		return ""
	}
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return runtime.GOOS + "/" + runtime.GOARCH + ":" + strings.Join(ids, ",")
}

// unresolvedLocators reports which locator forms this appliance could not fetch,
// attributed to the server family that emitted them (§5.3 / Phase 0A / B1).
//
// A nil resolver yields nothing rather than an empty-and-therefore-clean-looking
// answer, but the two render identically here for a reason: an empty list is not
// a claim that every locator resolved, it is the absence of any observation to
// the contrary. The dashboard shows the list when it is non-empty and says
// nothing when it is not, which is what §5.3 asks for — evidence, not a verdict.
func (s *supervisor) unresolvedLocators() []model.UnresolvedLocator {
	if s.res == nil {
		return nil
	}
	return s.res.UnresolvedLocators()
}

// serverFamilyCompat is §10.8's per-family compatibility revision: which cinema
// server family this auditorium is pinned to, and the ACSP variant in force.
//
// The family is a Phase 0A finding (B1) and is often unknown, which is why this
// returns EMPTY rather than a placeholder: "unknown" written into a revision
// field compares equal across two appliances talking to different servers.
func (s *supervisor) serverFamilyCompat() string {
	aud, ok := auditoriumProfile(s.store)
	if !ok {
		return ""
	}
	family := strings.TrimSpace(aud.ServerFamily)
	if family == "" {
		return ""
	}
	variant := strings.TrimSpace(string(aud.ACSPInitiation))
	if variant == "" {
		variant = string(model.ACSInitiated) // D6: Variant A is the working assumption
	}
	return family + "/" + variant
}
