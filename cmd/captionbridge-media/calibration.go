package main

// The measuring half of the §6.4 calibration wizard. The admin screen lives in
// core and owns no audio; this process owns every capture device, so step 1's
// device list and step 5's "live input meter (the qualifier's real-time output)"
// are both answered here.
//
// A calibration run is a SECOND, independent set of pipeline.Streams that runs
// beside whatever the auditorium profile configured. Calibrating an input must
// never silence a room that is mid-show, and the run reports through the same
// capture → qualify chain the show uses, so the wizard's verdict and the
// dashboard's verdict cannot disagree.
//
// WHAT THIS BUILD WILL NOT DO. There is no ALSA capture source in this image
// (BLOCKERS B3/B4). manager.configure already refuses an auditorium profile that
// names a real device rather than serving a tone under that device's identity;
// a calibration run is the same refusal for the same reason, and a stronger one:
// its whole output is a verdict a booth tech reads as "this is how the booth's
// input measured". The synthetic stand-in is offered under ITS OWN name so the
// wizard can be exercised end to end, and every report says which it was.

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/audio/calibration"
	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/pipeline"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// syntheticStandInID is the tone source's own device identity in the §6.4 step-1
// picker. It is deliberately a name no booth input can have, and it satisfies
// isSyntheticDeviceID so the same guard covers the wizard and media.configure.
const syntheticStandInID = syntheticDevicePrefix + "tone"

// syntheticStandInLabel is what the picker shows for it. It says what it is on
// the line the installer reads before selecting it, not in a footnote.
const syntheticStandInLabel = "Synthetic test tone (no hardware — measures a generated signal, not an input)"

// calibrateStartupGrace is how long a freshly started run reports as STARTING
// rather than as a measurement. pipeline.NewStream seeds every stream with a
// Failed "not yet started" placeholder until its first 200 ms analysis window
// closes; forwarding that as a measurement would put "Failed — reconnect the
// input device" on the meter the instant an installer pressed Start, for a fault
// that has not happened. A false alarm nobody can act on erodes the loud warnings
// that do matter just as surely as a silent failure does. Nothing is suppressed:
// a stream that genuinely fails to start is still reported, one window later.
const calibrateStartupGrace = 300 * time.Millisecond

// calibrateIdleTimeout releases the input when nothing has polled the meter for
// this long. An installer who closes the wizard tab stops polling without saying
// so, and a capture device left open by a forgotten browser tab is a fault
// waiting for the next show.
const calibrateIdleTimeout = 30 * time.Second

// calibrator owns the live calibration run: at most one at a time, entirely
// separate from manager.slots.
type calibrator struct {
	mu      sync.Mutex
	log     *logging.Logger
	parent  context.Context
	backend encode.Backend
	// synthetic mirrors manager.synthetic: false means this process was started
	// with no source at all, so there is nothing to measure with.
	synthetic bool

	req      calibration.Request
	running  bool
	kind     calibration.SourceKind
	detail   string
	deviceID string
	errText  string
	started  time.Time
	lastPoll time.Time
	// lastRefusal is the reason already reported to the log, so a meter polling a
	// refused selection twice a second logs the line once.
	lastRefusal string

	order   []model.StreamType
	streams map[model.StreamType]*pipeline.Stream
	srcs    []capture.Source
	cancel  context.CancelFunc
}

// handleCalibration dispatches the §6.4 wizard's messages. It is called from
// manager.handle so the wizard shares the media process's single socket.
func (m *manager) handleCalibration(msg ipc.Message) *ipc.Message {
	switch msg.Type {
	case ipc.TypeMediaDevices:
		return payload(msg, m.deviceList())
	case ipc.TypeMediaCalibrate:
		var req calibration.Request
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			return payload(msg, calibration.Report{
				SourceKind: calibration.SourceNone,
				Error:      "malformed calibration request",
			})
		}
		return payload(msg, m.cal.measure(req))
	case ipc.TypeMediaCalibrateStop:
		return payload(msg, m.cal.stop())
	default:
		return nil
	}
}

// deviceList answers the §6.4 step-1 picker. It reports an enumeration FAILURE
// as a failure: "this host has no sound subsystem" and "no inputs are plugged
// in" are different facts, and an installer who cannot tell them apart will spend
// the afternoon re-seating a cable that was never the problem (§6.3).
func (m *manager) deviceList() calibration.DeviceList {
	out := calibration.DeviceList{
		// No ALSA capture source exists in this build, so nothing in this list can
		// actually be opened here. Saying so once, plainly, is what stops the rest
		// of the wizard from reading as a measurement of the chosen device.
		HardwareCapture: false,
		HardwareDetail: "this appliance image has no ALSA capture source (BLOCKERS B3/B4): an input can be " +
			"listed, selected and saved into the auditorium profile, but its signal cannot be measured here",
	}
	if m.synthetic {
		out.SyntheticDeviceID = syntheticStandInID
		out.SyntheticLabel = syntheticStandInLabel
	}
	devs, err := capture.Devices()
	if err != nil {
		out.Error = "no audio capture devices could be enumerated on this host"
		return out
	}
	calibration.SortDevices(devs)
	out.Devices = devs
	return out
}

// measure starts, refreshes, or restarts the live calibration run.
func (c *calibrator) measure(req calibration.Request) calibration.Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastPoll = time.Now()
	if c.running && c.req.SameAs(req) {
		// Same selection: this is the meter refreshing, not a new run. Restarting
		// would reset the measurement every poll and never let a verdict settle.
		c.req = req
		return c.reportLocked()
	}
	c.stopLocked()
	c.req = req
	c.started = time.Now()

	mode, err := mapper.ParseMode(string(req.Mode))
	if err != nil {
		return c.refuseLocked("channel mapping not recognized (§6.5): " + string(req.Mode))
	}

	// §6.5's "Device A = HI, Device B = VI-N" is TWO physical inputs, and this
	// wizard opens the ONE input the installer selected. mapper.PlanFor is right
	// to return both named streams for the mode — the pairing is configured
	// elsewhere — but running that plan here would build two lanes out of one
	// device and report HI and VI-N as measured when the second device was never
	// opened. A booth tech reads that as "both feeds are qualified", which is the
	// same shape of untrue as a tone reported under a device's name. Refuse, and
	// say how to calibrate the pair instead (§6.4 step 2 splits it cleanly).
	if mode == mapper.MultiDevice {
		return c.refuseLocked("the multi-device mapping serves HI and VI-N from two separate inputs, " +
			"and this wizard measures one input at a time: calibrate each device on its own by selecting " +
			"\"Hearing assistance (HI) only\" for one and \"Audio description (VI-N) only\" for the other")
	}

	// --- The two ways a request can name something this build cannot open. Both
	// are refused with a reason rather than approximated, and the refusal is the
	// same one manager.configure gives, for the same reason (§10.5). ---
	if !c.synthetic {
		return c.refuseLocked("this media process was started with no capture source, so there is nothing to measure")
	}
	if !isSyntheticDeviceID(req.DeviceID) {
		return c.refuseLocked("no capture backend for input device " + req.DeviceID +
			" in this build (BLOCKERS B3/B4); refusing to measure a synthetic tone and report it as that device")
	}

	// The mapping decides the shape the input must present (§6.5), and the mapper
	// is what decides whether a shape can satisfy it — never an assumption here.
	channels := calibration.RequiredChannels(mode)
	plan, err := mapper.PlanFor(mode, channels)
	if err != nil {
		return c.refuseLocked(err.Error())
	}

	deviceID := req.DeviceID
	if deviceID == "" {
		deviceID = syntheticStandInID
	}
	ctx, cancel := context.WithCancel(c.parent)
	c.cancel = cancel
	c.streams = map[model.StreamType]*pipeline.Stream{}
	c.order = nil
	c.srcs = nil
	c.kind = calibration.SourceSynthetic
	c.detail = "the synthetic tone source stood in for a booth feed; these figures describe that tone " +
		"and make no claim about " + describeRequestedDevice(req.DeviceID)
	c.deviceID = deviceID
	c.errText = ""
	c.lastRefusal = "" // a run that started re-arms the refusal warning

	// Distinct stand-in tones per stream so HI and VI-N are distinguishable end
	// to end. They stand in for the booth's feed; nothing here claims to have
	// measured hardware.
	tones := map[model.StreamType][2]float64{
		model.StreamHI:  {440, 0.3},
		model.StreamVIN: {660, 0.25},
	}
	for _, sm := range plan.Maps {
		tone := tones[sm.Stream]
		src := &capture.SyntheticTone{
			FreqHz: tone[0], Amplitude: tone[1], SampleRate: pipeline.DefaultSampleRate,
			Channels: channels, FrameSize: 480,
		}
		st := pipeline.NewStream(src, c.backend, pipeline.StreamConfig{
			DeviceID: deviceID, Stream: sm.Stream, Source: sm.Source,
			RequiredChannels: channels, Tier: req.Tier, Logger: c.log,
		})
		c.order = append(c.order, sm.Stream)
		c.streams[sm.Stream] = st
		c.srcs = append(c.srcs, src)
		go func() {
			// §6.4 step 4 has the installer play a known signal on purpose, so
			// silence during a calibration run is a real fault to report rather
			// than an idle input between shows — hence duringPlayback=true.
			_ = st.Run(ctx, true)
		}()
	}
	c.running = true
	go c.watchIdle(ctx)

	c.log.Info(logging.EvStateTransition,
		logging.Field("phase", "calibration_started"),
		logging.Field("source_kind", string(c.kind)),
		logging.Field("mapping", mode.String()),
		logging.Field("streams", len(c.order)))
	return c.reportLocked()
}

// describeRequestedDevice names what a refusal or a stand-in run is NOT a
// measurement of, without echoing an empty id as if it were a device.
func describeRequestedDevice(id string) string {
	if id == "" || isSyntheticDeviceID(id) {
		return "any audio input"
	}
	return "input device " + id
}

// refuseLocked records a refusal. Nothing runs, nothing is measured, and the
// report says why in a sentence an installer can act on (guardrail #7: a failure
// surfaces LOUDLY rather than degrading into a plausible-looking number).
//
// The warning is emitted on TRANSITION only. The live meter re-asks twice a
// second, so an installer who leaves the wizard open on an input this build
// cannot measure would otherwise write the same line to the appliance log a
// hundred times a minute — which buries the warnings that need to be read.
func (c *calibrator) refuseLocked(reason string) calibration.Report {
	c.running = false
	c.kind = calibration.SourceNone
	c.detail = ""
	c.deviceID = ""
	c.errText = reason
	if c.lastRefusal != reason {
		c.lastRefusal = reason
		c.log.Warn(logging.EvProcessUnhealthy,
			logging.Field("phase", "calibration_refused"),
			// The reason can quote an operator-supplied device id, which is
			// configuration text and does not belong in the log (§9.4). The class does.
			logging.Field("reason", "input_not_measurable"))
	}
	return c.reportLocked()
}

// stop ends the run and releases the input.
func (c *calibrator) stop() calibration.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	return c.reportLocked()
}

func (c *calibrator) stopLocked() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	for _, src := range c.srcs {
		_ = src.Close()
	}
	c.srcs = nil
	c.streams = nil
	c.order = nil
	c.running = false
	c.kind = calibration.SourceNone
	c.detail = ""
	c.deviceID = ""
	// The previous run's refusal belongs to the previous run. Carrying it into a
	// stopped or restarted one would put a stale reason on the screen next to a
	// state it does not describe.
	c.errText = ""
}

// watchIdle releases the input when the meter stops being polled.
func (c *calibrator) watchIdle(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			idle := c.running && time.Since(c.lastPoll) > calibrateIdleTimeout
			if idle {
				c.stopLocked()
				c.errText = "the calibration run was released because the live meter stopped being read"
			}
			c.mu.Unlock()
			if idle {
				c.log.Info(logging.EvStateTransition, logging.Field("phase", "calibration_idle_released"))
				return
			}
		}
	}
}

// report returns the current run's state without touching it.
func (c *calibrator) report() calibration.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportLocked()
}

func (c *calibrator) reportLocked() calibration.Report {
	rep := calibration.Report{
		Running:           c.running,
		SourceKind:        c.kind,
		SourceDetail:      c.detail,
		RequestedDeviceID: c.req.DeviceID,
		DeviceID:          c.deviceID,
		Mode:              c.req.Mode,
		Error:             c.errText,
	}
	if rep.SourceKind == "" {
		rep.SourceKind = calibration.SourceNone
	}
	if !c.running {
		return rep
	}
	elapsed := time.Since(c.started)
	rep.MeasuredMS = int(elapsed.Milliseconds())
	if elapsed < calibrateStartupGrace {
		// Running, but nothing has been qualified yet: report no streams rather
		// than the pipeline's placeholder. The screen renders "not measured".
		return rep
	}
	for _, name := range c.order {
		if st := c.streams[name]; st != nil {
			rep.Streams = append(rep.Streams, st.Status())
		}
	}
	return rep
}

// payload marshals a reply body onto the request's type and id.
func payload(m ipc.Message, v any) *ipc.Message {
	b, err := json.Marshal(v)
	if err != nil {
		return &ipc.Message{Type: m.Type, ID: m.ID}
	}
	return &ipc.Message{Type: m.Type, ID: m.ID, Payload: b}
}
