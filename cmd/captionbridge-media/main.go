// Command captionbridge-media is the supervised media process (§3.5). It runs the
// audio pipeline (ALSA/synthetic capture → four-state qualifier → channel mapper →
// Opus encode → WebRTC) in isolation, so a media crash cannot drop the caption
// lease or the admin surface. It answers core over a loopback Unix socket.
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

	log := logging.New(logging.Options{Component: buildinfo.ComponentMedia, JSON: true})
	log.Info(logging.EvProcessStarted, logging.Field("version", buildinfo.Version))

	sockPath := filepath.Join(*sockDir, ipc.SocketMedia)
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
			// Configure/start/stop/signal handlers are wired in by the audio
			// subsystem (internal/audio). Until then, acknowledge without acting.
			return &ipc.Message{Type: "unsupported", ID: m.ID}
		}
	}
}
