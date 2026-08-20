// Command captionbridge-media is the supervised media process (§3.5). It runs the
// audio pipeline (capture → four-state qualifier → channel mapper → Opus encode →
// WebRTC) in isolation, so a media crash cannot drop the caption lease or the
// admin surface. It answers core over a loopback Unix socket with per-stream
// AudioStreamState — never audio samples (guardrail #5, §9.4).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/pipeline"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	synthetic := flag.Bool("synthetic", true, "run synthetic-tone HI/VI-N streams (no hardware) so the path can be exercised end to end")
	listDevices := flag.Bool("list-devices", false, "print the detected audio capture devices (§6.4 step 1) and exit")
	tierFlag := flag.String("input-tier", "", "§6.2 input tier of the connected hardware: tier0_builtin_jack|tier1_usb_dongle|tier2_usb_interface|tier3_balanced (default tier2)")
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentMedia, JSON: true})

	// An unrecognized tier is fatal, not silently coerced to the default: the
	// tier decides how harshly every input is judged (§6.2).
	tier, err := qualifier.ParseTier(*tierFlag)
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(2)
	}
	if *listDevices {
		os.Exit(printDevices(os.Stdout))
	}

	backend := encode.Select()
	log.Info(logging.EvProcessStarted,
		logging.Field("version", buildinfo.Version),
		logging.Field("backend", backend.Name()),
		logging.Field("input_tier", tier.String()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streams := map[model.StreamType]*pipeline.Stream{}
	if *synthetic {
		// Two independent synthetic streams prove HI and VI-N run independently
		// (§6.1) and let the dashboard show audio readiness with no hardware. Each
		// stream emits its own connect/verdict events (§9.4).
		streams[model.StreamHI] = startSynthetic(ctx, log, backend, tier, model.StreamHI, 440, 0.3)
		streams[model.StreamVIN] = startSynthetic(ctx, log, backend, tier, model.StreamVIN, 660, 0.25)
	}

	sockPath := filepath.Join(*sockDir, ipc.SocketMedia)
	srv, err := ipc.Listen(sockPath, handle(streams))
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	defer srv.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
	for _, s := range streams {
		_ = s.Stop()
	}
}

func startSynthetic(ctx context.Context, log *logging.Logger, backend encode.Backend, tier qualifier.Tier, stream model.StreamType, freq, amp float64) *pipeline.Stream {
	src := &capture.SyntheticTone{FreqHz: freq, Amplitude: amp, SampleRate: 48000, Channels: 1, FrameSize: 480}
	st := pipeline.NewStream(src, backend, pipeline.StreamConfig{
		DeviceID: "synthetic-" + string(stream), Stream: stream, Source: model.ChannelMono,
		Tier: tier, Logger: log,
	})
	go func() {
		if err := st.Run(ctx, true); err != nil && ctx.Err() == nil {
			log.Warn(logging.EvAudioInputDisconnected, logging.Field("stream", string(stream)))
		}
	}()
	return st
}

// printDevices lists the detected capture devices for the installer and for the
// §6.4 step-1 device picker. It prints the STABLE identifier (§6.1 by-id /
// by-path, never a card index) and says plainly when only a weak one is
// available or when the channel count cannot be read without opening the device.
func printDevices(w io.Writer) int {
	devs, err := capture.Devices()
	if err != nil {
		fmt.Fprintf(w, "no audio capture devices could be enumerated: %v\n", err)
		return 1
	}
	if len(devs) == 0 {
		fmt.Fprintln(w, "no audio capture devices detected")
		return 1
	}
	for _, d := range devs {
		channels := "unknown (not readable without opening the device)"
		if d.Channels > 0 {
			channels = fmt.Sprintf("%d", d.Channels)
		}
		stable := "stable across reboots"
		if !d.Stable {
			stable = "NOT reboot-stable — no by-id/by-path link for this device"
		}
		fmt.Fprintf(w, "%s\n  name:     %s\n  id kind:  %s (%s)\n  channels: %s\n  alsa:     %s\n",
			d.ID, d.FriendlyName, d.IDKind, stable, channels, d.ALSAName)
	}
	return 0
}

func handle(streams map[model.StreamType]*pipeline.Stream) ipc.Handler {
	return func(m ipc.Message) *ipc.Message {
		switch m.Type {
		case ipc.TypePing:
			return &ipc.Message{Type: ipc.TypePong, ID: m.ID}
		case ipc.TypeMediaStatus:
			states := make([]model.AudioStreamState, 0, len(streams))
			for _, s := range streams {
				states = append(states, s.Status())
			}
			payload, _ := json.Marshal(states)
			return &ipc.Message{Type: ipc.TypeMediaStatus, ID: m.ID, Payload: payload}
		default:
			return &ipc.Message{Type: "unsupported", ID: m.ID}
		}
	}
}
