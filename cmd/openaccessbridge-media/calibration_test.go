package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/audio/calibration"
	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// measureUntil polls the run until it has streams, or gives up. The pipeline
// needs a couple of analysis windows before it has anything to report.
func measureUntil(t *testing.T, m *manager, req calibration.Request) calibration.Report {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	rep := m.cal.measure(req)
	for len(rep.Streams) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		rep = m.cal.measure(req)
	}
	return rep
}

// THE REFUSAL THIS BUILD MUST NOT LOSE. There is no ALSA capture source in this
// image (BLOCKERS B3/B4). A calibration run whose entire output is a verdict a
// booth tech reads as "this is how the booth's input measured" must therefore
// refuse a real device outright, rather than measuring a tone and captioning the
// result with that device's name.
func TestCalibrateRefusesToMeasureARealInputDevice(t *testing.T) {
	m := newManager(t, true)
	rep := m.cal.measure(calibration.Request{
		DeviceID: "by-id:usb-Focusrite_Scarlett_Solo-00,0",
		Mode:     mapper.MonoHI, Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	})

	if rep.Running {
		t.Fatal("a run against an input this build cannot open must not be running")
	}
	if rep.SourceKind != calibration.SourceNone {
		t.Errorf("source kind = %q, want %q", rep.SourceKind, calibration.SourceNone)
	}
	if len(rep.Streams) != 0 {
		t.Errorf("a refused run must report NO measurements, got %d streams", len(rep.Streams))
	}
	if rep.Qualified() {
		t.Fatal("a refused run must never be able to qualify a device")
	}
	// Guardrail #7: the failure has to be loud and say what is actually wrong.
	if !strings.Contains(rep.Error, "B3/B4") || !strings.Contains(rep.Error, "Focusrite") {
		t.Errorf("refusal = %q; it must name the device and the reason", rep.Error)
	}
}

// The refusal is loud, but it is loud ONCE. The live meter re-asks twice a
// second, and an installer who leaves the wizard open on an input this build
// cannot measure would otherwise write the same warning to the appliance log a
// hundred times a minute — which buries the warnings that need to be read.
func TestARefusedSelectionWarnsOnceNotOnEveryPoll(t *testing.T) {
	var buf bytes.Buffer
	m := newManager(t, true)
	m.cal.log = logging.New(logging.Options{Component: "media", Writer: &buf, JSON: true})

	req := calibration.Request{
		DeviceID: "by-id:usb-Focusrite_Scarlett_Solo-00,0", Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	}
	for i := 0; i < 12; i++ {
		if rep := m.cal.measure(req); rep.Error == "" {
			t.Fatalf("poll %d: the refusal must be reported on EVERY poll, not only the first", i)
		}
	}
	if got := strings.Count(buf.String(), "calibration_refused"); got != 1 {
		t.Errorf("logged the refusal %d times over 12 polls, want 1", got)
	}
}

// The stand-in may be exercised end to end — under its OWN name, and labelled.
func TestCalibrateOnTheStandInIsLabelledSynthetic(t *testing.T) {
	m := newManager(t, true)
	rep := measureUntil(t, m, calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	})

	if !rep.Running {
		t.Fatalf("the stand-in run should be measuring: %+v", rep)
	}
	if rep.SourceKind != calibration.SourceSynthetic {
		t.Fatalf("source kind = %q, want %q", rep.SourceKind, calibration.SourceSynthetic)
	}
	if rep.DeviceID != syntheticStandInID {
		t.Errorf("measured device = %q, want the stand-in's own name %q", rep.DeviceID, syntheticStandInID)
	}
	if len(rep.Streams) != 1 || rep.Streams[0].StreamType != model.StreamHI {
		t.Fatalf("want one measured HI stream, got %+v", rep.Streams)
	}
	if rep.Qualified() {
		t.Error("a synthetic measurement must never qualify anything, not even the stand-in")
	}
	if !strings.Contains(rep.SourceDetail, "synthetic") {
		t.Errorf("source detail = %q; it must say the figures describe the tone", rep.SourceDetail)
	}
}

// A run that has only just started has qualified nothing yet. pipeline.NewStream
// seeds each stream with a Failed "not yet started" placeholder, and forwarding
// that as a measurement would put "Failed — reconnect the input device" on the
// meter the instant Start was pressed. That is a false alarm, which damages the
// loud warnings that matter as surely as a silent failure does.
func TestAJustStartedRunReportsNoMeasurementYet(t *testing.T) {
	m := newManager(t, true)
	rep := m.cal.measure(calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	})
	if !rep.Running {
		t.Fatal("the run should be starting")
	}
	if len(rep.Streams) != 0 {
		t.Fatalf("a run that has qualified nothing must report no measurements, got %+v", rep.Streams)
	}
	if rep.Settled() {
		t.Error("a run with no measurements must not report itself settled")
	}
	// And it must not stay quiet: one analysis window later, real state appears.
	if got := measureUntil(t, m, calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	}); len(got.Streams) == 0 {
		t.Error("the meter must start reporting once the pipeline has qualified a window")
	}
}

// Calibrating one input must not take down the audio a room is already serving.
func TestCalibrationRunDoesNotDisturbTheConfiguredStreams(t *testing.T) {
	m := newManager(t, true)
	if res := m.configure(model.AuditoriumProfile{
		AuditoriumID: "screen-3", ChannelMapping: "stereo:L=HI,R=VI-N",
	}); !res.OK {
		t.Fatalf("configure refused a valid mapping: %s", res.Error)
	}
	before := m.states()
	if len(before) != 2 {
		t.Fatalf("expected the configured HI + VI-N streams, got %d", len(before))
	}

	measureUntil(t, m, calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoVIN,
		Use: calibration.UseVIN, Tier: qualifier.Tier2USBInterface,
	})

	after := m.states()
	if len(after) != len(before) {
		t.Fatalf("the calibration run changed the configured streams: %d → %d", len(before), len(after))
	}
	for i := range after {
		if after[i].StreamType != before[i].StreamType || after[i].SourceChannel != before[i].SourceChannel {
			t.Errorf("configured stream %d changed: %+v → %+v", i, before[i], after[i])
		}
	}
}

// The meter refreshes by re-asking; that must not restart the capture, or the
// measurement would reset every poll and no verdict could ever settle.
func TestRepeatingTheSameRequestRefreshesRatherThanRestarts(t *testing.T) {
	m := newManager(t, true)
	req := calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	}
	first := measureUntil(t, m, req)
	time.Sleep(120 * time.Millisecond)
	second := m.cal.measure(req)

	if second.MeasuredMS < first.MeasuredMS {
		t.Errorf("elapsed measurement went backwards (%d ms → %d ms): the run restarted",
			first.MeasuredMS, second.MeasuredMS)
	}
}

// Changing the selection DOES restart, and the new run reports the new mapping.
func TestChangingTheSelectionStartsAFreshRun(t *testing.T) {
	m := newManager(t, true)
	base := calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI,
		Use: calibration.UseHI, Tier: qualifier.Tier2USBInterface,
	}
	measureUntil(t, m, base)

	next := base
	next.Mode = mapper.StereoLeftHIRightVIN
	next.Use = calibration.UseDual
	rep := measureUntil(t, m, next)
	if rep.Mode != mapper.StereoLeftHIRightVIN {
		t.Fatalf("report mapping = %q, want the newly selected one", rep.Mode)
	}
	if len(rep.Streams) != 2 {
		t.Fatalf("a stereo split measures both named streams, got %d", len(rep.Streams))
	}
}

// A process with no capture source at all has nothing to measure and says so.
func TestCalibrateWithNoSourceAtAllIsRefused(t *testing.T) {
	m := newManager(t, false)
	rep := m.cal.measure(calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI, Use: calibration.UseHI,
	})
	if rep.Running || rep.SourceKind != calibration.SourceNone {
		t.Fatalf("want a refusal, got %+v", rep)
	}
	if rep.Error == "" {
		t.Error("a refusal must carry a reason (guardrail #7)")
	}
}

// An unrecognized §6.5 mapping is refused rather than coerced into some default.
func TestCalibrateRefusesAnUnknownMapping(t *testing.T) {
	m := newManager(t, true)
	rep := m.cal.measure(calibration.Request{DeviceID: syntheticStandInID, Mode: "stereo:L=HI,R=HI"})
	if rep.Running || !strings.Contains(rep.Error, "§6.5") {
		t.Fatalf("want a §6.5 refusal, got %+v", rep)
	}
}

// stop releases the input and reports nothing measured afterwards.
func TestStopReleasesTheInput(t *testing.T) {
	m := newManager(t, true)
	measureUntil(t, m, calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MonoHI, Use: calibration.UseHI,
	})
	rep := m.cal.stop()
	if rep.Running || len(rep.Streams) != 0 || rep.SourceKind != calibration.SourceNone {
		t.Fatalf("after stop the run must report nothing measured, got %+v", rep)
	}
}

// §6.4 step 1's picker has to be honest twice over: about what is attached, and
// about the fact that nothing attached can be measured on this build.
func TestDeviceListSaysPlainlyThatNothingCanBeMeasuredHere(t *testing.T) {
	m := newManager(t, true)
	list := m.deviceList()

	if list.HardwareCapture {
		t.Fatal("this build has no ALSA capture source; the picker must not claim otherwise (B3/B4)")
	}
	if !strings.Contains(list.HardwareDetail, "B3/B4") {
		t.Errorf("hardware detail = %q; it must name why nothing can be measured", list.HardwareDetail)
	}
	if list.SyntheticDeviceID != syntheticStandInID {
		t.Errorf("stand-in id = %q, want %q", list.SyntheticDeviceID, syntheticStandInID)
	}
	if !strings.Contains(strings.ToLower(list.SyntheticLabel), "no hardware") {
		t.Errorf("stand-in label = %q; it must say what it is on the line the installer reads", list.SyntheticLabel)
	}
	// An enumeration FAILURE and "no inputs attached" must not collapse into one
	// empty dropdown (§6.3 presence / device health).
	if len(list.Devices) == 0 && list.Error == "" {
		t.Error("an empty device list must say whether enumeration failed or no inputs are attached")
	}
}

// A process started with no source still lists devices, but offers no stand-in:
// there is nothing to measure with, and pretending otherwise would put a Start
// button on a run that cannot happen.
func TestDeviceListOffersNoStandInWithoutASource(t *testing.T) {
	m := newManager(t, false)
	if id := m.deviceList().SyntheticDeviceID; id != "" {
		t.Errorf("stand-in id = %q, want none when no source exists", id)
	}
}

// §6.5's "Device A = HI, Device B = VI-N" is TWO physical inputs, and the wizard
// opens the one the installer selected. Left alone, mapper.PlanFor returns both
// named streams for that mode (correctly — the pairing is configured elsewhere),
// so the run would build two lanes out of ONE device and report HI and VI-N as
// measured while the second device was never opened. A tech reads two green
// lanes as "both feeds are qualified". Refuse instead, and say how to calibrate
// the pair.
func TestMultiDeviceMappingIsNotMeasuredFromASingleInput(t *testing.T) {
	m := newManager(t, true)
	rep := m.cal.measure(calibration.Request{
		DeviceID: syntheticStandInID, Mode: mapper.MultiDevice,
		Use: calibration.UseDual, Tier: qualifier.Tier2USBInterface,
	})
	if rep.Running {
		t.Error("a multi-device mapping must not start a run: only one of its two inputs is here")
	}
	if len(rep.Streams) != 0 {
		t.Errorf("a multi-device run measured %d streams from one input; both lanes would be fiction",
			len(rep.Streams))
	}
	if rep.SourceKind != calibration.SourceNone {
		t.Errorf("source kind = %q, want none: nothing was measured", rep.SourceKind)
	}
	if rep.Error == "" {
		t.Fatal("the refusal must reach the screen (guardrail #7: fail loudly)")
	}
	// The refusal has to be ACTIONABLE, not just a denial.
	for _, want := range []string{"two separate inputs", "one input at a time"} {
		if !strings.Contains(rep.Error, want) {
			t.Errorf("refusal %q does not tell the installer what to do instead (missing %q)", rep.Error, want)
		}
	}
	// And it must not have qualified anything on the way past.
	if rep.Qualified() {
		t.Error("a refused multi-device run must never qualify an input")
	}
}
