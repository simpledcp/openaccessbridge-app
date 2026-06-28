// Command captionbridge-firewall is the privileged network helper (§3.5). It is
// the ONLY component that needs root, kept small and auditable: it applies
// nftables rulesets and verifies forwarding/isolation, and answers core over a
// loopback Unix socket. It holds no web/parsing surface.
package main

import (
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
)

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentFirewall, JSON: true})
	log.Info(logging.EvProcessStarted, logging.Field("version", buildinfo.Version))

	sockPath := filepath.Join(*sockDir, ipc.SocketFirewall)
	srv, err := ipc.Listen(sockPath, handle(log))
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	defer srv.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

func handle(log *logging.Logger) ipc.Handler {
	return func(m ipc.Message) *ipc.Message {
		switch m.Type {
		case ipc.TypePing:
			return &ipc.Message{Type: ipc.TypePong, ID: m.ID}
		default:
			// Firewall apply/verify handlers are wired in by the firewall
			// subsystem (internal/security). Until then, acknowledge unknown
			// control frames without acting.
			return &ipc.Message{Type: "unsupported", ID: m.ID}
		}
	}
}
