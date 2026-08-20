package calibration

// The LIVE half of the §6.4 wizard: the seam between the admin screen (which
// owns no audio) and captionbridge-media (which owns all of it). Step 5 asks for
// "a live input meter (the qualifier's real-time output)", so the measurements
// the wizard renders must be the SAME capture → qualify chain that runs during a
// show — not a second, wizard-only measurement path that could disagree with it.
//
// These are the request/report shapes that cross the loopback socket (§3.5).
// They carry MEASUREMENTS and configuration only, never samples (guardrail #5).
//
// PROVENANCE IS PART OF THE MEASUREMENT. Every report says what actually
// produced it. This appliance image has no ALSA capture source (BLOCKERS B3/B4),
// so the only source it can run is the synthetic tone; a wizard that measured
// that tone and presented the result as a calibration of the booth's named input
// would be writing a verdict nobody took into §10.5's last_qualification. So
// SourceKind travels with every report, Qualified() is the single place that
// decides whether a verdict may be persisted against a device, and a report that
// is not from hardware can never say yes.

import (
	"fmt"
	"strings"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Request is what the wizard asks captionbridge-media to measure — the
// installer's §6.4 step 1-3 selections, and nothing else.
type Request struct {
	// DeviceID is the STABLE device identifier from capture.Device.ID (§6.1),
	// never a card index. Empty means "no device chosen yet".
	DeviceID string `json:"device_id"`
	// Mode is the §6.5 channel mapping the installer selected.
	Mode mapper.Mode `json:"channel_mapping"`
	// Use is the §6.4 step-2 intended use. It only sharpens the wording; the
	// mapping is what decides which streams exist.
	Use Use `json:"use"`
	// Tier is the §6.2 hardware tier the installer declared, which selects the
	// qualifier thresholds this run is judged against.
	Tier qualifier.Tier `json:"input_tier"`
}

// SameAs reports whether two requests describe the same measurement, so a
// repeated poll refreshes a running meter instead of restarting the capture.
func (r Request) SameAs(o Request) bool {
	return r.DeviceID == o.DeviceID && r.Mode == o.Mode && r.Tier.Resolved() == o.Tier.Resolved()
}

// SourceKind records WHAT produced a measurement. It is not decoration: it is
// the fact that decides whether a verdict may be written against a device.
type SourceKind string

const (
	// SourceNone: nothing was measured. Either no run is active, or the run was
	// refused because this build cannot open what was asked for.
	SourceNone SourceKind = "none"
	// SourceHardware: a real capture device was opened and its own signal was
	// measured. This is the ONLY kind that may qualify a device (§6.4 step 9).
	SourceHardware SourceKind = "hardware"
	// SourceSynthetic: the synthetic tone stood in for a booth feed. Everything
	// downstream is a measurement OF THE TONE and of nothing else — the wizard
	// must say so on every figure it renders, and must not persist the verdict
	// as any device's last_qualification (§10.5).
	SourceSynthetic SourceKind = "synthetic"
)

// Label is the plain-language provenance line the wizard shows beside every
// measurement.
func (k SourceKind) Label() string {
	switch k {
	case SourceHardware:
		return "Measured from the selected capture hardware"
	case SourceSynthetic:
		return "SYNTHETIC TEST SOURCE — these figures measure a generated tone, not any audio input"
	default:
		return "Nothing measured"
	}
}

// Report is captionbridge-media's answer for one live calibration run: the
// qualifier's real-time output for every stream the mapping produces (§6.4
// step 5), plus the provenance of the measurement.
type Report struct {
	// Running reports whether a measurement is in progress right now.
	Running bool `json:"running"`
	// SourceKind and SourceDetail are the provenance. SourceDetail is a
	// content-free sentence a booth tech can read verbatim.
	SourceKind   SourceKind `json:"source_kind"`
	SourceDetail string     `json:"source_detail,omitempty"`
	// RequestedDeviceID is the device the installer picked; DeviceID is the
	// device the measurement actually came from. On a host with no capture
	// backend they differ, and the wizard renders the difference LOUDLY rather
	// than letting the requested name caption a tone's measurements.
	RequestedDeviceID string      `json:"requested_device_id"`
	DeviceID          string      `json:"device_id"`
	Mode              mapper.Mode `json:"channel_mapping"`
	// Streams is the per-named-stream state, straight from the running pipeline.
	Streams []model.AudioStreamState `json:"streams,omitempty"`
	// MeasuredMS is how long this run has been measuring. A verdict read a few
	// milliseconds after the capture started describes the start-up, not the
	// booth's signal (the same trap §10.5's last_qualification already has).
	MeasuredMS int `json:"measured_ms"`
	// Error is why nothing is being measured. It is set on a REFUSAL as much as
	// on a fault: refusing to substitute a tone for a named device is a normal,
	// expected answer on this build, and it must read as an explanation rather
	// than a crash (guardrail #7).
	Error string `json:"error,omitempty"`
}

// Qualified reports whether this report's verdict may be persisted as §10.5's
// last_qualification for the requested device. It is deliberately the narrowest
// possible test — a real device, opened for real, measured for real:
//
//   - the measurement came from hardware, not the synthetic stand-in;
//   - the device measured is the device the installer selected, so a report from
//     a previous selection cannot qualify the current one;
//   - at least one stream was actually measured.
//
// Anything else leaves the input UNQUALIFIED. A codebase that has already had a
// synthetic tone persisted under a real device's identity does not get to decide
// this loosely.
func (r Report) Qualified() bool {
	return r.SourceKind == SourceHardware &&
		r.DeviceID != "" && r.DeviceID == r.RequestedDeviceID &&
		len(r.Streams) > 0
}

// MinMeasureMS is how long a run must have been measuring before its verdict is
// worth saving. The pipeline qualifies in 200 ms analysis windows and the §6.3
// sustained-silence rule is a duration test, so a verdict read immediately after
// the capture opened has seen at most one window of start-up.
const MinMeasureMS = 1500

// Settled reports whether the run has measured for long enough that its verdict
// describes the input rather than the moment the capture opened.
func (r Report) Settled() bool { return r.MeasuredMS >= MinMeasureMS }

// DeviceList is captionbridge-media's answer to the §6.4 step-1 device picker.
type DeviceList struct {
	Devices []capture.Device `json:"devices"`
	// Error is why the list is empty. "ALSA is not present on this host" and "no
	// capture inputs are attached" are DIFFERENT facts and must not collapse into
	// one empty dropdown (§6.3 presence/device health).
	Error string `json:"error,omitempty"`
	// HardwareCapture reports whether this build can OPEN any of the listed
	// devices. False on an image with no ALSA capture source (BLOCKERS B3/B4):
	// the devices can still be listed, selected and saved into the auditorium
	// profile, but nothing on them can be measured here.
	HardwareCapture bool   `json:"hardware_capture"`
	HardwareDetail  string `json:"hardware_detail,omitempty"`
	// SyntheticDeviceID is the stand-in tone source's OWN name, offered in the
	// picker as itself so an installer can exercise the wizard end to end without
	// any device's identity being borrowed for it. Empty when no stand-in exists.
	SyntheticDeviceID string `json:"synthetic_device_id,omitempty"`
	// SyntheticLabel is the picker label for that entry.
	SyntheticLabel string `json:"synthetic_label,omitempty"`
}

// Uses lists the §6.4 step-2 intended uses in the order the wizard offers them.
func Uses() []Use { return []Use{UseHI, UseVIN, UseDual} }

// UseLabel is the plain-language name for an intended use (§6.4 step 2).
func UseLabel(u Use) string {
	switch u {
	case UseHI:
		return "Hearing assistance (HI) only"
	case UseVIN:
		return "Audio description (VI-N) only"
	case UseDual:
		return "Both, from this room's inputs (dual HI + VI-N)"
	default:
		return string(u)
	}
}

// ParseUse parses a §6.4 step-2 intended-use token. An unrecognized value is an
// error rather than a silent fallback to one of the three: guessing which
// accessibility service an installer meant is exactly the kind of quiet default
// that ships a room serving the wrong feed (guardrail #7).
func ParseUse(s string) (Use, error) {
	switch u := Use(strings.TrimSpace(s)); u {
	case UseHI, UseVIN, UseDual:
		return u, nil
	default:
		return "", fmt.Errorf("calibration: unrecognized intended use %q (§6.4 step 2: HI|VI-N|dual)", s)
	}
}

// DeviceSupports reports whether a mapping is worth offering for a device, which
// is the §6.5 rule the picker enforces: "a mapping that the detected hardware
// cannot satisfy is a Failed qualifier state surfaced BEFORE a show". A device
// that reports NO channel count (Channels == 0, typical of on-board HDA codecs)
// is neither proven able nor proven unable, so the mapping stays on offer and is
// confirmed by measurement instead — never rejected on a guess.
func DeviceSupports(dev capture.Device, m mapper.Mode) bool {
	if dev.Channels <= 0 || m == mapper.MultiDevice {
		return true
	}
	return dev.Channels >= RequiredChannels(m)
}
