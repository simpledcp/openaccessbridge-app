// Command captionbridge-core is the main supervised process (§3.5): the ACSP
// state machine, RPL/timed-text parsing, timing engine, caption fan-out, and the
// web/admin server. It connects to the media and firewall helpers over loopback,
// runs the security self-introspection, and assembles the dashboard readiness.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"sync"
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
	"github.com/simpledcp/captionbridge/internal/security/verify"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := fanout.NewHub(fanout.Options{Auditorium: *auditorium})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	defer ctrl.Close()

	// ACSP machine over a memory-backed resolver (a real server / Phase 0A wire
	// contract swaps in behind the same Resolver/Transport seam).
	fetcher := &resolver.MemoryFetcher{Files: map[string][]byte{}}
	res := resolver.NewPluggable(fetcher, nil, log)
	loader := resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(8<<20), log), 1)
	machine := acsp.New(acsp.Options{
		Controller: ctrl, Resolver: res, Loader: loader, Log: log,
		PreferredLanguages: preferredLanguages(store),
	})

	// Helper clients (best-effort): poll the media helper for audio status.
	mc := &mediaClient{sockPath: socketPath(*socketDir, ipc.SocketMedia)}
	go mc.pollLoop(ctx)

	srv, err := web.NewServer(web.Config{
		Profile: profile, PatronBind: *patronBind, AdminBind: *adminBind,
		Auditorium: *auditorium, DefaultLanguage: "en",
	}, web.Deps{
		Hub: hub, Log: log, Store: store,
		Languages: func() []web.LangOption { return langOptions(machine.State().AvailableLanguages) },
		Status: func() model.SystemReadiness {
			return assembleStatus(store, machine, mc, profile, *patronBind, *adminBind, *socketDir)
		},
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
		logging.Field("profile", string(profile)))

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

// mediaClient polls the media helper for the latest per-stream audio state.
type mediaClient struct {
	sockPath string
	mu       sync.Mutex
	latest   []model.AudioStreamState
}

func (c *mediaClient) pollLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.poll()
		}
	}
}

func (c *mediaClient) poll() {
	cli, err := ipc.Dial(c.sockPath)
	if err != nil {
		c.set(nil) // helper not running → no audio (dashboard shows audio not ready)
		return
	}
	defer cli.Close()
	resp, err := cli.Request(ipc.Message{Type: ipc.TypeMediaStatus, ID: "status"})
	if err != nil {
		c.set(nil)
		return
	}
	var states []model.AudioStreamState
	_ = json.Unmarshal(resp.Payload, &states)
	c.set(states)
}

func (c *mediaClient) set(s []model.AudioStreamState) {
	c.mu.Lock()
	c.latest = s
	c.mu.Unlock()
}

func (c *mediaClient) audio() []model.AudioStreamState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// assembleStatus builds the dashboard readiness from the live caption-link state,
// the polled audio states, and a LOCAL self-introspection verification (reading
// this appliance's own /proc/sys — never the network, §9.3).
func assembleStatus(store *config.SQLiteStore, m *acsp.Machine, mc *mediaClient, profile model.FirewallProfile, patronBind, adminBind, socketDir string) model.SystemReadiness {
	hasCred, _ := store.HasAnyAdminCredential()
	hasDefault, _ := store.HasDefaultCredential()
	sshOn, _, _ := store.GetSetting(config.SettingSSHEnabled)

	rep := verify.Run(verify.Config{
		Paths:                    verify.DefaultPaths(),
		Profile:                  profile,
		FirewallActive:           ipcReachable(socketPath(socketDir, ipc.SocketFirewall)),
		AdminReachableFromPatron: web.AdminReachableFromPatron(patronBind, adminBind),
		DefaultCredentials:       !hasCred || hasDefault,
		SSHEnabledOutsideSupport: sshOn == "true",
	})
	net := readiness.Network{
		TheaterInterface: settingOr(store, config.SettingTheaterInterface, "eth0"),
		ClientInterface:  settingOr(store, config.SettingClientInterface, "wlan0"),
		AllowedServer:    m.State().Server,
	}
	return readiness.Assemble(m.State(), mc.audio(), rep, net, readiness.DefaultLatencyBudgets())
}

func ipcReachable(path string) bool {
	cli, err := ipc.Dial(path)
	if err != nil {
		return false
	}
	defer cli.Close()
	resp, err := cli.Request(ipc.Message{Type: ipc.TypePing, ID: "p"})
	return err == nil && resp.Type == ipc.TypePong
}

func langOptions(langs []string) []web.LangOption {
	out := make([]web.LangOption, 0, len(langs))
	for _, l := range langs {
		out = append(out, web.LangOption{Code: l, Label: l})
	}
	return out
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
