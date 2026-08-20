package calibration

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/model"
)

func healthyStream(s model.StreamType, src model.SourceChannel) model.AudioStreamState {
	return model.AudioStreamState{
		InputDeviceID: "usb-Focusrite_Scarlett-00", StreamType: s, SourceChannel: src,
		SampleRate: 48000, Channels: 2, LevelDBFS: -18, NoiseFloorDBFS: -70,
		Status: model.QualExcellent, Present: true, DeviceConnected: true,
	}
}

// THE RULE THIS PACKAGE EXISTS FOR. This appliance has already had a synthetic
// tone persisted under a real hardware device's identity as §10.5's
// last_qualification. Qualified() is the single gate that decides whether a
// measurement may become that field, and it must say no to everything except a
// real, settled measurement of the very device that was selected.
func TestOnlyASettledHardwareMeasurementMayQualifyADevice(t *testing.T) {
	hw := Report{
		SourceKind: SourceHardware, RequestedDeviceID: "usb-X", DeviceID: "usb-X",
		MeasuredMS: 5000, Streams: []model.AudioStreamState{healthyStream(model.StreamHI, model.ChannelMono)},
	}
	if !hw.Qualified() {
		t.Fatal("a real hardware measurement of the selected device must be able to qualify it")
	}

	synthetic := hw
	synthetic.SourceKind = SourceSynthetic
	if synthetic.Qualified() {
		t.Error("a SYNTHETIC measurement must never qualify a device — that is the fabrication this guard prevents")
	}

	none := hw
	none.SourceKind = SourceNone
	if none.Qualified() {
		t.Error("a run that measured nothing must never qualify a device")
	}

	otherDevice := hw
	otherDevice.DeviceID = "usb-Y"
	if otherDevice.Qualified() {
		t.Error("a measurement of a DIFFERENT input must never qualify the selected one")
	}

	empty := hw
	empty.Streams = nil
	if empty.Qualified() {
		t.Error("a report with no measured streams must never qualify a device")
	}
}

// §6.4 step 8's verdict has to describe the input, not the instant the capture
// opened: the pipeline qualifies in 200 ms windows and §6.3's sustained-silence
// rule is a DURATION test.
func TestAVerdictIsNotSettledUntilEnoughAudioWasMeasured(t *testing.T) {
	r := Report{
		SourceKind: SourceHardware, RequestedDeviceID: "usb-X", DeviceID: "usb-X",
		MeasuredMS: 50, Streams: []model.AudioStreamState{healthyStream(model.StreamHI, model.ChannelMono)},
	}
	if r.Settled() {
		t.Fatalf("a %d ms reading must not count as settled", r.MeasuredMS)
	}
	r.MeasuredMS = MinMeasureMS
	if !r.Settled() {
		t.Errorf("%d ms of audio should be enough to settle a verdict", r.MeasuredMS)
	}
}

// Judge is the same decision layer Analyze uses, over measurements taken by the
// live pipeline instead of a block. The two must never disagree, or the wizard
// and the dashboard would report different verdicts for the same input.
func TestJudgeAgreesWithAnalyzeOnTheSameMeasurements(t *testing.T) {
	s := Session{Device: usbDevice(2), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier2USBInterface}
	analyzed := Analyze(s, Block{
		Samples: stereo(0.3, 0.0004, 4800), SampleRate: 48000, Channels: 2,
		Present: true, DeviceConnected: true,
	})

	judged := Judge(s, analyzed.Streams)
	if judged.Verdict != analyzed.Verdict {
		t.Errorf("Judge verdict = %q, Analyze verdict = %q", judged.Verdict, analyzed.Verdict)
	}
	if judged.Summary != analyzed.Summary {
		t.Errorf("Judge summary = %+v, Analyze summary = %+v", judged.Summary, analyzed.Summary)
	}
	if len(judged.Recommendations) != len(analyzed.Recommendations) {
		t.Errorf("Judge recommended %d fixes, Analyze recommended %d",
			len(judged.Recommendations), len(analyzed.Recommendations))
	}
}

// §6.5: "A mapping that the detected hardware cannot satisfy (e.g., stereo split
// on a mono device) is a Failed qualifier state surfaced BEFORE a show." The
// live path is where that is easiest to get wrong: a mono device can produce two
// perfectly healthy-looking lanes if something upstream fed it two channels, and
// the measurements alone would then read Excellent.
func TestJudgeFailsAStereoMappingOnAMonoDeviceEvenWhenTheLanesLookClean(t *testing.T) {
	s := Session{Device: usbDevice(1), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier2USBInterface}
	res := Judge(s, []model.AudioStreamState{
		healthyStream(model.StreamHI, model.ChannelLeft),
		healthyStream(model.StreamVIN, model.ChannelRight),
	})
	if res.Verdict != model.QualFailed {
		t.Fatalf("verdict = %q, want failed: this device cannot satisfy the mapping", res.Verdict)
	}
	if res.Usable() {
		t.Error("an unsatisfiable mapping must not be offered to patrons")
	}
	if !hasRec(res, "mapping_needs_stereo") {
		t.Errorf("no concrete mapping fix recommended: %+v", res.Recommendations)
	}
}

// A device that reports NO channel count is not proven unable — the measurement
// itself is the stronger answer (§6.1: 0 means unknown, never mono).
func TestJudgeDoesNotFailADeviceThatSimplyReportsNoChannelCount(t *testing.T) {
	dev := capture.Device{ID: "card-id:PCH,0", IDKind: capture.DeviceIDCardID, FriendlyName: "HDA Intel PCH", Channels: 0}
	s := Session{Device: dev, Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier0BuiltInJack}
	res := Judge(s, []model.AudioStreamState{
		healthyStream(model.StreamHI, model.ChannelLeft),
		healthyStream(model.StreamVIN, model.ChannelRight),
	})
	if res.Verdict == model.QualFailed {
		t.Errorf("verdict = failed; an unknown channel count must be confirmed by measurement, not failed on a guess")
	}
	if !hasRec(res, "mapping_unconfirmed") {
		t.Errorf("the installer should still be asked to confirm on the meter: %+v", res.Recommendations)
	}
}

// Nothing measured is Failed, never "waiting" and never a blank verdict a screen
// could render as fine.
func TestJudgeWithNoMeasurementsIsFailed(t *testing.T) {
	s := Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI}
	if got := Judge(s, nil).Verdict; got != model.QualFailed {
		t.Errorf("verdict = %q, want failed when nothing was measured", got)
	}
}

// §6.4 step 2: guessing which accessibility service an installer meant is how a
// room ends up serving the wrong feed.
func TestParseUseRejectsAnUnknownToken(t *testing.T) {
	for _, ok := range []string{"HI", "VI-N", "dual", " dual "} {
		if _, err := ParseUse(ok); err != nil {
			t.Errorf("ParseUse(%q) errored: %v", ok, err)
		}
	}
	if _, err := ParseUse("hearing"); err == nil {
		t.Error("an unrecognized intended use must be an error, not a silent default")
	}
}

// The §6.4 step-1 picker must never reject a mapping on a guess.
func TestDeviceSupportsTreatsAnUnknownChannelCountAsUnproven(t *testing.T) {
	unknown := capture.Device{ID: "card-id:PCH,0", Channels: 0}
	if !DeviceSupports(unknown, mapper.StereoLeftHIRightVIN) {
		t.Error("a device with no reported channel count must not have a stereo mapping withheld")
	}
	if DeviceSupports(capture.Device{ID: "x", Channels: 1}, mapper.StereoLeftHIRightVIN) {
		t.Error("a device that reports one channel cannot satisfy a stereo split (§6.5)")
	}
	if !DeviceSupports(capture.Device{ID: "x", Channels: 1}, mapper.MultiDevice) {
		t.Error("multi-device pairs one mono input per stream, so one channel is enough")
	}
}

// The provenance label is what a booth tech reads next to every figure. It has
// to say plainly that a synthetic run measured no input at all.
func TestSyntheticProvenanceLabelSaysItMeasuredNoInput(t *testing.T) {
	got := SourceSynthetic.Label()
	if !strings.Contains(strings.ToLower(got), "synthetic") || !strings.Contains(strings.ToLower(got), "not any audio input") {
		t.Errorf("synthetic label = %q; it must say it is not a measurement of an input", got)
	}
}

// Settledness is part of the gate, not a separate check a caller may forget.
// Qualified() previously stopped at "hardware, right device, some streams", and
// the 1500 ms rule lived only in the web layer's own copy of the decision — two
// gates, the exported one weaker. A rule this load-bearing gets exactly one home.
func TestQualifiedRefusesAMeasurementThatHasNotSettled(t *testing.T) {
	base := Report{
		SourceKind: SourceHardware, RequestedDeviceID: "usb-X", DeviceID: "usb-X",
		Streams: []model.AudioStreamState{healthyStream(model.StreamHI, model.ChannelMono)},
	}
	for _, tc := range []struct {
		ms   int
		want bool
	}{
		{0, false},
		{MinMeasureMS - 1, false},
		{MinMeasureMS, true},
		{MinMeasureMS + 5000, true},
	} {
		rep := base
		rep.MeasuredMS = tc.ms
		if got := rep.Qualified(); got != tc.want {
			t.Errorf("Qualified() at %d ms = %v, want %v (§6.4 step 9 needs %d ms before a verdict describes the input)",
				tc.ms, got, tc.want, MinMeasureMS)
		}
		if rep.Qualified() != rep.Settled() {
			t.Errorf("at %d ms Qualified()=%v disagrees with Settled()=%v on an otherwise perfect report",
				tc.ms, rep.Qualified(), rep.Settled())
		}
	}
}

// A report with no device identity at all must not qualify one. An empty
// RequestedDeviceID equals an empty DeviceID, so an "ids match" test on its own
// says yes to a run that named nothing.
func TestQualifiedRefusesAReportWithNoDeviceIdentity(t *testing.T) {
	rep := Report{
		SourceKind: SourceHardware, MeasuredMS: 5000,
		Streams: []model.AudioStreamState{healthyStream(model.StreamHI, model.ChannelMono)},
	}
	if rep.Qualified() {
		t.Error("a measurement that names no device must never qualify one")
	}
}
