package qualifier

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// quietLine is a live input carrying only a low-level tone: RMS is below the
// silence threshold, but the noise floor is well above digital zero, so §6.3's
// noise-floor context says "live line, nothing on it" rather than "no signal".
func quietLine(frames int) []float64 { return SineWave(1000, 0.0005, sr, 1, frames) } // ~-69 dBFS

// deadLine is digital zero: no signal at all, the floor collapsed to MinDBFS.
func deadLine(frames int) []float64 { return make([]float64, frames) }

// TestSustainedSilenceNeedsDuration is the regression test for the mid-show
// false alarm: one quiet 200 ms analysis block used to demote a stream straight
// to Failed, which readiness turns into a CRITICAL "input is silent during
// playback" warning. §6.3 requires silence to be judged "via duration +
// noise-floor context".
func TestSustainedSilenceNeedsDuration(t *testing.T) {
	const frames = sr / 5 // 200 ms, the pipeline's default analysis window

	tests := []struct {
		name           string
		stream         model.StreamType
		samples        []float64
		duringPlayback bool
		silentForMS    int
		wantStatus     model.QualifierState
		reasonSub      string
	}{
		{
			name:    "one quiet block mid-show is not a fault",
			samples: deadLine(frames), duringPlayback: true, silentForMS: 200,
			wantStatus: model.QualUsable, reasonSub: "watching for sustained silence",
		},
		{
			name:    "a dead input fails once the silence is sustained",
			samples: deadLine(frames), duringPlayback: true, silentForMS: 2000,
			wantStatus: model.QualFailed, reasonSub: "no signal at all",
		},
		{
			name:    "a live but quiet line gets far longer than a dead one",
			samples: quietLine(frames), duringPlayback: true, silentForMS: 2000,
			wantStatus: model.QualUsable, reasonSub: "watching for sustained silence",
		},
		{
			name:    "a live HI line with nothing on it eventually fails too",
			samples: quietLine(frames), duringPlayback: true, silentForMS: 30000,
			wantStatus: model.QualFailed, reasonSub: "the line is live but carries no programme audio",
		},
		{
			// VI-N narration is intermittent by design, so a live line between
			// descriptions is reported, not failed.
			name:    "a live VI-N line between descriptions is reported, not failed",
			stream:  model.StreamVIN,
			samples: quietLine(frames), duringPlayback: true, silentForMS: 30000,
			wantStatus: model.QualMarginal, reasonSub: "gap between descriptions",
		},
		{
			// A VI-N line with NO signal at all is still §6.3's Failed example.
			name:    "a dead VI-N line still fails",
			stream:  model.StreamVIN,
			samples: deadLine(frames), duringPlayback: true, silentForMS: 2000,
			wantStatus: model.QualFailed, reasonSub: "no signal at all",
		},
		{
			name:    "an untracked one-shot block is judged on its own (calibration)",
			samples: deadLine(frames), duringPlayback: true, silentForMS: 0,
			wantStatus: model.QualFailed, reasonSub: "silent during playback",
		},
		{
			name:    "silence before playback is still just marginal",
			samples: deadLine(frames), duringPlayback: false, silentForMS: 60000,
			wantStatus: model.QualMarginal, reasonSub: "play your source",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base(tc.samples, 1)
			if tc.stream != "" {
				in.StreamType = tc.stream
			}
			in.DuringPlayback = tc.duringPlayback
			in.SilentForMS = tc.silentForMS
			got := Analyze(in, DefaultThresholds())

			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (reasons=%v)", got.Status, tc.wantStatus, got.Reasons)
			}
			if !got.Silence {
				t.Errorf("Silence should be reported for a silent block regardless of the verdict")
			}
			if !reasonsContain(got.Reasons, tc.reasonSub) {
				t.Errorf("reasons %v do not contain %q", got.Reasons, tc.reasonSub)
			}
		})
	}
}

// TestSilenceHysteresis pins the re-arm band: a level sitting on the silence
// threshold must not flip the verdict block by block. Once silent, the input has
// to climb SilenceRearmDB above the threshold before it counts as live again.
func TestSilenceHysteresis(t *testing.T) {
	const frames = sr / 5
	// A sine whose RMS is ~-58 dBFS: above the -60 threshold, below the -57
	// re-arm level.
	inBand := SineWave(1000, 0.00178, sr, 1, frames)

	tests := []struct {
		name       string
		prevSilent bool
		want       bool
	}{
		{name: "a live input at -58 dBFS is not silent", prevSilent: false, want: false},
		{name: "an already-silent input stays silent inside the re-arm band", prevSilent: true, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base(inBand, 1)
			in.DuringPlayback = true
			in.PrevSilent = tc.prevSilent
			in.SilentForMS = 200
			got := Analyze(in, DefaultThresholds())
			if got.Silence != tc.want {
				t.Fatalf("Silence = %v, want %v (level %.2f dBFS)", got.Silence, tc.want, got.LevelDBFS)
			}
		})
	}
}

// TestSilenceThresholdsAreOverridable confirms an installation can shorten or
// lengthen the sustained-silence windows without restating the whole profile.
func TestSilenceThresholdsAreOverridable(t *testing.T) {
	th := Thresholds{SustainedSilenceMS: 400}.WithDefaults(DefaultThresholds())
	if th.SustainedSilenceMS != 400 {
		t.Fatalf("SustainedSilenceMS = %d, want 400", th.SustainedSilenceMS)
	}
	if th.QuietPassageMS != DefaultThresholds().QuietPassageMS {
		t.Errorf("unset knobs must keep the base value")
	}

	in := base(deadLine(sr/5), 1)
	in.DuringPlayback = true
	in.SilentForMS = 600
	if got := Analyze(in, th); got.Status != model.QualFailed {
		t.Fatalf("Status = %q, want failed after 600 ms with a 400 ms threshold", got.Status)
	}
}
