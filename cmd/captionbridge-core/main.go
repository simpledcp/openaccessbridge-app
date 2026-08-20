// Command captionbridge-core is the main supervised process (§3.5): the ACSP
// state machine, RPL/timed-text parsing, timing engine, caption fan-out, and the
// web/admin server. It connects to the media and firewall helpers over loopback,
// runs the security self-introspection, and assembles the dashboard readiness.
//
// It is also the supervisor: it pushes the stored auditorium input profile to the
// media helper, has the privileged helper apply and verify the firewall, and
// reports each process's liveness to the dashboard (§3.5, §9.3, §10.6).
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
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
	"github.com/simpledcp/captionbridge/internal/sim"
	"github.com/simpledcp/captionbridge/internal/web"
)

func main() {
	var (
		configPath = flag.String("config", defaultConfigPath(), "SQLite config database path (configuration only — never content)")
		patronBind = flag.String("patron-bind", "0.0.0.0:8080", "patron web bind address")
		adminBind  = flag.String("admin-bind", "127.0.0.1:8090", "admin web bind address (isolated in production profiles)")
		auditorium = flag.String("auditorium", "Auditorium 1", "auditorium identity shown to patrons")
		socketDir  = flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory for the media/firewall helpers")
		demo       = flag.Bool("demo", true, "run a self-advancing in-process ACSP demo (no real server) driving the caption path")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentCore, JSON: true})
	if *showVer {
		os.Stdout.WriteString("captionbridge-core " + buildinfo.Version + "\n")
		return
	}
	log.Info(logging.EvProcessStarted, logging.Field("version", buildinfo.Version))

	store, err := config.OpenSQLite(*configPath)
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	defer store.Close()
	profile := activeProfile(store, log)
	deployed, _ := deploy.Resolve(profile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// D11: the per-profile concurrent-client cap is enforced by the fan-out
	// backpressure, which means the cap has to reach the fan-out (§12 capacity).
	hub := fanout.NewHub(fanout.Options{Auditorium: *auditorium, MaxClients: deployed.MaxConcurrentClients})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	defer ctrl.Close()

	// The caption path's loud warnings (§8.3) — lease near expiry, an asset the
	// server would not serve, a parse degradation, a server that is not the one
	// this room is pinned to. Guardrail #7 is not audio-only.
	captionWarnings := readiness.NewCaptionWarnings(leaseWarnFraction(store, log))

	// ACSP machine over a memory-backed resolver (a real server / Phase 0A wire
	// contract swaps in behind the same Resolver/Transport seam).
	fetcher := &resolver.MemoryFetcher{Files: map[string][]byte{}}
	res := resolver.NewPluggable(fetcher, nil, log)
	loader := newCaptionLoader(resolver.NewPipeline(res, resolver.NewCache(8<<20), log), 1, captionWarnings)
	aud, _ := auditoriumProfile(store)
	opts := captionMachineOptions(store, log, aud, captionWarnings)
	opts.Controller, opts.Resolver, opts.Loader = ctrl, res, loader
	machine := acsp.New(opts)

	sup := &supervisor{
		store: store, log: log, profile: profile, deployed: deployed,
		patronBind: *patronBind, adminBind: *adminBind,
		media:    newMediaClient(socketPath(*socketDir, ipc.SocketMedia), log),
		firewall: newFirewallClient(socketPath(*socketDir, ipc.SocketFirewall), log),
		machine:  machine, hub: hub, ctrl: ctrl, captionWarnings: captionWarnings,
	}
	// Install-time verification and the firewall apply happen BEFORE the surfaces
	// come up, so the dashboard's first answer is a measurement rather than a
	// zero value (§9.3 "at install and periodically at runtime").
	sup.reconcile(ctx)
	go sup.loop(ctx)

	srv, err := web.NewServer(web.Config{
		Profile: profile, PatronBind: *patronBind, AdminBind: *adminBind,
		Auditorium: *auditorium, AuditoriumID: auditoriumID(store),
		// §9.4: the locked profiles require a per-show patron token; Lab and Booth
		// Diagnostic stay open. An unrecognised stored profile requires one too —
		// the gate fails closed (web.RequirePerShowToken).
		DefaultLanguage: "en", PerShowToken: web.RequirePerShowToken(profile),
	}, web.Deps{
		Hub: hub, Log: log, Store: store,
		Languages: func() []web.LangOption { return langOptions(machine.State().AvailableLanguages) },
		// The patron audio surface projects this down to availability only; no
		// device name, level or reason string reaches a patron (§7.2).
		Audio: sup.media.audio,
		// Deps.Signaler stays nil: there is no webrtcbin peer to relay to
		// (BLOCKERS B4/B2), and the patron page reports listening as unavailable
		// rather than drawing a player that cannot play.
		Status: sup.status,
		// The §6.4 calibration wizard reaches captionbridge-media over the SAME
		// loopback socket dir this process was given. Without it the wizard falls
		// back to the default location and reports "the media process is not
		// answering" for every device whenever core runs with --socket-dir.
		MediaSocketDir: *socketDir,
	})
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	errCh := make(chan error, 2)
	srv.Start(errCh)
	log.Info(logging.EvStateTransition,
		logging.Field("phase", "serving"),
		logging.Field("patron_bind", *patronBind),
		logging.Field("admin_bind", *adminBind),
		logging.Field("profile", string(profile)),
		logging.Field("max_clients", deployed.MaxConcurrentClients))

	if complete, _, _ := store.GetSetting(config.SettingSetupComplete); complete != "true" {
		log.Info(logging.EvStateTransition, logging.Field("phase", "awaiting_first_boot_setup"))
	}

	if *demo {
		runACSPDemo(ctx, log, machine, fetcher)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errCh:
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	log.Info(logging.EvStateTransition, logging.Field("phase", "shutdown"))
}

// runACSPDemo drives the ACSP machine through the canonical sequence and then lets
// the timing engine self-advance, with periodic lease keepalives and a loop at
// show end — a realistic end-to-end caption demo with no real server.
func runACSPDemo(ctx context.Context, log *logging.Logger, m *acsp.Machine, fetcher *resolver.MemoryFetcher) {
	const showMS int64 = 60000
	fetcher.Files["tt_en.xml"] = sim.TimedText("en", []sim.CueSpec{
		{StartMS: 1000, EndMS: 4000, Text: "Welcome to CaptionBridge."},
		{StartMS: 6000, EndMS: 10000, Text: "Captions are served locally to your phone."},
		{StartMS: 12000, EndMS: 16000, Text: "[MUSIC PLAYING]"},
	})
	fetcher.Files["tt_es.xml"] = sim.TimedText("es-419", []sim.CueSpec{
		{StartMS: 1000, EndMS: 4000, Text: "Bienvenido a CaptionBridge."},
		{StartMS: 6000, EndMS: 10000, Text: "Los subtítulos se sirven localmente."},
		{StartMS: 12000, EndMS: 16000, Text: "[MÚSICA]"},
	})
	rplBytes := sim.RPL("1", []sim.ReelSpec{{Number: 1, DurationMS: showMS, Resources: []sim.ResSpec{
		{ID: "res-en", Type: "ClosedCaption", Language: "en", Locator: "tt_en.xml"},
		{ID: "res-es", Type: "ClosedCaption", Language: "es-419", Locator: "tt_es.xml"},
	}}})

	m.Connected()
	m.Handle(sim.Announce("demo-server", "Auditorium 1", ""))
	m.Handle(sim.GetNewLease(30000))
	m.Handle(sim.UpdateTimeline(0, "1", acsp.PlayPlaying))
	m.Handle(sim.SetRPLInline(rplBytes))
	m.Handle(sim.SetOutputMode(true))
	log.Info(logging.EvStateTransition, logging.Field("phase", "demo_acsp_playing"))

	go func() {
		tick := time.NewTicker(1 * time.Second)
		defer tick.Stop()
		var elapsed int64
		var sinceKeepalive int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.Tick()
				elapsed += 1000
				sinceKeepalive += 1000
				if sinceKeepalive >= 10000 { // lease keepalive at well under half-lease
					m.Handle(sim.Status(30000))
					sinceKeepalive = 0
				}
				if elapsed >= showMS { // loop the demo show
					elapsed = 0
					m.Handle(sim.UpdateTimeline(0, "1", acsp.PlayPlaying))
				}
			}
		}
	}()
}

// captionMachineOptions builds the ACSP machine's configuration from the stored
// auditorium profile and settings. The caller fills in Controller/Resolver/Loader.
//
// This is where the caption path stops being silent: OnWarning routes the §5.2
// conditions (lease near expiry, an asset the server would not serve) into the
// dashboard's warning set, OnState feeds the clearing rules, and
// ExpectedFingerprint arms the §8.5 wrong-room guard against the identity pinned
// for this auditorium. Without them the machine raises warnings nobody receives.
func captionMachineOptions(store *config.SQLiteStore, log *logging.Logger, aud model.AuditoriumProfile, w *readiness.CaptionWarnings) acsp.Options {
	return acsp.Options{
		Log:                log,
		PreferredLanguages: preferredLanguages(store),
		// Wrong-room guard (§8.5/§10.5): the identity pinned for this auditorium
		// at setup. Empty disables it — an unprovisioned appliance pins nothing.
		ExpectedFingerprint: aud.ExpectedServerFingerprint,
		WarnFraction:        leaseWarnFraction(store, log),
		FetchTimeout:        assetFetchTimeout(store, log),
		OnState:             w.Observe,
		OnWarning:           w.Raise,
	}
}

// newCaptionLoader builds the caption-asset loader together with the wiring that
// makes a content degradation visible: every timed-text parse degradation is
// raised into the dashboard's warning set, and every load reports what it found
// so a condition that has passed is retired on evidence (§5.3, §5.4).
//
// It is one constructor rather than three lines at the call site so the tests
// drive the SAME wiring the binary does. A hook assigned only in main() is a hook
// no test can notice going missing.
func newCaptionLoader(p *resolver.Pipeline, fetchAheadReels int, w *readiness.CaptionWarnings) *resolver.Loader {
	loader := resolver.NewLoader(p, fetchAheadReels)
	loader.OnParseWarning = func(pw resolver.ParseWarning) { w.Raise(parseDegradationWarning(pw)) }
	loader.OnLoadComplete = captionLoadOutcomeSink(w)
	return loader
}

// captionLoadOutcomeSink returns the hook that CLEARS the two content warnings
// when the load that just ran found nothing to justify them (§5.3).
//
// This is the clearing half of guardrail #7. A caption-content condition must
// stop being reported because a later load measured it gone, never because the
// link state happened to change shape: the conditions are raised part-way
// through loading a show, before the server has necessarily said which playout
// is on screen, so anything inferred from the link state at that moment files
// them under the wrong show and drops them unread.
func captionLoadOutcomeSink(w *readiness.CaptionWarnings) func(resolver.LoadOutcome) {
	return func(o resolver.LoadOutcome) {
		// Ordered after this load's own OnParseWarning calls, so a load that DID
		// find degradation never clears itself.
		if o.DegradedAssets == 0 {
			w.Clear(model.WarnCaptionParseDegraded)
		}
		// The machine raises the "did not serve every asset" warning from the same
		// load's result, after Load returns, so clearing here cannot erase it.
		if o.MissingAssets == 0 {
			w.Clear(model.WarnCaptionAssetUnavailable)
		}
	}
}

// parseDegradationWarning turns a timed-text parse degradation into a dashboard
// warning. It names the classes — never a cue, a locator or a title (§9.4).
func parseDegradationWarning(w resolver.ParseWarning) model.Warning {
	severity := model.SeverityWarning
	msg := "Caption assets for " + w.Language + " parsed with warnings (" + joinClasses(w.Classes) + ")"
	for _, c := range w.Classes {
		if c == "all_cues_before_start_time" {
			// A whole reel of captions vanished: that is not a nit.
			severity = model.SeverityCritical
			msg = "A caption asset for " + w.Language + " lost every cue when normalized to its start time — that reel will show no captions"
		}
	}
	return model.Warning{Code: model.WarnCaptionParseDegraded, Message: msg, Severity: severity}
}

func joinClasses(classes []string) string {
	out := ""
	for i, c := range classes {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}

func langOptions(langs []string) []web.LangOption {
	out := make([]web.LangOption, 0, len(langs))
	for _, l := range langs {
		out = append(out, web.LangOption{Code: l, Label: l})
	}
	return out
}

// auditoriumID is the stable room id bound into per-show patron tokens (§8.5).
// Empty lets internal/web derive one from the displayed room name.
func auditoriumID(store *config.SQLiteStore) string {
	profiles, _ := store.ListAuditoriumProfiles()
	for _, p := range profiles {
		if p.AuditoriumID != "" {
			return p.AuditoriumID
		}
	}
	return ""
}

func preferredLanguages(store *config.SQLiteStore) []string {
	profiles, _ := store.ListAuditoriumProfiles()
	for _, p := range profiles {
		if len(p.PreferredLanguages) > 0 {
			return p.PreferredLanguages
		}
	}
	return nil
}

// auditoriumProfile returns the stored §10.5 profile this appliance runs. One
// appliance serves one auditorium, so the first stored profile is it; the second
// result is false when setup has not created one yet, in which case the
// wrong-room guard and the media configuration stay switched off rather than
// running against a guess.
func auditoriumProfile(store *config.SQLiteStore) (model.AuditoriumProfile, bool) {
	profiles, err := store.ListAuditoriumProfiles()
	if err != nil || len(profiles) == 0 {
		return model.AuditoriumProfile{}, false
	}
	return profiles[0], true
}

// activeProfile reads the configured deployment posture. An un-provisioned or
// unrecognized value resolves to the safe default (Booth Diagnostic, D13) rather
// than Lab — a fresh appliance must not come up in the most permissive posture
// of the four (§9.2, §12.2).
func activeProfile(store *config.SQLiteStore, log *logging.Logger) model.FirewallProfile {
	v, ok, _ := store.GetSetting(config.SettingActiveProfile)
	if !ok || v == "" {
		return deploy.DefaultProfile
	}
	p, known := deploy.Resolve(model.FirewallProfile(v))
	if !known {
		// A stored profile we do not recognize is a misconfiguration, not a
		// reason to guess: fall back to the safe default and say so loudly
		// (guardrail #7 — never degrade silently).
		log.Warn(logging.EvStateTransition,
			logging.Field("configured_profile", "unrecognized"),
			logging.Field("resolved_profile", string(p.Name)))
	}
	return p.Name
}

// leaseWarnFraction reads the §5.2 lease-warning fraction. Out-of-range or
// unparsable values fall back to the default LOUDLY rather than silently, since
// a mis-set value changes when the booth is told the lease is slipping.
func leaseWarnFraction(store *config.SQLiteStore, log *logging.Logger) float64 {
	raw, ok, _ := store.GetSetting(config.SettingLeaseWarnFraction)
	if !ok || raw == "" {
		return readiness.DefaultLeaseWarnFraction
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 || f >= 1 {
		log.Warn(logging.EvStateTransition,
			logging.Field("setting", config.SettingLeaseWarnFraction),
			logging.Field("result", "out_of_range_default_used"))
		return readiness.DefaultLeaseWarnFraction
	}
	return f
}

// assetFetchTimeout reads the §5.2 per-fetch timeout. Zero leaves the machine's
// own default in force.
func assetFetchTimeout(store *config.SQLiteStore, log *logging.Logger) time.Duration {
	raw, ok, _ := store.GetSetting(config.SettingAssetFetchTimeoutMS)
	if !ok || raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Warn(logging.EvStateTransition,
			logging.Field("setting", config.SettingAssetFetchTimeoutMS),
			logging.Field("result", "out_of_range_default_used"))
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func settingOr(store *config.SQLiteStore, key, def string) string {
	if v, ok, _ := store.GetSetting(key); ok && v != "" {
		return v
	}
	return def
}

func socketPath(dir, name string) string { return dir + "/" + name }

func defaultConfigPath() string {
	if d := os.Getenv("CAPTIONBRIDGE_CONFIG"); d != "" {
		return d
	}
	if d := os.Getenv("CAPTIONBRIDGE_STATE_DIR"); d != "" {
		return d + "/config.db"
	}
	return "captionbridge-config.db"
}
