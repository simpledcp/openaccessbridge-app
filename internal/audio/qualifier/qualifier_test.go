package qualifier

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

const sr = 48000

// --- synthetic signal helpers (all interleaved, mono unless noted) ---

func square(amplitude float64, frames int) []float64 {
	out := make([]float64, frames)
	half := frames / 200 // ~200 Hz at 48k, well within a window
	if half < 1 {
		half = 1
	}
	for n := 0; n < frames; n++ {
		if (n/half)%2 == 0 {
			out[n] = amplitude
		} else {
			out[n] = -amplitude
		}
	}
	return out
}

func clampedSine(freq, amplitude float64, frames int) []float64 {
	s := SineWave(freq, amplitude, sr, 1, frames)
	for i := range s {
		if s[i] > 1 {
			s[i] = 1
		} else if s[i] < -1 {
			s[i] = -1
		}
	}
	return s
}

func sineWithDC(freq, amplitude, dc float64, frames int) []float64 {
	s := SineWave(freq, amplitude, sr, 1, frames)
	for i := range s {
		s[i] += dc
	}
	return s
}

func nearSilence(frames int) []float64 {
	out := make([]float64, frames)
	for i := range out {
		out[i] = 1e-7
	}
	return out
}

// noiseWithBursts builds quiet broadband noise with sparse loud tone bursts so the
// low-percentile floor sits at the noise level while the signal rides well above it.
func noiseWithBursts(frames int) []float64 {
	r := rand.New(rand.NewSource(42))
	out := make([]float64, frames)
	noiseAmp := 0.017 // RMS ~ 0.01 => ~-40 dBFS, above the -45 floor threshold
	w := 2 * math.Pi * 1000 / float64(sr)
	for n := 0; n < frames; n++ {
		out[n] = (r.Float64()*2 - 1) * noiseAmp
		// Loud burst for ~15% of the buffer.
		if n%2000 < 300 {
			out[n] += 0.7 * math.Sin(w*float64(n))
		}
	}
	return out
}

func base(samples []float64, ch int) Input {
	return Input{
		Samples:         samples,
		SampleRate:      sr,
		Channels:        ch,
		Present:         true,
		DeviceConnected: true,
		InputDeviceID:   "usb-test-0",
		StreamType:      model.StreamHI,
		SourceChannel:   model.ChannelMono,
	}
}

func reasonsContain(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestAnalyzeClassification(t *testing.T) {
	const frames = sr / 10 // 100 ms

	tests := []struct {
		name        string
		mutate      func(*Input)
		wantStatus  model.QualifierState
		wantClip    bool
		wantSilence bool
		reasonSub   string // substring expected somewhere in Reasons
	}{
		{
			name:       "clean tone is excellent",
			mutate:     func(in *Input) { in.Samples = SineWave(1000, 0.5, sr, 1, frames) },
			wantStatus: model.QualExcellent,
			reasonSub:  "normal range",
		},
		{
			name:       "full-scale square clips and is marginal",
			mutate:     func(in *Input) { in.Samples = square(1.0, frames) },
			wantStatus: model.QualMarginal,
			wantClip:   true,
			reasonSub:  "clipping",
		},
		{
			name:       "over-amplitude sine clips and is marginal",
			mutate:     func(in *Input) { in.Samples = clampedSine(1000, 1.5, frames) },
			wantStatus: model.QualMarginal,
			wantClip:   true,
			reasonSub:  "clipping",
		},
		{
			name: "silent during playback fails",
			mutate: func(in *Input) {
				in.Samples = nearSilence(frames)
				in.DuringPlayback = true
			},
			wantStatus:  model.QualFailed,
			wantSilence: true,
			reasonSub:   "silent during playback",
		},
		{
			name: "silent before playback is marginal",
			mutate: func(in *Input) {
				in.Samples = nearSilence(frames)
				in.DuringPlayback = false
			},
			wantStatus:  model.QualMarginal,
			wantSilence: true,
			reasonSub:   "no signal detected",
		},
		{
			name:       "tiny amplitude is marginal low level",
			mutate:     func(in *Input) { in.Samples = SineWave(1000, 0.005, sr, 1, frames) },
			wantStatus: model.QualMarginal,
			reasonSub:  "signal level low",
		},
		{
			name:       "tiny amplitude warns about mic level",
			mutate:     func(in *Input) { in.Samples = SineWave(1000, 0.005, sr, 1, frames) },
			wantStatus: model.QualMarginal,
			reasonSub:  "mic-level input",
		},
		{
			name:       "dc offset is flagged marginal",
			mutate:     func(in *Input) { in.Samples = sineWithDC(1000, 0.5, 0.1, frames) },
			wantStatus: model.QualMarginal,
			reasonSub:  "DC offset",
		},
		{
			name:       "high noise floor is marginal",
			mutate:     func(in *Input) { in.Samples = noiseWithBursts(frames) },
			wantStatus: model.QualMarginal,
			reasonSub:  "noise floor is high",
		},
		{
			name:       "quiet-but-clean tone is usable",
			mutate:     func(in *Input) { in.Samples = SineWave(1000, 0.02, sr, 1, frames) },
			wantStatus: model.QualUsable,
			reasonSub:  "lower than ideal",
		},
		{
			name:       "hot-but-clean tone is usable",
			mutate:     func(in *Input) { in.Samples = SineWave(1000, 0.9, sr, 1, frames) },
			wantStatus: model.QualUsable,
			reasonSub:  "level is hot",
		},
		{
			name: "required stereo on mono input fails",
			mutate: func(in *Input) {
				in.Samples = SineWave(1000, 0.5, sr, 1, frames)
				in.Channels = 1
				in.RequiredChannels = 2
			},
			wantStatus: model.QualFailed,
			reasonSub:  "requires two channels",
		},
		{
			name: "not present fails",
			mutate: func(in *Input) {
				in.Samples = SineWave(1000, 0.5, sr, 1, frames)
				in.Present = false
			},
			wantStatus: model.QualFailed,
			reasonSub:  "no input detected",
		},
		{
			name: "device disconnected fails",
			mutate: func(in *Input) {
				in.Samples = SineWave(1000, 0.5, sr, 1, frames)
				in.DeviceConnected = false
			},
			wantStatus: model.QualFailed,
			reasonSub:  "no input detected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base(nil, 1)
			tc.mutate(&in)
			got := Analyze(in, DefaultThresholds())

			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (reasons=%v)", got.Status, tc.wantStatus, got.Reasons)
			}
			if got.Clipping != tc.wantClip {
				t.Errorf("Clipping = %v, want %v", got.Clipping, tc.wantClip)
			}
			if got.Silence != tc.wantSilence {
				t.Errorf("Silence = %v, want %v", got.Silence, tc.wantSilence)
			}
			if len(got.Reasons) == 0 {
				t.Fatalf("expected at least one reason, got none")
			}
			if tc.reasonSub != "" && !reasonsContain(got.Reasons, tc.reasonSub) {
				t.Errorf("reasons %v do not contain %q", got.Reasons, tc.reasonSub)
			}
		})
	}
}

// TestPassthroughFields confirms identity/config fields are echoed onto the state.
func TestPassthroughFields(t *testing.T) {
	in := base(SineWave(1000, 0.5, sr, 2, sr/10), 2)
	in.StreamType = model.StreamVIN
	in.SourceChannel = model.ChannelStereo
	got := Analyze(in, DefaultThresholds())

	if got.InputDeviceID != "usb-test-0" {
		t.Errorf("InputDeviceID = %q", got.InputDeviceID)
	}
	if got.StreamType != model.StreamVIN {
		t.Errorf("StreamType = %q", got.StreamType)
	}
	if got.SourceChannel != model.ChannelStereo {
		t.Errorf("SourceChannel = %q", got.SourceChannel)
	}
	if got.SampleRate != sr || got.Channels != 2 {
		t.Errorf("SampleRate/Channels = %d/%d", got.SampleRate, got.Channels)
	}
	if !got.Present || !got.DeviceConnected {
		t.Errorf("Present/DeviceConnected not echoed")
	}
}

// TestMeasurements pins the numeric DSP outputs for a known tone.
func TestMeasurements(t *testing.T) {
	in := base(SineWave(1000, 0.5, sr, 1, sr/10), 1)
	got := Analyze(in, DefaultThresholds())

	// RMS of a 0.5 sine = 0.5/sqrt(2) => ~ -9.03 dBFS.
	if math.Abs(got.LevelDBFS-(-9.03)) > 0.3 {
		t.Errorf("LevelDBFS = %.2f, want ~-9.03", got.LevelDBFS)
	}
	if math.Abs(got.DCOffset) > 1e-3 {
		t.Errorf("DCOffset = %.5f, want ~0", got.DCOffset)
	}
	if got.Clipping || got.Silence {
		t.Errorf("clean tone should not clip or be silent")
	}
	// A clean continuous tone must not be blamed for a high noise floor.
	if got.Status != model.QualExcellent {
		t.Errorf("clean tone Status = %q, want excellent", got.Status)
	}
}

// TestDCOffsetMeasured checks the DC value is reported, not just flagged.
func TestDCOffsetMeasured(t *testing.T) {
	in := base(sineWithDC(1000, 0.5, 0.1, sr/10), 1)
	got := Analyze(in, DefaultThresholds())
	if math.Abs(got.DCOffset-0.1) > 0.01 {
		t.Errorf("DCOffset = %.4f, want ~0.1", got.DCOffset)
	}
	if got.Status != model.QualMarginal {
		t.Errorf("Status = %q, want marginal", got.Status)
	}
}

func TestSineWave(t *testing.T) {
	frames := 100
	got := SineWave(1000, 0.8, sr, 2, frames)
	if len(got) != frames*2 {
		t.Fatalf("len = %d, want %d", len(got), frames*2)
	}
	// Channels are interleaved with identical values per frame.
	for n := 0; n < frames; n++ {
		if got[n*2] != got[n*2+1] {
			t.Fatalf("frame %d channels differ: %v vs %v", n, got[n*2], got[n*2+1])
		}
	}
	// Frame 0 is sin(0) = 0; peak magnitude stays within amplitude.
	if got[0] != 0 {
		t.Errorf("sample 0 = %v, want 0", got[0])
	}
	var peak float64
	for _, v := range got {
		if math.Abs(v) > peak {
			peak = math.Abs(v)
		}
	}
	if peak > 0.8+1e-9 {
		t.Errorf("peak %.4f exceeds amplitude 0.8", peak)
	}

	if SineWave(1000, 0.5, 0, 1, 10) != nil {
		t.Errorf("zero sample rate should return nil")
	}
	if SineWave(1000, 0.5, sr, 1, 0) != nil {
		t.Errorf("zero frames should return nil")
	}
}

// TestZeroThresholdsUseDefaults confirms a zero Thresholds behaves like the defaults.
func TestZeroThresholdsUseDefaults(t *testing.T) {
	in := base(SineWave(1000, 0.5, sr, 1, sr/10), 1)
	got := Analyze(in, Thresholds{})
	if got.Status != model.QualExcellent {
		t.Fatalf("zero thresholds Status = %q, want excellent", got.Status)
	}
}

// TestEmptySamples treats a present device with no samples as silent.
func TestEmptySamples(t *testing.T) {
	in := base(nil, 1)
	in.DuringPlayback = true
	got := Analyze(in, DefaultThresholds())
	if got.Status != model.QualFailed || !got.Silence {
		t.Fatalf("empty samples during playback: Status=%q Silence=%v", got.Status, got.Silence)
	}
}

// TestNearFullScaleIsNotClipping locks in the corrected distinction (adversarial
// review finding): a clean, unclipped sine peaking just inside the early-warning
// band must be Marginal with a HEADROOM reason and Clipping=false — never a
// misleading "audio is clipping" claim.
func TestNearFullScaleIsNotClipping(t *testing.T) {
	const sr, frames = 48000, 4800
	in := Input{
		Samples:    SineWave(1000, 0.96, sr, 1, frames), // peak ~-0.35 dBFS, no saturation
		SampleRate: sr, Channels: 1, Present: true, DeviceConnected: true, DuringPlayback: true,
	}
	got := Analyze(in, DefaultThresholds())
	if got.Clipping {
		t.Errorf("unclipped near-full-scale sine should not report Clipping")
	}
	if got.Status != model.QualMarginal {
		t.Errorf("near-full-scale should be Marginal (headroom), got %q", got.Status)
	}
	if !reasonsContain(got.Reasons, "headroom") {
		t.Errorf("expected a headroom reason, got %v", got.Reasons)
	}
	for _, r := range got.Reasons {
		if r == "audio is clipping — reduce source level or add the inline attenuator" {
			t.Errorf("must not claim clipping for an unclipped signal")
		}
	}
}

// TestSustainedSaturationClips exercises the now-live ClipSampleFraction path: a
// signal with a sustained run of truly saturated samples is detected as clipping.
func TestSustainedSaturationClips(t *testing.T) {
	const frames = 4800
	in := Input{
		Samples: square(1.0, frames), SampleRate: 48000, Channels: 1,
		Present: true, DeviceConnected: true, DuringPlayback: true,
	}
	got := Analyze(in, DefaultThresholds())
	if !got.Clipping {
		t.Errorf("sustained full-scale should clip")
	}
}
