package mapper

import (
	"math"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// allModes is every mode defined in §6.5, used for exhaustive round-trip tests.
var allModes = []Mode{
	MonoHI, MonoVIN,
	StereoLeftHIRightVIN, StereoLeftVINRightHI,
	SummedMonoHI, SummedMonoVIN,
	StereoPairHI, StereoPairVIN,
	MultiDevice,
}

func mapsEqual(a, b []StreamMap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlanForModes(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		channels int
		want     []StreamMap
		wantErr  bool
	}{
		{"mono HI", MonoHI, 1, []StreamMap{{model.StreamHI, model.ChannelMono}}, false},
		{"mono VI-N", MonoVIN, 1, []StreamMap{{model.StreamVIN, model.ChannelMono}}, false},
		{"stereo L=HI R=VIN", StereoLeftHIRightVIN, 2,
			[]StreamMap{{model.StreamHI, model.ChannelLeft}, {model.StreamVIN, model.ChannelRight}}, false},
		{"stereo L=VIN R=HI", StereoLeftVINRightHI, 2,
			[]StreamMap{{model.StreamVIN, model.ChannelLeft}, {model.StreamHI, model.ChannelRight}}, false},
		{"summed HI", SummedMonoHI, 2, []StreamMap{{model.StreamHI, model.ChannelSummed}}, false},
		{"summed VI-N", SummedMonoVIN, 2, []StreamMap{{model.StreamVIN, model.ChannelSummed}}, false},
		{"stereo-pair HI", StereoPairHI, 2, []StreamMap{{model.StreamHI, model.ChannelStereo}}, false},
		{"stereo-pair VI-N", StereoPairVIN, 2, []StreamMap{{model.StreamVIN, model.ChannelStereo}}, false},
		{"multi-device", MultiDevice, 1,
			[]StreamMap{{model.StreamHI, model.ChannelMono}, {model.StreamVIN, model.ChannelMono}}, false},
		{"multi-device on stereo still ok", MultiDevice, 2,
			[]StreamMap{{model.StreamHI, model.ChannelMono}, {model.StreamVIN, model.ChannelMono}}, false},

		// §6.5 stereo-on-mono => Failed before show.
		{"L/R split on mono errors", StereoLeftHIRightVIN, 1, nil, true},
		{"reverse L/R split on mono errors", StereoLeftVINRightHI, 1, nil, true},
		{"summed on mono errors", SummedMonoHI, 1, nil, true},
		{"stereo-pair on mono errors", StereoPairVIN, 1, nil, true},

		// Boundary / misuse.
		{"mono mode wants exactly 1 channel", MonoHI, 2, nil, true},
		{"zero channels errors", MonoHI, 0, nil, true},
		{"negative channels errors", StereoPairHI, -1, nil, true},
		{"multi-device needs an input", MultiDevice, 0, nil, true},
		{"unknown mode errors", Mode("bogus"), 2, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PlanFor(tt.mode, tt.channels)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PlanFor(%q, %d) = %+v, want error", tt.mode, tt.channels, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanFor(%q, %d) unexpected error: %v", tt.mode, tt.channels, err)
			}
			if !mapsEqual(got.Maps, tt.want) {
				t.Fatalf("PlanFor(%q, %d) maps = %+v, want %+v", tt.mode, tt.channels, got.Maps, tt.want)
			}
		})
	}
}

// TestPlanForReturnsCopy verifies the returned Plan does not alias modeMaps.
func TestPlanForReturnsCopy(t *testing.T) {
	p, err := PlanFor(StereoLeftHIRightVIN, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p.Maps[0] = StreamMap{model.StreamVIN, model.ChannelRight} // mutate the caller copy
	p2, err := PlanFor(StereoLeftHIRightVIN, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p2.Maps[0] != (StreamMap{model.StreamHI, model.ChannelLeft}) {
		t.Fatalf("modeMaps table was mutated through returned Plan: %+v", p2.Maps[0])
	}
}

func TestParseModeRoundTrip(t *testing.T) {
	for _, m := range allModes {
		got, err := ParseMode(m.String())
		if err != nil {
			t.Fatalf("ParseMode(%q) error: %v", m.String(), err)
		}
		if got != m {
			t.Fatalf("ParseMode(%q) = %q, want %q", m.String(), got, m)
		}
	}
}

func TestParseModeCanonicalStrings(t *testing.T) {
	tests := []struct {
		in   string
		want Mode
	}{
		{"stereo:L=HI,R=VI-N", StereoLeftHIRightVIN},
		{"stereo:L=VI-N,R=HI", StereoLeftVINRightHI},
		{"  mono:HI  ", MonoHI}, // surrounding whitespace tolerated
		{"summed:VI-N", SummedMonoVIN},
		{"stereo-pair:HI", StereoPairHI},
		{"multi-device", MultiDevice},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.in)
		if err != nil {
			t.Fatalf("ParseMode(%q) error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("ParseMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseModeRejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "stereo", "stereo:L=HI", "left=HI", "HI"} {
		if got, err := ParseMode(in); err == nil {
			t.Fatalf("ParseMode(%q) = %q, want error", in, got)
		}
	}
}

func floatsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-12 {
			return false
		}
	}
	return true
}

func TestExtract(t *testing.T) {
	// Stereo, 3 frames: L = {1,3,5}, R = {2,4,6}.
	stereo := []float64{1, 2, 3, 4, 5, 6}
	mono := []float64{0.5, -0.5, 0.25}

	tests := []struct {
		name     string
		source   model.SourceChannel
		in       []float64
		channels int
		want     []float64
		wantErr  bool
	}{
		{"left picks lane 0", model.ChannelLeft, stereo, 2, []float64{1, 3, 5}, false},
		{"right picks lane 1", model.ChannelRight, stereo, 2, []float64{2, 4, 6}, false},
		{"summed averages L+R", model.ChannelSummed, stereo, 2, []float64{1.5, 3.5, 5.5}, false},
		{"stereo passes the pair through", model.ChannelStereo, stereo, 2, []float64{1, 2, 3, 4, 5, 6}, false},
		{"mono passes through", model.ChannelMono, mono, 1, []float64{0.5, -0.5, 0.25}, false},

		// Source impossible for the channel count.
		{"left on mono errors", model.ChannelLeft, mono, 1, nil, true},
		{"summed on mono errors", model.ChannelSummed, mono, 1, nil, true},
		{"stereo on mono errors", model.ChannelStereo, mono, 1, nil, true},
		{"mono on stereo errors", model.ChannelMono, stereo, 2, nil, true},

		// Malformed buffers / bad args.
		{"non-multiple length errors", model.ChannelLeft, []float64{1, 2, 3}, 2, nil, true},
		{"zero channels errors", model.ChannelMono, mono, 0, nil, true},
		{"unknown source errors", model.SourceChannel("weird"), stereo, 2, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Extract(tt.source, tt.in, tt.channels)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Extract(%q, ..., %d) = %v, want error", tt.source, tt.channels, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract(%q, ..., %d) unexpected error: %v", tt.source, tt.channels, err)
			}
			if !floatsEqual(got, tt.want) {
				t.Fatalf("Extract(%q, ..., %d) = %v, want %v", tt.source, tt.channels, got, tt.want)
			}
		})
	}
}

// TestExtractStereoMultilane confirms a >2-lane buffer yields only the L/R pair.
func TestExtractStereoMultilane(t *testing.T) {
	// 4 channels (L,R,C,LFE), 2 frames. Only the first two lanes are taken.
	in := []float64{1, 2, 9, 9, 3, 4, 9, 9}
	got, err := Extract(model.ChannelStereo, in, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !floatsEqual(got, []float64{1, 2, 3, 4}) {
		t.Fatalf("stereo from 4ch = %v, want [1 2 3 4]", got)
	}
}

// TestExtractDoesNotAliasInput ensures the output is a fresh buffer.
func TestExtractDoesNotAliasInput(t *testing.T) {
	in := []float64{1, -1, 2, -2}
	out, err := Extract(model.ChannelMono, []float64{1, 2, 3}, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out[0] = 999
	if in[0] != 1 {
		t.Fatalf("input buffer was mutated")
	}
}

// TestModeStringValues locks the canonical wire strings (§6.5).
func TestModeStringValues(t *testing.T) {
	want := map[Mode]string{
		MonoHI:               "mono:HI",
		MonoVIN:              "mono:VI-N",
		StereoLeftHIRightVIN: "stereo:L=HI,R=VI-N",
		StereoLeftVINRightHI: "stereo:L=VI-N,R=HI",
		SummedMonoHI:         "summed:HI",
		SummedMonoVIN:        "summed:VI-N",
		StereoPairHI:         "stereo-pair:HI",
		StereoPairVIN:        "stereo-pair:VI-N",
		MultiDevice:          "multi-device",
	}
	for m, s := range want {
		if m.String() != s {
			t.Fatalf("%v.String() = %q, want %q", m, m.String(), s)
		}
	}
}
