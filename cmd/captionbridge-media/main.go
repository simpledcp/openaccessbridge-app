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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/pipeline"
	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	synthetic := flag.Bool("synthetic", true, "run synthetic-tone HI/VI-N streams (no hardware) so the path can be exercised end to end")
	flag.Parse()

	log := logging.New(logging.Options{Component: buildinfo.ComponentMedia, JSON: true})
	log.Info(logging.EvProcessStarted, logging.Field("version", buildinfo.Version))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streams := map[model.StreamType]*pipeline.Stream{}
	backend := encode.Select()
	if *synthetic {
		// Two independent synthetic streams prove HI and VI-N run independently
		// (§6.1) and let the dashboard show audio readiness with no hardware.
		streams[model.StreamHI] = startSynthetic(ctx, log, backend, model.StreamHI, 440, 0.3)
		streams[model.StreamVIN] = startSynthetic(ctx, log, backend, model.StreamVIN, 660, 0.25)
		log.Info(logging.EvAudioInputConnected, logging.Field("mode", "synthetic"), logging.Field("backend", backend.Name()))
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

func startSynthetic(ctx context.Context, log *logging.Logger, backend encode.Backend, stream model.StreamType, freq, amp float64) *pipeline.Stream {
	src := &capture.SyntheticTone{FreqHz: freq, Amplitude: amp, SampleRate: 48000, Channels: 1, FrameSize: 480}
	st := pipeline.NewStream(src, backend, pipeline.StreamConfig{
		DeviceID: "synthetic-" + string(stream), Stream: stream, Source: model.ChannelMono,
	})
	go func() {
		if err := st.Run(ctx, true); err != nil && ctx.Err() == nil {
			log.Warn(logging.EvAudioInputDisconnected, logging.Field("stream", string(stream)))
		}
	}()
	return st
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
