// Command captionbridge-media is the supervised media process (§3.5). It runs the
// audio pipeline (capture → four-state qualifier → channel mapper → Opus encode →
// WebRTC) in isolation, so a media crash cannot drop the caption lease or the
// admin surface. It answers core over a loopback Unix socket with per-stream
// AudioStreamState — never audio samples (guardrail #5, §9.4).
//
// Core drives it with the §10.5 auditorium input profile over media.configure /
// media.start / media.stop: the persisted input_device_id and channel_mapping are
// configuration this process CONSUMES, not decoration on a settings page.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/pipeline"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

func main() {
	sockDir := flag.String("socket-dir", ipc.DefaultSocketDir(), "loopback socket directory")
	synthetic := flag.Bool("synthetic", true, "run synthetic-tone streams (no hardware) so the path can be exercised end to end")
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

	mgr := &manager{log: log, backend: backend, tier: tier, synthetic: *synthetic, parent: ctx}
	if *synthetic {
		// Until core pushes an auditorium profile, run the two independent
		// synthetic streams: they prove HI and VI-N run independently (§6.1) and
		// let the dashboard show audio readiness with no hardware. A
		// media.configure replaces them wholesale.
		mgr.applyDefaultSynthetic()
	}

	sockPath := filepath.Join(*sockDir, ipc.SocketMedia)
	srv, err := ipc.Listen(sockPath, mgr.handle)
	if err != nil {
		log.Error(logging.EvProcessUnhealthy, logging.Field("error", err.Error()))
		os.Exit(1)
	}
	defer srv.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
	mgr.stopAll()
}

// slot is one named stream in the current plan: how to build its source, and the
// running pipeline when it is started. A slot with a nil stream is configured but
// not running.
type slot struct {
	cfg      pipeline.StreamConfig
	channels int     // input channels the source must present for the mapping
	freqHz   float64 // synthetic stand-in only
	amp      float64

	stream *pipeline.Stream
	cancel context.CancelFunc
	src    capture.Source
}

// manager owns the streams and applies the §10.5 auditorium input profile core
// pushes over the loopback socket. Every mutation goes through its mutex: the IPC
// server handles each connection on its own goroutine.
type manager struct {
	mu        sync.Mutex
	log       *logging.Logger
	backend   encode.Backend
	tier      qualifier.Tier
	synthetic bool
	parent    context.Context

	deviceID string
	mapping  mapper.Mode
	order    []model.StreamType // stable report order
	slots    map[model.StreamType]*slot
}

func (m *manager) handle(msg ipc.Message) *ipc.Message {
	switch msg.Type {
	case ipc.TypePing:
		return &ipc.Message{Type: ipc.TypePong, ID: msg.ID}
	case ipc.TypeMediaStatus:
		payload, _ := json.Marshal(m.states())
		return &ipc.Message{Type: ipc.TypeMediaStatus, ID: msg.ID, Payload: payload}
	case ipc.TypeMediaConfigure:
		var prof model.AuditoriumProfile
		if err := json.Unmarshal(msg.Payload, &prof); err != nil {
			return result(msg, model.MediaCommandResult{Error: "malformed auditorium profile"})
		}
		return result(msg, m.configure(prof))
	case ipc.TypeMediaStart:
		stream, err := decodeStream(msg.Payload)
		if err != nil {
			return result(msg, model.MediaCommandResult{Error: err.Error()})
		}
		return result(msg, m.start(stream))
	case ipc.TypeMediaStop:
		stream, err := decodeStream(msg.Payload)
		if err != nil {
			return result(msg, model.MediaCommandResult{Error: err.Error()})
		}
		return result(msg, m.stop(stream))
	default:
		return &ipc.Message{Type: "unsupported", ID: msg.ID}
	}
}

// configure applies an auditorium input profile (§10.5): it parses the stored
// channel_mapping into the per-stream source channels (§6.5) and rebuilds the
// streams around them.
//
// A mapping this process cannot honor is REFUSED with a named reason rather than
// quietly left running the old plan — core turns the refusal into a loud
// dashboard warning (guardrail #7). That includes a real device: no ALSA capture
// source exists yet (BLOCKERS B3/B4), so a profile naming one is refused instead
// of being served by a synthetic tone pretending to be the booth's input.
func (m *manager) configure(prof model.AuditoriumProfile) model.MediaCommandResult {
	mode, err := mapper.ParseMode(prof.ChannelMapping)
	if err != nil {
		return model.MediaCommandResult{Error: "channel mapping not recognized (§6.5): " + prof.ChannelMapping}
	}
	// The ONLY capture.Source in this build is the synthetic tone (BLOCKERS
	// B3/B4). Two separate ways a profile can ask for something that does not
	// exist, and both are refused rather than approximated:
	//
	//   - the process was started with --synthetic=false, so there is no source
	//     at all;
	//   - the profile names a REAL capture device. Running a tone under that
	//     device's identity would put a verdict nobody measured into §10.5's
	//     last_qualification and §10.8's audio_device_compat_profile, both of
	//     which a tech reads as "how the booth's input measured".
	if !m.synthetic {
		return model.MediaCommandResult{
			Error: "no capture backend for a real input device in this build (BLOCKERS B3/B4); refusing to substitute a synthetic source",
		}
	}
	if !isSyntheticDeviceID(prof.InputDeviceID) {
		return model.MediaCommandResult{
			Error: "no capture backend for input device " + prof.InputDeviceID +
				" in this build (BLOCKERS B3/B4); refusing to report a synthetic tone as that device",
		}
	}
	// The mapping decides the shape the input must present, and the mapper is what
	// decides whether a shape can satisfy it (§6.5) — never an assumption here.
	// Offer the narrowest input the mode accepts: mono modes want exactly one
	// channel, the L/R, summed and stereo-pair modes want two.
	channels := 1
	plan, err := mapper.PlanFor(mode, channels)
	if err != nil {
		channels = 2
		plan, err = mapper.PlanFor(mode, channels)
	}
	if err != nil {
		return model.MediaCommandResult{Error: err.Error()}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopAllLocked()
	m.deviceID, m.mapping = prof.InputDeviceID, mode
	m.order = nil
	m.slots = map[model.StreamType]*slot{}
	// Distinct stand-in tones per stream so the two are distinguishable end to
	// end. They stand in for the booth's feed; nothing here claims to have
	// measured real hardware.
	tones := map[model.StreamType][2]float64{
		model.StreamHI:  {440, 0.3},
		model.StreamVIN: {660, 0.25},
	}
	for _, sm := range plan.Maps {
		tone := tones[sm.Stream]
		deviceID := prof.InputDeviceID
		if deviceID == "" {
			deviceID = syntheticDevicePrefix + string(sm.Stream)
		}
		m.order = append(m.order, sm.Stream)
		m.slots[sm.Stream] = &slot{
			cfg: pipeline.StreamConfig{
				DeviceID: deviceID, Stream: sm.Stream, Source: sm.Source,
				RequiredChannels: channels, Tier: m.tier, Logger: m.log,
			},
			channels: channels, freqHz: tone[0], amp: tone[1],
		}
	}
	m.log.Info(logging.EvStateTransition,
		logging.Field("phase", "media_configured"),
		logging.Field("device_id", m.deviceID),
		logging.Field("mapping", mode.String()),
		logging.Field("streams", len(m.slots)))
	for _, st := range m.order {
		m.startLocked(st)
	}
	return model.MediaCommandResult{OK: true, Streams: m.statesLocked()}
}

// applyDefaultSynthetic installs the pre-configuration synthetic plan.
func (m *manager) applyDefaultSynthetic() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slots = map[model.StreamType]*slot{}
	m.order = nil
	for _, s := range []struct {
		stream model.StreamType
		freq   float64
		amp    float64
	}{{model.StreamHI, 440, 0.3}, {model.StreamVIN, 660, 0.25}} {
		m.order = append(m.order, s.stream)
		m.slots[s.stream] = &slot{
			cfg: pipeline.StreamConfig{
				DeviceID: syntheticDevicePrefix + string(s.stream), Stream: s.stream,
				Source: model.ChannelMono, Tier: m.tier, Logger: m.log,
			},
			channels: 1, freqHz: s.freq, amp: s.amp,
		}
		m.startLocked(s.stream)
	}
}

// start runs a configured stream that is not currently running.
func (m *manager) start(stream model.StreamType) model.MediaCommandResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.slots[stream]
	if !ok {
		return model.MediaCommandResult{Error: "stream " + string(stream) + " is not in the configured channel mapping"}
	}
	if s.stream == nil {
		m.startLocked(stream)
	}
	return model.MediaCommandResult{OK: true, Streams: m.statesLocked()}
}

// stop stops a running stream, leaving it in the plan so it can be started again.
func (m *manager) stop(stream model.StreamType) model.MediaCommandResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.slots[stream]
	if !ok {
		return model.MediaCommandResult{Error: "stream " + string(stream) + " is not in the configured channel mapping"}
	}
	m.stopSlotLocked(s)
	m.log.Info(logging.EvStateTransition,
		logging.Field("phase", "media_stream_stopped"),
		logging.Field("stream", string(stream)))
	return model.MediaCommandResult{OK: true, Streams: m.statesLocked()}
}

// startLocked builds the source and runs the pipeline for one slot.
func (m *manager) startLocked(stream model.StreamType) {
	s := m.slots[stream]
	if s == nil || s.stream != nil {
		return
	}
	// The synthetic tone stands in for the booth's feed on a host with no audio
	// hardware; a real ALSA source plugs in behind the same capture.Source seam
	// (B3/B4). It is never presented as a measurement of real hardware.
	src := &capture.SyntheticTone{
		FreqHz: s.freqHz, Amplitude: s.amp, SampleRate: pipeline.DefaultSampleRate,
		Channels: s.channels, FrameSize: 480,
	}
	ctx, cancel := context.WithCancel(m.parent)
	st := pipeline.NewStream(src, m.backend, s.cfg)
	s.stream, s.cancel, s.src = st, cancel, src
	go func() {
		if err := st.Run(ctx, true); err != nil && ctx.Err() == nil {
			m.log.Warn(logging.EvAudioInputDisconnected, logging.Field("stream", string(stream)))
		}
	}()
}

func (m *manager) stopSlotLocked(s *slot) {
	if s == nil || s.stream == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.stream.Stop()
	s.stream, s.cancel, s.src = nil, nil, nil
}

func (m *manager) stopAllLocked() {
	for _, s := range m.slots {
		m.stopSlotLocked(s)
	}
}

func (m *manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopAllLocked()
}

// states returns the per-stream state of the RUNNING streams. A stream that is
// stopped is not reported at all: §10.2 states describe a live input, and
// inventing one for a stopped stream would be a measurement nobody took.
func (m *manager) states() []model.AudioStreamState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statesLocked()
}

func (m *manager) statesLocked() []model.AudioStreamState {
	out := make([]model.AudioStreamState, 0, len(m.slots))
	for _, name := range m.order {
		if s := m.slots[name]; s != nil && s.stream != nil {
			out = append(out, s.stream.Status())
		}
	}
	return out
}

// syntheticDevicePrefix marks the stand-in device identities this process makes
// up for itself. §10.5's input_device_id is otherwise a STABLE IDENTIFIER OF REAL
// HARDWARE (capture.Device.ID, from /dev/snd/by-id), and this build can open none
// of them.
const syntheticDevicePrefix = "synthetic-"

// isSyntheticDeviceID reports whether an auditorium profile's input_device_id can
// be honored by the synthetic stand-in. An empty id means the installer has not
// chosen a device yet (§6.4 step 1), which the stand-in may serve under its own
// name; anything else names hardware.
func isSyntheticDeviceID(id string) bool {
	return id == "" || strings.HasPrefix(id, syntheticDevicePrefix)
}

// decodeStream reads a stream name payload ("HI" / "VI-N").
func decodeStream(payload []byte) (model.StreamType, error) {
	var s model.StreamType
	if err := json.Unmarshal(payload, &s); err != nil {
		return "", errors.New("malformed stream name")
	}
	switch s {
	case model.StreamHI, model.StreamVIN:
		return s, nil
	default:
		return "", errors.New("unknown stream name")
	}
}

func result(m ipc.Message, r model.MediaCommandResult) *ipc.Message {
	payload, _ := json.Marshal(r)
	return &ipc.Message{Type: m.Type, ID: m.ID, Payload: payload}
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
