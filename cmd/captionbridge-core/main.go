// Command captionbridge-core is the main supervised process (§3.5): the ACSP
// state machine, RPL/timed-text parsing, timing engine, caption fan-out, and the
// web/admin server. It supervises captionbridge-media and captionbridge-firewall
// over loopback and reports their liveness to the dashboard.
//
// This entrypoint is intentionally thin; the subsystems live under internal/ and
// are wired together here as they land.
package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
)

func main() {
	var (
		configPath = flag.String("config", defaultConfigPath(), "path to the SQLite config database (configuration only — never content)")
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

	// Surface whether first-boot setup is still required (§8.1). The web server,
	// supervisor, ACSP machine, and firewall client are wired in here as the
	// subsystems land.
	if complete, _, _ := store.GetSetting(config.SettingSetupComplete); complete != "true" {
		log.Info(logging.EvStateTransition, logging.Field("phase", "awaiting_first_boot_setup"))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info(logging.EvStateTransition, logging.Field("phase", "shutdown"))
}

// defaultConfigPath returns the config DB path, preferring an explicit runtime
// dir, then XDG, then a local file.
func defaultConfigPath() string {
	if d := os.Getenv("CAPTIONBRIDGE_CONFIG"); d != "" {
		return d
	}
	if d := os.Getenv("CAPTIONBRIDGE_STATE_DIR"); d != "" {
		return d + "/config.db"
	}
	return "captionbridge-config.db"
}
