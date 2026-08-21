// Package calibration is the engine behind the §6.4 audio calibration wizard —
// the guided, non-specialist workflow that makes the low-cost replacement
// profile viable in the field. It turns one measured block of captured audio
// into a verdict, a plain-language summary a booth tech can act on, and a set of
// CONCRETE fixes ("fit the attenuator pad", "add a ground-loop isolator"),
// then hands back the auditorium input profile to persist.
//
// It is PURE: no hardware, no ALSA, no logging of audio content. Device
// enumeration lives in internal/audio/capture, the mapping table in
// internal/audio/mapper, and the measurements in internal/audio/qualifier;
// this package is the decision layer over the three, so the admin UI is a thin
// renderer and the same logic is testable without a sound card.
//
// The wizard's own steps (§6.4): (1) select device, (2) select intended use,
// (3) select channel mapping, (4) prompt for a known signal, (5) live meter,
// (6) detect defects, (7) recommend a fix, (8) confirm usability with a verdict
// and summary, (9) save the profile. Steps 4 and 5 are UI over Analyze; the
// rest are here.
package calibration

import (
	"fmt"
	"sort"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Use is the intended accessibility use of an input (§6.4 step 2).
type Use string

const (
	UseHI   Use = "HI"   // hearing-impaired feed only
	UseVIN  Use = "VI-N" // visually-impaired narrative only
	UseDual Use = "dual" // both, from one stereo device (§6.5 stereo split)
)

// Session is the wizard's selections for one auditorium input (§6.4 steps 1-3).
// It carries configuration only — never audio.
type Session struct {
	Device capture.Device
	Use    Use
	Mode   mapper.Mode
	// Tier is the §6.2 hardware tier the installer declared. It selects the
	// qualifier thresholds and sharpens the recommendations: the same noisy
	// measurement means "expected of a built-in jack, move to a USB interface"
	// on tier 0 and "something is wrong with the wiring" on tier 2.
	Tier qualifier.Tier
	// GainNotes is the installer's free-text note persisted with the profile
	// (§10.5 "attenuator pad inline; isolator fitted"). It is configuration the
	// operator typed, so it is never logged (§9.4).
	GainNotes string
}

// Feasibility reports whether the selected mapping can be satisfied by the
// detected hardware (§6.5). A mapping the device cannot satisfy is a Failed
// state surfaced BEFORE a show, never a silent fault during one.
type Feasibility struct {
	OK bool `json:"ok"`
	// Unknown is true when the device did not expose a channel count (typical
	// of on-board HDA codecs, capture.Device.Channels == 0). The mapping is then
	// neither proven possible nor proven impossible, and the wizard must say so
	// rather than guess in either direction.
	Unknown  bool   `json:"unknown"`
	Required int    `json:"required_channels"`
	Detected int    `json:"detected_channels"`
	Reason   string `json:"reason,omitempty"`
}

// Feasibility checks the mapping against the detected device up front.
func (s Session) Feasibility() Feasibility {
	required := RequiredChannels(s.Mode)
	detected := s.Device.Channels

	switch {
	case s.Mode == mapper.MultiDevice:
		// Device A = HI, Device B = VI-N (§6.5): each device needs only one
		// channel, but the pairing itself is configured outside this session.
		return Feasibility{OK: true, Required: 1, Detected: detected,
			Reason: "multi-device mapping: this device feeds one stream; pair a second device for the other"}
	case detected <= 0:
		return Feasibility{OK: false, Unknown: true, Required: required, Detected: 0,
			Reason: "this device does not report a channel count until it is opened, so a " +
				itoa(required) + "-channel mapping cannot be confirmed in advance"}
	case detected < required:
		return Feasibility{OK: false, Required: required, Detected: detected,
			Reason: fmt.Sprintf("the mapping needs %d channels and this device offers %d", required, detected)}
	default:
		return Feasibility{OK: true, Required: required, Detected: detected}
	}
}

// itoa avoids importing strconv for one call site in a message string.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// RequiredChannels is how many capture channels a mode needs. It mirrors the
// §6.5 table in internal/audio/mapper: every stereo-derived mode needs two. It
// is exported because openaccessbridge-media has to size the input it opens for
// a live calibration run to the same table the wizard judged against (§6.4 step
// 5).
func RequiredChannels(m mapper.Mode) int {
	switch m {
	case mapper.StereoLeftHIRightVIN, mapper.StereoLeftVINRightHI,
		mapper.SummedMonoHI, mapper.SummedMonoVIN,
		mapper.StereoPairHI, mapper.StereoPairVIN:
		return 2
	default:
		return 1
	}
}

// Block is one measured chunk of captured audio for the wizard to judge
// (§6.4 steps 4-6). The samples are transient CONTENT: they are measured and
// discarded, never persisted or logged (guardrail #5).
type Block struct {
	Samples    []float64
	SampleRate int
	// Channels is the interleaved width of Samples as captured from the device.
	Channels int
	// Present and DeviceConnected come from the capture layer's health seam.
	Present         bool
	DeviceConnected bool
}

// Result is the wizard's answer for one block (§6.4 steps 6-8).
type Result struct {
	// Streams is the per-named-stream measurement the mapping produced.
	Streams []model.AudioStreamState `json:"streams"`
	// Verdict is the WORST of the per-stream verdicts: an input is only as good
	// as its weakest configured stream, and a Failed VI-N is not excused by a
	// clean HI (§6.3 conservative classification).
	Verdict         model.QualifierState `json:"verdict"`
	Feasible        Feasibility          `json:"-"`
	Recommendations []Recommendation     `json:"recommendations"`
	Summary         Summary              `json:"summary"`
}

// Usable reports whether this input may be offered to patrons at all.
func (r Result) Usable() bool { return r.Verdict.Usable() }

// Analyze runs the wizard's measurement pass over one block (§6.4 steps 5-8):
// it checks the mapping against the hardware, measures every stream the mapping
// produces, reduces them to one verdict, and derives the fixes and the summary.
func Analyze(s Session, b Block) Result {
	res := Result{Feasible: s.Feasibility()}
	th := qualifier.ThresholdsForTier(s.Tier)

	plan, err := mapper.PlanFor(s.Mode, b.Channels)
	if err != nil || !res.Feasible.OK {
		// The mapping cannot be satisfied by this hardware. That is a Failed
		// input surfaced before the show (§6.5), not an error to swallow — the
		// stream still gets a state so the dashboard has something to shout.
		reason := res.Feasible.Reason
		if reason == "" && err != nil {
			reason = err.Error()
		}
		// One Failed stream, so the reduced verdict Judge computes is Failed.
		res.Streams = []model.AudioStreamState{{
			InputDeviceID: s.Device.ID,
			StreamType:    primaryStream(s.Mode, s.Use),
			Channels:      b.Channels,
			SampleRate:    b.SampleRate,
			Status:        model.QualFailed,
			Present:       b.Present,
			// A mapping fault is a CONFIGURATION fault: the device may well be
			// connected and delivering audio.
			DeviceConnected: b.DeviceConnected,
			Reasons:         []string{reason},
		}}
		return Judge(s, res.Streams)
	}

	required := RequiredChannels(s.Mode)
	for _, sm := range plan.Maps {
		lane, err := mapper.Extract(sm.Source, b.Samples, b.Channels)
		if err != nil {
			res.Streams = append(res.Streams, model.AudioStreamState{
				InputDeviceID: s.Device.ID, StreamType: sm.Stream, SourceChannel: sm.Source,
				SampleRate: b.SampleRate, Channels: b.Channels, Status: model.QualFailed,
				Present: b.Present, DeviceConnected: b.DeviceConnected,
				Reasons: []string{err.Error()},
			})
			continue
		}
		laneChannels := 1
		if sm.Source == model.ChannelStereo {
			laneChannels = 2
		}
		st := qualifier.Analyze(qualifier.Input{
			Samples: lane, SampleRate: b.SampleRate,
			Channels: laneChannels, DeviceChannels: b.Channels,
			RequiredChannels: required,
			Present:          b.Present, DeviceConnected: b.DeviceConnected,
			// Calibration plays a known signal on purpose, so silence here is a
			// real fault to report rather than an idle input between shows.
			DuringPlayback: true,
		}, th)
		st.InputDeviceID = s.Device.ID
		st.StreamType = sm.Stream
		st.SourceChannel = sm.Source
		res.Streams = append(res.Streams, st)
	}

	return Judge(s, res.Streams)
}

// Judge derives the wizard's answer (§6.4 steps 6-8) from measurements taken
// SOMEWHERE ELSE — the live capture → qualify chain already running inside
// openaccessbridge-media, which is what §6.4 step 5's "live input meter" is showing.
// It is the same decision layer Analyze uses; only the origin of the
// measurements differs, so the verdict the installer sees in the wizard and the
// verdict the dashboard shows during a show can never disagree.
//
// It judges only what it is handed. An empty stream list is Failed, not
// "waiting": nothing was measured, and a wizard that renders an unmeasured input
// as anything but failed is the fabrication this package exists to prevent.
func Judge(s Session, streams []model.AudioStreamState) Result {
	res := Result{Feasible: s.Feasibility(), Streams: streams}
	res.Verdict = worst(res.Streams)
	// §6.5: a mapping the DETECTED hardware cannot satisfy is a Failed state
	// surfaced before a show, whatever the lanes currently measure — a stereo
	// split on a mono device can read perfectly on the one channel that exists.
	// A device that reports no channel count at all is a different case: it is
	// not proven unable, and the measurement itself (the qualifier compares the
	// mapping's required channels against the device count it actually saw) is a
	// stronger answer than a guess, so it is allowed to stand.
	if !res.Feasible.OK && !res.Feasible.Unknown {
		res.Verdict = model.QualFailed
	}
	res.Recommendations = recommend(s, res)
	res.Summary = summarize(s, res)
	return res
}

// worst reduces per-stream verdicts to the input's verdict. No configured
// stream may be silently outvoted by a healthier one.
func worst(streams []model.AudioStreamState) model.QualifierState {
	if len(streams) == 0 {
		return model.QualFailed
	}
	v := model.QualExcellent
	for _, s := range streams {
		if s.Status.Rank() > v.Rank() {
			v = s.Status
		}
	}
	return v
}

// primaryStream names the stream a mode feeds, for the failure path where no
// plan could be built. UseDual with an unusable mapping reports HI, the
// stricter of the two budgets (§6.7 / D10).
func primaryStream(m mapper.Mode, u Use) model.StreamType {
	switch m {
	case mapper.MonoVIN, mapper.SummedMonoVIN, mapper.StereoPairVIN:
		return model.StreamVIN
	case mapper.MonoHI, mapper.SummedMonoHI, mapper.StereoPairHI:
		return model.StreamHI
	}
	if u == UseVIN {
		return model.StreamVIN
	}
	return model.StreamHI
}

// Profile builds the auditorium input profile to persist (§6.4 step 9, §10.5),
// so re-opening the same room reuses known-good settings. It stores the STABLE
// device id and the mapping — never the card index, which renumbers on reboot.
func (s Session) Profile(auditoriumID string, r Result) model.AuditoriumProfile {
	return model.AuditoriumProfile{
		AuditoriumID:      auditoriumID,
		InputDeviceID:     s.Device.ID,
		ChannelMapping:    string(s.Mode),
		GainNotes:         s.GainNotes,
		LastQualification: r.Verdict,
	}
}

// SortDevices orders enumerated devices for the wizard's picker: devices with a
// stable identifier first (a card-id-only device is a re-cabling hazard, §6.1),
// then by friendly name, so the list is deterministic across reboots.
func SortDevices(devs []capture.Device) {
	sort.SliceStable(devs, func(i, j int) bool {
		if devs[i].Stable != devs[j].Stable {
			return devs[i].Stable
		}
		if devs[i].FriendlyName != devs[j].FriendlyName {
			return devs[i].FriendlyName < devs[j].FriendlyName
		}
		return devs[i].ID < devs[j].ID
	})
}

// ModesFor lists the channel mappings worth offering for a device and intended
// use (§6.5, mirrored by the admin selector). A mapping the hardware cannot
// satisfy is not offered at all — the wizard should not let an installer pick a
// stereo split on a mono device and discover it mid-show.
func ModesFor(dev capture.Device, use Use) []mapper.Mode {
	stereo := dev.Channels >= 2 || dev.Channels == 0 // unknown → offer, then confirm by measurement
	var out []mapper.Mode
	switch use {
	case UseHI:
		out = append(out, mapper.MonoHI)
		if stereo {
			out = append(out, mapper.SummedMonoHI, mapper.StereoPairHI)
		}
	case UseVIN:
		out = append(out, mapper.MonoVIN)
		if stereo {
			out = append(out, mapper.SummedMonoVIN, mapper.StereoPairVIN)
		}
	case UseDual:
		if stereo {
			out = append(out, mapper.StereoLeftHIRightVIN, mapper.StereoLeftVINRightHI)
		}
		// Two mono devices are the other way to serve both streams (§6.5).
		out = append(out, mapper.MultiDevice)
	}
	return out
}

// String renders the summary as the plain-language block §6.4 step 8 specifies.
func (s Summary) String() string { return strings.Join(s.Lines(), "\n") }
