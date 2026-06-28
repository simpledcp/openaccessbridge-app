// Command captionbridge-core is the main supervised process (§3.5): the ACSP
// state machine, RPL/timed-text parsing, timing engine, caption fan-out, and the
// web/admin server. It supervises captionbridge-media and captionbridge-firewall
// over loopback and reports their liveness to the dashboard.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/timedtext"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/web"
)

func main() {
	var (
		configPath = flag.String("config", defaultConfigPath(), "SQLite config database path (configuration only — never content)")
		patronBind = flag.String("patron-bind", "0.0.0.0:8080", "patron web bind address")
		adminBind  = flag.String("admin-bind", "127.0.0.1:8090", "admin web bind address (isolated in production profiles)")
		auditorium = flag.String("auditorium", "Auditorium 1", "auditorium identity shown to patrons")
		demoFile   = flag.String("demo-captions", "", "Phase-1 offline player: a timed-text XML file to loop against a fake show clock")
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

	profile := activeProfile(store)

	hub := fanout.NewHub(fanout.Options{Auditorium: *auditorium})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	defer ctrl.Close()

	srv, err := web.NewServer(web.Config{
		Profile:         profile,
		PatronBind:      *patronBind,
		AdminBind:       *adminBind,
		Auditorium:      *auditorium,
		DefaultLanguage: "en",
	}, web.Deps{
		Hub: hub,
		Log: log,
		Status: func() model.SystemReadiness {
			return model.SystemReadiness{
				TheaterInterface: settingOr(store, config.SettingTheaterInterface, "eth0"),
				ClientInterface:  settingOr(store, config.SettingClientInterface, "wlan0"),
				CaptionLink:      model.CaptionLinkState{State: model.StateDisconnected, AvailableLanguages: ctrl.Languages()},
			}
		},
		Languages: func() []web.LangOption {
			out := []web.LangOption{}
			for _, l := range ctrl.Languages() {
				out = append(out, web.LangOption{Code: l, Label: l})
			}
			return out
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

	if *demoFile != "" {
		if err := startDemo(log, ctrl, *demoFile); err != nil {
			log.Error(logging.EvAssetFetchFailed, logging.Field("error", err.Error()))
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errCh:
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Info(logging.EvStateTransition, logging.Field("phase", "shutdown"))
}

// startDemo loads a timed-text file and loops it against a real-time fake show
// clock, exercising the full caption path (parser → timing → fan-out → patron
// page) with no server in the loop — the Phase-1 offline web caption player.
func startDemo(log *logging.Logger, ctrl *caption.Controller, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	doc, err := timedtext.ParseBytes(b)
	if err != nil {
		return err
	}
	lang := doc.Language
	if lang == "" {
		lang = "en"
	}
	ctrl.LoadReel(map[string][]model.Cue{lang: doc.Cues})
	log.Info(logging.EvRPLReceived,
		logging.Field("phase", "demo_loaded"),
		logging.Field("cue_count", len(doc.Cues)),
		logging.Field("language", lang),
		logging.Field("format", string(doc.Format)))

	var maxEnd int64
	for _, c := range doc.Cues {
		if c.EndMS > maxEnd {
			maxEnd = c.EndMS
		}
	}
	loopMS := maxEnd + 3000

	ctrl.Play(0)
	go func() {
		t := time.NewTicker(time.Duration(loopMS) * time.Millisecond)
		defer t.Stop()
		for range t.C {
			ctrl.SeekTo(0) // loop the demo timeline
		}
	}()
	return nil
}

func activeProfile(store *config.SQLiteStore) model.FirewallProfile {
	if v, ok, _ := store.GetSetting(config.SettingActiveProfile); ok && v != "" {
		return model.FirewallProfile(v)
	}
	// Default for an un-set-up appliance: Lab (development). A real deployment
	// selects a profile in the setup wizard (D13).
	return model.ProfileLab
}

func settingOr(store *config.SQLiteStore, key, def string) string {
	if v, ok, _ := store.GetSetting(key); ok && v != "" {
		return v
	}
	return def
}

func defaultConfigPath() string {
	if d := os.Getenv("CAPTIONBRIDGE_CONFIG"); d != "" {
		return d
	}
	if d := os.Getenv("CAPTIONBRIDGE_STATE_DIR"); d != "" {
		return d + "/config.db"
	}
	return "captionbridge-config.db"
}
