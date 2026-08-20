package calibration

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/audio/mapper"
	"github.com/simpledcp/captionbridge/internal/audio/qualifier"
	"github.com/simpledcp/captionbridge/internal/model"
)

// stereo builds an interleaved two-channel block with independent per-channel
// amplitudes, so a test can make one side healthy and the other silent.
func stereo(ampL, ampR float64, frames int) []float64 {
	l := qualifier.SineWave(1000, ampL, 48000, 1, frames)
	r := qualifier.SineWave(1000, ampR, 48000, 1, frames)
	out := make([]float64, 0, frames*2)
	for i := 0; i < frames; i++ {
		out = append(out, l[i], r[i])
	}
	return out
}

func monoBlock(amp float64, frames int) Block {
	return Block{
		Samples: qualifier.SineWave(1000, amp, 48000, 1, frames), SampleRate: 48000,
		Channels: 1, Present: true, DeviceConnected: true,
	}
}

func usbDevice(channels int) capture.Device {
	return capture.Device{
		ID: "usb-Focusrite_Scarlett-00", IDKind: capture.DeviceIDByID, Stable: true,
		FriendlyName: "Scarlett Solo — Analog", Channels: channels, ALSAName: "hw:1,0",
	}
}

// §6.5: "A mapping that the detected hardware cannot satisfy (e.g., stereo split
// on a mono device) is a Failed qualifier state surfaced BEFORE a show, not a
// silent fault during one."
func TestStereoMappingOnMonoDeviceFailsBeforeTheShow(t *testing.T) {
	s := Session{Device: usbDevice(1), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier2USBInterface}

	f := s.Feasibility()
	if f.OK {
		t.Fatal("a stereo split on a mono device must not be reported feasible")
	}
	if f.Required != 2 || f.Detected != 1 {
		t.Errorf("feasibility = %+v, want required 2 / detected 1", f)
	}

	res := Analyze(s, monoBlock(0.3, 4800))
	if res.Verdict != model.QualFailed {
		t.Errorf("verdict = %q, want failed", res.Verdict)
	}
	if res.Usable() {
		t.Error("an unsatisfiable mapping must not be offered to patrons")
	}
	// And the fix must be concrete and actionable, not "check your settings".
	if !hasRec(res, "mapping_needs_stereo") {
		t.Errorf("no concrete mapping fix recommended: %+v", res.Recommendations)
	}
	if got := res.Summary.RecommendedMode; !strings.Contains(got, "mono") {
		t.Errorf("recommended mode = %q, want it to name the mono alternatives (§6.4 example)", got)
	}
}

// A legal stereo split on a stereo device is the case the pipeline previously
// got wrong by hard-coding one channel. Both streams must qualify independently.
func TestLegalStereoSplitQualifiesBothStreams(t *testing.T) {
	s := Session{Device: usbDevice(2), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier2USBInterface}
	res := Analyze(s, Block{
		Samples: stereo(0.3, 0.3, 4800), SampleRate: 48000, Channels: 2,
		Present: true, DeviceConnected: true,
	})

	if len(res.Streams) != 2 {
		t.Fatalf("got %d streams, want HI and VI-N", len(res.Streams))
	}
	if !res.Usable() {
		t.Errorf("a healthy stereo split should be usable, got %q: %+v", res.Verdict, res.Streams)
	}
	seen := map[model.StreamType]bool{}
	for _, st := range res.Streams {
		seen[st.StreamType] = true
		if st.Channels != 2 {
			t.Errorf("%s reports %d device channels, want 2 — the DEVICE is stereo even though the lane is mono",
				st.StreamType, st.Channels)
		}
	}
	if !seen[model.StreamHI] || !seen[model.StreamVIN] {
		t.Errorf("both HI and VI-N must be measured, got %+v", seen)
	}
	if res.Summary.StereoChannels != "Yes" {
		t.Errorf("summary stereo = %q, want Yes", res.Summary.StereoChannels)
	}
}

// The verdict is the WORST stream: one dead side of a stereo split must not be
// excused by a healthy other side (guardrail #7, no silent degradation).
func TestOneSilentSideFailsTheWholeInput(t *testing.T) {
	s := Session{Device: usbDevice(2), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier2USBInterface}
	res := Analyze(s, Block{
		Samples: stereo(0.3, 0, 4800), SampleRate: 48000, Channels: 2,
		Present: true, DeviceConnected: true,
	})

	if res.Verdict != model.QualFailed {
		t.Errorf("verdict = %q, want failed — the right channel is dead", res.Verdict)
	}
	if !hasRec(res, "no_signal") {
		t.Errorf("a dead channel must produce a cabling recommendation: %+v", res.Recommendations)
	}
	// The summary must describe the worst stream, not the healthy one.
	if !strings.Contains(res.Summary.InputDetected, "No") {
		t.Errorf("summary input detected = %q, want it to reflect the dead channel", res.Summary.InputDetected)
	}
}

// §6.4 step 7: recommendations must be CONCRETE and tier-aware. The same noisy
// measurement means different things on a built-in jack and a USB interface.
func TestRecommendationsAreTierAware(t *testing.T) {
	// A loud clipped signal: the fix ladder is level-at-source, then a pad.
	clipped := Block{
		Samples: qualifier.SineWave(1000, 1.0, 48000, 1, 4800), SampleRate: 48000,
		Channels: 1, Present: true, DeviceConnected: true,
	}
	res := Analyze(Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier2USBInterface}, clipped)
	if !hasRec(res, "clipping") {
		t.Fatalf("clipping not detected: %+v", res.Streams)
	}
	rec := recByCode(res, "clipping")
	for _, want := range []string{"Lower the output level", "attenuator pad"} {
		if !strings.Contains(rec.Action, want) {
			t.Errorf("clipping action %q should mention %q (§6.4 step 7)", rec.Action, want)
		}
	}
	if rec.Severity != SeverityBlocking {
		t.Errorf("clipping severity = %q, want blocking", rec.Severity)
	}

	// A quiet signal on a built-in jack should recommend better hardware; the
	// same level on a USB interface should just ask for more level.
	quiet := monoBlock(0.004, 4800) // ~ -50 dBFS: below mic-level, above silence
	onJack := Analyze(Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier0BuiltInJack}, quiet)
	onIface := Analyze(Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier2USBInterface}, quiet)

	if !hasRec(onJack, "low_level_mic_jack") {
		t.Errorf("tier 0 low level should recommend moving to a USB interface: %+v", onJack.Recommendations)
	}
	if !hasRec(onIface, "low_level") || hasRec(onIface, "low_level_mic_jack") {
		t.Errorf("tier 2 low level should ask for more level, not different hardware: %+v", onIface.Recommendations)
	}
}

// A device the kernel cannot describe must be reported as UNKNOWN, never
// silently assumed mono (which would hide a legal stereo mapping) or stereo
// (which would promise one that cannot work).
func TestUnknownChannelCountIsNeverGuessed(t *testing.T) {
	dev := capture.Device{ID: "PCH", IDKind: capture.DeviceIDCardID, Stable: false,
		FriendlyName: "HDA Intel PCH", Channels: 0}
	s := Session{Device: dev, Use: UseDual, Mode: mapper.StereoLeftHIRightVIN, Tier: qualifier.Tier0BuiltInJack}

	f := s.Feasibility()
	if !f.Unknown {
		t.Fatal("a device with no reported channel count must be Unknown, not decided")
	}
	res := Analyze(s, monoBlock(0.3, 4800))
	if !strings.Contains(res.Summary.StereoChannels, "Unknown") {
		t.Errorf("summary stereo = %q, want it to say unknown", res.Summary.StereoChannels)
	}
	if !hasRec(res, "mapping_unconfirmed") {
		t.Errorf("an unconfirmable mapping must ask the installer to confirm on the meter: %+v", res.Recommendations)
	}
	// An unstable identifier is a re-cabling hazard worth flagging (§6.1, §8.5).
	if !hasRec(res, "unstable_device_id") {
		t.Errorf("a card-id-only device should raise the stable-identifier advisory: %+v", res.Recommendations)
	}
}

// §6.4 step 8: the wizard confirms usability with an explicit verdict AND a
// plain-language summary. This pins the shape of the spec's example block.
func TestSummaryRendersThePlainLanguageBlock(t *testing.T) {
	s := Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier0BuiltInJack}
	res := Analyze(s, monoBlock(0.004, 4800)) // low but usable

	got := res.Summary.String()
	for _, field := range []string{
		"Input detected:", "Signal level:", "Clipping:", "Noise floor:",
		"Stereo channels:", "Recommended mode:", "Verdict:",
	} {
		if !strings.Contains(got, field) {
			t.Errorf("summary is missing the %q line (§6.4 step 8):\n%s", field, got)
		}
	}
	if !strings.Contains(got, "Low but usable") {
		t.Errorf("a low-but-usable level should be described as such:\n%s", got)
	}
	if !strings.Contains(got, "No — mono only") {
		t.Errorf("a mono device should say so plainly:\n%s", got)
	}
	// Never leak a raw sample or a stack of numbers at a booth tech.
	if strings.Contains(got, "[]float64") || strings.Contains(got, "0x") {
		t.Errorf("summary must be plain language, got:\n%s", got)
	}
}

// §6.4 step 9: saving the profile persists the STABLE device id and the mapping
// so re-opening the room reuses known-good settings — never the card index.
func TestProfileSavesStableIdentityAndVerdict(t *testing.T) {
	s := Session{
		Device: usbDevice(2), Use: UseDual, Mode: mapper.StereoLeftHIRightVIN,
		Tier: qualifier.Tier2USBInterface, GainNotes: "attenuator pad inline; isolator fitted",
	}
	res := Analyze(s, Block{Samples: stereo(0.3, 0.3, 4800), SampleRate: 48000, Channels: 2,
		Present: true, DeviceConnected: true})

	p := s.Profile("screen-3", res)
	if p.InputDeviceID != "usb-Focusrite_Scarlett-00" {
		t.Errorf("profile device id = %q, want the stable by-id string", p.InputDeviceID)
	}
	if p.ChannelMapping != string(mapper.StereoLeftHIRightVIN) {
		t.Errorf("profile mapping = %q", p.ChannelMapping)
	}
	if p.LastQualification != res.Verdict {
		t.Errorf("profile verdict = %q, want %q", p.LastQualification, res.Verdict)
	}
	if p.GainNotes != s.GainNotes {
		t.Errorf("gain notes not carried into the profile")
	}
	// The card index must never become identity: it renumbers on reboot (§6.1).
	if strings.Contains(p.InputDeviceID, "hw:") || p.InputDeviceID == "1" {
		t.Errorf("profile stored a card index as identity: %q", p.InputDeviceID)
	}
}

// The mapping picker must not offer a mapping the hardware cannot satisfy —
// the installer should never be able to choose the failure in the first place.
func TestModesForNeverOffersAnImpossibleMapping(t *testing.T) {
	mono := usbDevice(1)
	for _, use := range []Use{UseHI, UseVIN, UseDual} {
		for _, m := range ModesFor(mono, use) {
			if requiredChannels(m) > 1 {
				t.Errorf("use %q on a mono device offered %q, which needs %d channels",
					use, m, requiredChannels(m))
			}
			if ModeLabel(m) == string(m) && m != mapper.MultiDevice {
				t.Errorf("mode %q has no human label for the admin selector", m)
			}
		}
	}

	st := usbDevice(2)
	dual := ModesFor(st, UseDual)
	if len(dual) == 0 {
		t.Fatal("a stereo device must offer a dual HI/VI-N mapping")
	}
	var hasSplit bool
	for _, m := range dual {
		if m == mapper.StereoLeftHIRightVIN || m == mapper.StereoLeftVINRightHI {
			hasSplit = true
		}
	}
	if !hasSplit {
		t.Errorf("a stereo device should offer an L/R split, got %v", dual)
	}
}

// The picker lists stable-identity devices first, deterministically (§6.1).
func TestSortDevicesPutsStableIdentifiersFirst(t *testing.T) {
	devs := []capture.Device{
		{ID: "PCH", Stable: false, FriendlyName: "Built-in"},
		{ID: "usb-B", Stable: true, FriendlyName: "Zoom"},
		{ID: "usb-A", Stable: true, FriendlyName: "Scarlett"},
	}
	SortDevices(devs)
	if !devs[0].Stable || !devs[1].Stable || devs[2].Stable {
		t.Fatalf("stable devices must sort first: %+v", devs)
	}
	if devs[0].FriendlyName != "Scarlett" || devs[1].FriendlyName != "Zoom" {
		t.Errorf("stable devices must then sort by name: %+v", devs)
	}
}

// A disconnected device is a blocking fault with its own fix, and must not be
// confused with a connected device that has gone quiet.
func TestDisconnectedDeviceIsDistinctFromSilence(t *testing.T) {
	s := Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier2USBInterface}

	gone := Analyze(s, Block{Samples: qualifier.SineWave(1000, 0, 48000, 1, 4800),
		SampleRate: 48000, Channels: 1, Present: false, DeviceConnected: false})
	if gone.Verdict != model.QualFailed || !hasRec(gone, "device_disconnected") {
		t.Errorf("a disconnected device must be a blocking, distinct fault: %+v", gone.Recommendations)
	}
	if hasRec(gone, "no_signal") {
		t.Error("a disconnected device should not also be reported as a cabling/level fault")
	}

	quiet := Analyze(s, Block{Samples: qualifier.SineWave(1000, 0, 48000, 1, 4800),
		SampleRate: 48000, Channels: 1, Present: true, DeviceConnected: true})
	if !hasRec(quiet, "no_signal") {
		t.Errorf("a connected but silent input must recommend checking the cabling: %+v", quiet.Recommendations)
	}
}

func hasRec(r Result, code string) bool {
	for _, rec := range r.Recommendations {
		if rec.Code == code {
			return true
		}
	}
	return false
}

func recByCode(r Result, code string) Recommendation {
	for _, rec := range r.Recommendations {
		if rec.Code == code {
			return rec
		}
	}
	return Recommendation{}
}

// §6.4 step 4 has the installer play a test TONE. A continuous tone has no quiet
// windows, so its estimated noise floor sits at the tone's own level — comparing
// that floor to a threshold alone would tell every installer playing a quiet
// tone to go fit a ground-loop isolator they do not need. Noise may only be
// blamed when the signal genuinely rides above the floor (§6.3).
func TestQuietTestToneIsNotBlamedOnNoise(t *testing.T) {
	for _, tier := range []qualifier.Tier{qualifier.Tier0BuiltInJack, qualifier.Tier2USBInterface} {
		s := Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: tier}
		res := Analyze(s, monoBlock(0.004, 4800)) // quiet pure tone, no background

		if hasRec(res, "noise_floor_isolate") || hasRec(res, "noise_floor") {
			t.Errorf("tier %s: a pure test tone was blamed on noise: %+v", tier, res.Recommendations)
		}
		if res.Summary.NoiseFloor == "High" || res.Summary.NoiseFloor == "Moderate" {
			t.Errorf("tier %s: summary claims a %q noise floor it cannot actually measure under a tone",
				tier, res.Summary.NoiseFloor)
		}
	}
}

// The converse: a signal that DOES ride above a high floor is real noise and
// must still be reported, or the guard above would silence a genuine fault.
// The fixture is a GATED tone — bursts separated by background-only gaps — so
// the low-percentile window estimator can actually see the background, which a
// continuous tone never lets it do.
func TestGenuineNoiseFloorIsStillReported(t *testing.T) {
	const (
		rate    = 48000
		burst   = 4800 // 100 ms of tone
		gap     = 4800 // 100 ms of background only
		cycles  = 5
		bgAmp   = 0.012 // ~ -38 dBFS background: above the -45 dBFS threshold
		toneAmp = 0.5   // ~ -9 dBFS signal, well clear of the background
	)
	tone := qualifier.SineWave(1000, toneAmp, rate, 1, burst)
	bg := qualifier.SineWave(7000, bgAmp, rate, 1, gap)

	var samples []float64
	for c := 0; c < cycles; c++ {
		for i := 0; i < burst; i++ {
			samples = append(samples, tone[i]+bgAmp*0.5)
		}
		samples = append(samples, bg...)
	}

	s := Session{Device: usbDevice(1), Use: UseHI, Mode: mapper.MonoHI, Tier: qualifier.Tier2USBInterface}
	res := Analyze(s, Block{Samples: samples, SampleRate: rate, Channels: 1, Present: true, DeviceConnected: true})

	st := res.Streams[0]
	gapDB := st.LevelDBFS - st.NoiseFloorDBFS
	if gapDB < 12 {
		t.Fatalf("fixture failed to separate signal from background: level %.1f dBFS, floor %.1f dBFS, gap %.1f dB",
			st.LevelDBFS, st.NoiseFloorDBFS, gapDB)
	}
	if !hasRec(res, "noise_floor") && !hasRec(res, "noise_floor_isolate") {
		t.Errorf("a genuinely high noise floor must still be reported: level %.1f floor %.1f, recs %+v",
			st.LevelDBFS, st.NoiseFloorDBFS, res.Recommendations)
	}
	if res.Summary.NoiseFloor != "Moderate" && res.Summary.NoiseFloor != "High" {
		t.Errorf("summary noise floor = %q, want it to report the measurable background", res.Summary.NoiseFloor)
	}
}
