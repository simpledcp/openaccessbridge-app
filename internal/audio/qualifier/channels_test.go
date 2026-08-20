package qualifier

import (
	"math"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// interleave builds a 2-channel buffer from two mono lanes.
func interleave(left, right []float64) []float64 {
	out := make([]float64, len(left)*2)
	for i := range left {
		out[2*i] = left[i]
		out[2*i+1] = right[i]
	}
	return out
}

// TestDeviceChannelsDrivesCapability pins §6.5: Failed is reserved for a mapping
// "the detected hardware cannot satisfy". A legal stereo mapping necessarily
// hands the qualifier a ONE-channel lane, so judging capability from the lane
// width failed every legal stereo mode; capability is judged from the DEVICE.
func TestDeviceChannelsDrivesCapability(t *testing.T) {
	const frames = sr / 10

	tests := []struct {
		name           string
		lane           []float64
		laneChannels   int
		deviceChannels int
		required       int
		wantFailed     bool
		wantChannels   int
	}{
		{
			name:         "stereo split hands over a mono lane from a stereo device",
			lane:         SineWave(1000, 0.5, sr, 1, frames),
			laneChannels: 1, deviceChannels: 2, required: 2,
			wantFailed: false, wantChannels: 2,
		},
		{
			name:         "summed mono from a stereo device",
			lane:         SineWave(1000, 0.5, sr, 1, frames),
			laneChannels: 1, deviceChannels: 2, required: 2,
			wantFailed: false, wantChannels: 2,
		},
		{
			name:         "stereo pair keeps both lanes",
			lane:         SineWave(1000, 0.5, sr, 2, frames),
			laneChannels: 2, deviceChannels: 2, required: 2,
			wantFailed: false, wantChannels: 2,
		},
		{
			name:         "stereo mapping on a mono device is impossible",
			lane:         SineWave(1000, 0.5, sr, 1, frames),
			laneChannels: 1, deviceChannels: 1, required: 2,
			wantFailed: true, wantChannels: 1,
		},
		{
			name:         "mono mapping on a mono device",
			lane:         SineWave(1000, 0.5, sr, 1, frames),
			laneChannels: 1, deviceChannels: 1, required: 0,
			wantFailed: false, wantChannels: 1,
		},
		{
			name:         "unset device channels falls back to the lane width",
			lane:         SineWave(1000, 0.5, sr, 1, frames),
			laneChannels: 1, deviceChannels: 0, required: 2,
			wantFailed: true, wantChannels: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base(tc.lane, tc.laneChannels)
			in.DeviceChannels = tc.deviceChannels
			in.RequiredChannels = tc.required
			in.DuringPlayback = true
			got := Analyze(in, DefaultThresholds())

			failed := got.Status == model.QualFailed
			if failed != tc.wantFailed {
				t.Fatalf("Status = %q (failed=%v), want failed=%v; reasons=%v", got.Status, failed, tc.wantFailed, got.Reasons)
			}
			if got.Channels != tc.wantChannels {
				t.Errorf("Channels = %d, want %d — the dashboard's mono-vs-stereo report is the DEVICE count (§6.3)",
					got.Channels, tc.wantChannels)
			}
			if tc.wantFailed && !reasonsContain(got.Reasons, "requires two channels") {
				t.Errorf("reasons %v should explain the impossible mapping", got.Reasons)
			}
		})
	}
}

// TestStereoPairLaneIsMeasuredAsTwoChannels catches the mis-windowing of the
// §6.5 stereo-pair mode: the mapper hands over a 2-channel interleaved lane, so
// measuring it as mono both halves every analysis window and averages the two
// lanes together sample-by-sample instead of frame-by-frame.
//
// Signal: left is a 0.5 sine, right is digital silence.
//
//	de-interleaved correctly → per-frame mean 0.25·sin → RMS ≈ -15.05 dBFS
//	measured as one mono lane → alternating 0.5·sin / 0 → RMS ≈ -12.04 dBFS
func TestStereoPairLaneIsMeasuredAsTwoChannels(t *testing.T) {
	const frames = sr / 10
	lane := interleave(SineWave(1000, 0.5, sr, 1, frames), make([]float64, frames))

	in := base(lane, 2)
	in.DeviceChannels = 2
	in.SourceChannel = model.ChannelStereo
	in.DuringPlayback = true
	got := Analyze(in, DefaultThresholds())

	if math.Abs(got.LevelDBFS-(-15.05)) > 0.5 {
		t.Fatalf("LevelDBFS = %.2f, want ~-15.05 (a mono reading of the same buffer gives ~-12.0)", got.LevelDBFS)
	}
}
