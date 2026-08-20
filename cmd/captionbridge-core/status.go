package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/deploy"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/readiness"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/verify"
	"github.com/simpledcp/captionbridge/internal/web"
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
type supervisor struct {
	store    *config.SQLiteStore
	log      *logging.Logger
	profile  model.FirewallProfile
	deployed deploy.Profile

	patronBind string
	adminBind  string

	media    *mediaClient
	firewall *firewallClient
	machine  *acsp.Machine
	hub      *fanout.Hub

	captionWarnings *readiness.CaptionWarnings

	mu sync.Mutex
	// network and versions are refreshed by reconcile and read by status, which
	// runs on the web goroutine.
	network      readiness.Network
	versions     model.SystemConfigVersion
	lastRecordAt time.Time
	lastRecordOK bool
	lastReason   string
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
		s.firewall.ensureApplied(firewall.ConfigFromInputs(firewall.Inputs{
			Profile: s.profile, TheaterIface: theater, ClientIface: client,
			Auditorium: aud, AdminOnClientIface: adminOnClient,
		}))
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

	// 3. Media: push the §10.5 input profile, then read back the stream states.
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
	s.mu.Unlock()
	net.ConnectedServer = link.Server

	return readiness.Assemble(readiness.Inputs{
		Link:            link,
		Audio:           s.media.audio(),
		Verify:          rep,
		Network:         net,
		CaptionWarnings: s.captionWarnings.Snapshot(),
		HelperWarnings:  append(s.media.warnings(), s.firewall.warnings()...),
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
