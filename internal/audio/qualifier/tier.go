package qualifier

import "fmt"

// Tier is the §6.2 input tier of the hardware feeding one input. The capture
// layer is written once; the tier changes only how HARSHLY the qualifier judges
// what it captures, because the four tiers have very different noise, DC-bias
// and overload behaviour.
//
// Tier is a string so it round-trips through config and the §6.4 wizard, and so
// the zero value means "unset" rather than accidentally selecting the strictest
// tier. An unset tier resolves to DefaultTier.
type Tier string

const (
	// Tier0BuiltInJack — built-in mini-PC mic/line jack. §6.2: "Highest
	// variability (mono, TRRS, electret bias, AGC, easily overloaded by line
	// level). First-class supported, but the qualifier must aggressively warn."
	Tier0BuiltInJack Tier = "tier0_builtin_jack"
	// Tier1USBDongle — cheap USB audio dongle: isolated from mainboard audio
	// quirks, still consumer-grade.
	Tier1USBDongle Tier = "tier1_usb_dongle"
	// Tier2USBInterface — USB audio interface with a real line input. §6.2 marks
	// this "◄ default shipped input".
	Tier2USBInterface Tier = "tier2_usb_interface"
	// Tier3Balanced — balanced / AES3 / Dante / AES67. Premium/future; the same
	// capture abstraction, never required.
	Tier3Balanced Tier = "tier3_balanced"
)

// DefaultTier is the §6.2 default shipped input (tier 2). An unset Tier resolves
// here, so a caller that says nothing gets the shipped behaviour, not the
// strictest or the loosest one.
const DefaultTier = Tier2USBInterface

// tierLabels are the plain-language names the §6.4 wizard shows.
var tierLabels = map[Tier]string{
	Tier0BuiltInJack:  "Tier 0 — built-in mic/line jack",
	Tier1USBDongle:    "Tier 1 — USB audio dongle",
	Tier2USBInterface: "Tier 2 — USB audio interface with line input (default)",
	Tier3Balanced:     "Tier 3 — balanced / AES3 / Dante / AES67",
}

// tierAliases accepts the short forms an installer or a CLI flag is likely to
// use ("0".."3") alongside the canonical tokens.
var tierAliases = map[string]Tier{
	"0": Tier0BuiltInJack, "tier0": Tier0BuiltInJack,
	"1": Tier1USBDongle, "tier1": Tier1USBDongle,
	"2": Tier2USBInterface, "tier2": Tier2USBInterface,
	"3": Tier3Balanced, "tier3": Tier3Balanced,
}

// Resolved returns t, or DefaultTier when t is unset.
func (t Tier) Resolved() Tier {
	if t == "" {
		return DefaultTier
	}
	return t
}

// Label returns the plain-language tier name for the admin UI.
func (t Tier) Label() string {
	if l, ok := tierLabels[t.Resolved()]; ok {
		return l
	}
	return string(t)
}

// String returns the canonical config token.
func (t Tier) String() string { return string(t.Resolved()) }

// ParseTier parses a tier token ("tier0_builtin_jack", "tier1", "2", …). An
// empty string resolves to DefaultTier; an unrecognized one is an error rather
// than a silent fallback (guardrail #7).
func ParseTier(s string) (Tier, error) {
	if s == "" {
		return DefaultTier, nil
	}
	if _, ok := tierLabels[Tier(s)]; ok {
		return Tier(s), nil
	}
	if t, ok := tierAliases[s]; ok {
		return t, nil
	}
	return "", fmt.Errorf("qualifier: unrecognized input tier %q (§6.2: tier0_builtin_jack|tier1_usb_dongle|tier2_usb_interface|tier3_balanced)", s)
}

// Tiers lists the four tiers in §6.2 order, for the wizard's device-tier picker.
func Tiers() []Tier {
	return []Tier{Tier0BuiltInJack, Tier1USBDongle, Tier2USBInterface, Tier3Balanced}
}

// ThresholdsForTier returns the qualifier profile for one §6.2 tier. Tier 2 is
// DefaultThresholds; the others move the early-warning knobs to match what the
// hardware realistically does:
//
//   - Tier 0 warns EARLIEST on every axis, as §6.2 requires. A built-in jack
//     carries electret bias (so a much smaller DC offset is already suspicious),
//     has a higher noise floor, applies AGC, and is easily overloaded by a
//     line-level cinema output — so overload, mic-level and noise all trip
//     sooner and the "pristine" band is narrower.
//   - Tier 1 sits between the two: isolated from mainboard quirks, still
//     consumer-grade.
//   - Tier 3 is a professional balanced/AES feed, so a noise floor or DC offset
//     a dongle would be forgiven for is itself worth reporting.
//
// The tier NUMBERS are engineering judgements, not measurements. Calibrating
// them against real jacks, dongles and interfaces is Phase 0C work and is
// hardware-gated (BLOCKERS B3); what is shipped here is the mechanism plus
// defensible starting values, and nothing claims field validation.
func ThresholdsForTier(t Tier) Thresholds {
	th := DefaultThresholds()
	switch t.Resolved() {
	case Tier0BuiltInJack:
		th.ClipDBFS = -1.5
		th.ClipSampleFraction = 0.002
		th.LowLevelDBFS = -34
		th.HighNoiseFloorDBFS = -55
		th.DCOffsetAbs = 0.005
		th.ExcellentMinDBFS = -24
		th.HotDBFS = -9
	case Tier1USBDongle:
		th.ClipDBFS = -1.0
		th.ClipSampleFraction = 0.005
		th.LowLevelDBFS = -37
		th.HighNoiseFloorDBFS = -50
		th.DCOffsetAbs = 0.01
	case Tier3Balanced:
		th.HighNoiseFloorDBFS = -60
		th.DCOffsetAbs = 0.005
	}
	return th
}
