package qualifier

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// TestParseTier covers the §6.2 tier tokens and the short forms an installer or
// a CLI flag is likely to use. An unrecognized tier is an error, not a silent
// fallback to the shipped profile.
func TestParseTier(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Tier
		wantErr bool
	}{
		{name: "empty is the default shipped input", in: "", want: Tier2USBInterface},
		{name: "canonical token", in: "tier0_builtin_jack", want: Tier0BuiltInJack},
		{name: "short form", in: "1", want: Tier1USBDongle},
		{name: "prefixed short form", in: "tier3", want: Tier3Balanced},
		{name: "unknown is rejected", in: "tier9", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTier(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTier(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTier(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseTier(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got.Label() == "" || got.Label() == string(got) {
				t.Errorf("tier %q has no plain-language label for the wizard", got)
			}
		})
	}
	if len(Tiers()) != 4 {
		t.Errorf("Tiers() should list the four §6.2 tiers, got %d", len(Tiers()))
	}
}

// TestTier2IsTheShippedDefault asserts the unset tier resolves to the profile the
// appliance ships with, so a caller that says nothing is not silently switched to
// the strictest or the loosest judgement.
func TestTier2IsTheShippedDefault(t *testing.T) {
	if ThresholdsForTier("") != DefaultThresholds() {
		t.Fatalf("an unset tier must resolve to DefaultThresholds")
	}
	if ThresholdsForTier(Tier2USBInterface) != DefaultThresholds() {
		t.Fatalf("tier 2 is the default shipped input (§6.2)")
	}
}

// TestTierZeroWarnsEarlier is the §6.2 requirement in one assertion: the same
// captured signal must be judged more harshly on a built-in jack ("the qualifier
// must aggressively warn") than on the shipped USB interface.
func TestTierZeroWarnsEarlier(t *testing.T) {
	const frames = sr / 10

	tests := []struct {
		name        string
		samples     []float64
		tier0Want   model.QualifierState
		tier2Want   model.QualifierState
		tier0Sub    string
		description string
	}{
		{
			name:      "electret bias offset passes on an interface, warns on a jack",
			samples:   sineWithDC(1000, 0.4, 0.012, frames),
			tier0Want: model.QualMarginal, tier2Want: model.QualExcellent,
			tier0Sub:    "DC offset",
			description: "a built-in jack biases its electret input, so a much smaller offset is suspicious",
		},
		{
			name:      "a hot level is flagged sooner on a jack",
			samples:   SineWave(1000, 0.6, sr, 1, frames),
			tier0Want: model.QualUsable, tier2Want: model.QualExcellent,
			tier0Sub:    "level is hot",
			description: "a jack overloads well before full scale",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base(tc.samples, 1)
			in.DuringPlayback = true

			tier0 := Analyze(in, ThresholdsForTier(Tier0BuiltInJack))
			tier2 := Analyze(in, ThresholdsForTier(Tier2USBInterface))

			if tier0.Status != tc.tier0Want {
				t.Fatalf("tier 0 Status = %q, want %q (%s); reasons=%v", tier0.Status, tc.tier0Want, tc.description, tier0.Reasons)
			}
			if tier2.Status != tc.tier2Want {
				t.Fatalf("tier 2 Status = %q, want %q; reasons=%v", tier2.Status, tc.tier2Want, tier2.Reasons)
			}
			if tier0.Status.Rank() <= tier2.Status.Rank() {
				t.Errorf("tier 0 must be stricter than tier 2 on the same fixture (%q vs %q)", tier0.Status, tier2.Status)
			}
			if !reasonsContain(tier0.Reasons, tc.tier0Sub) {
				t.Errorf("tier 0 reasons %v do not contain %q", tier0.Reasons, tc.tier0Sub)
			}
		})
	}
}

// TestTierOverridesLayerOnTheProfile confirms a caller can tune one knob without
// discarding the rest of the tier profile.
func TestTierOverridesLayerOnTheProfile(t *testing.T) {
	base := ThresholdsForTier(Tier0BuiltInJack)
	got := Thresholds{LowLevelDBFS: -50}.WithDefaults(base)
	if got.LowLevelDBFS != -50 {
		t.Errorf("override lost: LowLevelDBFS = %v", got.LowLevelDBFS)
	}
	if got.DCOffsetAbs != base.DCOffsetAbs {
		t.Errorf("DCOffsetAbs = %v, want the tier value %v", got.DCOffsetAbs, base.DCOffsetAbs)
	}
}
