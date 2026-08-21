package calibration

import (
	"fmt"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Severity orders recommendations for display.
type Severity string

const (
	// SeverityBlocking: the input cannot be used until this is fixed.
	SeverityBlocking Severity = "blocking"
	// SeverityImportant: usable, but it will bite during a show.
	SeverityImportant Severity = "important"
	// SeverityAdvisory: worth doing, not urgent.
	SeverityAdvisory Severity = "advisory"
)

// Recommendation is one CONCRETE fix (§6.4 step 7) — "lower the source level,
// add the attenuator pad, insert the ground-loop isolator, switch to a USB
// interface". Action is what to physically do; Why explains the measurement
// behind it, so a booth tech can tell a wiring fault from a level fault.
type Recommendation struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Action   string   `json:"action"`
	Why      string   `json:"why"`
}

// recommend derives the fix list from the measured streams and the declared
// hardware tier. Order is blocking → important → advisory, and each defect
// contributes at most once even when both streams show it.
func recommend(s Session, r Result) []Recommendation {
	var out []Recommendation
	seen := map[string]bool{}
	add := func(rec Recommendation) {
		if seen[rec.Code] {
			return
		}
		seen[rec.Code] = true
		out = append(out, rec)
	}

	// A mapping the hardware cannot satisfy blocks everything else: there is no
	// point tuning levels on a stream that cannot exist (§6.5).
	if !r.Feasible.OK {
		switch {
		case r.Feasible.Unknown:
			add(Recommendation{
				Code: "mapping_unconfirmed", Severity: SeverityImportant,
				Action: "Play the test signal and confirm both channels register on the meter before saving.",
				Why:    r.Feasible.Reason,
			})
		case r.Feasible.Detected == 1:
			add(Recommendation{
				Code: "mapping_needs_stereo", Severity: SeverityBlocking,
				Action: "Choose a mono mapping (HI only or VI-N only), or add a second USB input so one device " +
					"feeds HI and the other VI-N.",
				Why: r.Feasible.Reason,
			})
		default:
			add(Recommendation{
				Code: "mapping_impossible", Severity: SeverityBlocking,
				Action: "Choose a channel mapping this device can satisfy.",
				Why:    r.Feasible.Reason,
			})
		}
	}

	tier := s.Tier.Resolved()
	for _, st := range r.Streams {
		label := string(st.StreamType)

		if !st.DeviceConnected {
			add(Recommendation{
				Code: "device_disconnected", Severity: SeverityBlocking,
				Action: "Reconnect the input device, then re-run the wizard.",
				Why:    "the capture device is not connected",
			})
			continue
		}

		if st.Silence || (!st.Present && st.Status == model.QualFailed) {
			add(Recommendation{
				Code: "no_signal", Severity: SeverityBlocking,
				Action: "Check the cable from the cinema system's accessibility output to this input, confirm the " +
					"test signal is actually playing, and confirm the correct output is patched.",
				Why: fmt.Sprintf("%s reads no signal while a test signal should be playing", label),
			})
		}

		if st.Clipping {
			// §6.2's fix ladder: turn it down at the source first, then pad it,
			// and only then change the hardware.
			add(Recommendation{
				Code: "clipping", Severity: SeverityBlocking,
				Action: "Lower the output level at the cinema processor; if it cannot go lower, fit the inline " +
					"attenuator pad between the source and this input.",
				Why: fmt.Sprintf("%s is clipping at %.1f dBFS — clipped audio cannot be recovered downstream", label, st.LevelDBFS),
			})
		}

		if lowLevel(st, s.Tier) {
			if tier == qualifier.Tier0BuiltInJack {
				add(Recommendation{
					Code: "low_level_mic_jack", Severity: SeverityImportant,
					Action: "Move to a USB audio interface with a real line input (tier 2), or fit an attenuator " +
						"pad and use the line output.",
					Why: fmt.Sprintf("%s reads %.1f dBFS on a built-in mic-level jack; a line-level feed will "+
						"distort on it and a mic-level feed leaves no headroom", label, st.LevelDBFS),
				})
			} else {
				add(Recommendation{
					Code: "low_level", Severity: SeverityImportant,
					Action: "Raise the output level at the cinema processor, or remove an attenuator pad if one is fitted.",
					Why:    fmt.Sprintf("%s reads %.1f dBFS — low but usable; more level means more margin over the noise floor", label, st.LevelDBFS),
				})
			}
		}

		if highNoise(st, s.Tier) {
			if tier == qualifier.Tier0BuiltInJack || tier == qualifier.Tier1USBDongle {
				add(Recommendation{
					Code: "noise_floor_isolate", Severity: SeverityImportant,
					Action: "Insert a ground-loop isolator between the source and this input; if the noise persists, " +
						"move to a USB audio interface with a line input.",
					Why: fmt.Sprintf("%s has a noise floor near %.1f dBFS, typical of a shared mainboard ground or a "+
						"mic-level dongle", label, st.NoiseFloorDBFS),
				})
			} else {
				add(Recommendation{
					Code: "noise_floor", Severity: SeverityImportant,
					Action: "Check the cable run and connectors, keep the audio cable away from mains and lighting " +
						"runs, and try a ground-loop isolator.",
					Why: fmt.Sprintf("%s has a noise floor near %.1f dBFS, higher than this input tier should show",
						label, st.NoiseFloorDBFS),
				})
			}
		}

		if st.DCOffset != 0 && abs(st.DCOffset) > 0.02 {
			add(Recommendation{
				Code: "dc_offset", Severity: SeverityImportant,
				Action: "Use a line input rather than a mic input, or fit a ground-loop isolator — either removes " +
					"the DC bias the jack is putting on the signal.",
				Why: fmt.Sprintf("%s shows a DC offset of %.3f, which wastes headroom and can thump on stream start", label, st.DCOffset),
			})
		}

		if st.Dropouts > 0 {
			add(Recommendation{
				Code: "dropouts", Severity: SeverityImportant,
				Action: "Move the device to a directly-attached USB port (not a hub), and avoid sharing the port " +
					"with other high-bandwidth devices.",
				Why: fmt.Sprintf("%s dropped %d capture periods during the test", label, st.Dropouts),
			})
		}
	}

	// Identity stability is not a signal fault, but it IS a re-cabling hazard
	// worth catching at setup rather than mid-season (§6.1, §8.5).
	if s.Device.ID != "" && !s.Device.Stable {
		add(Recommendation{
			Code: "unstable_device_id", Severity: SeverityAdvisory,
			Action: "Prefer a USB device that exposes a by-id identifier, so a reboot or a re-cabling cannot " +
				"silently swap which input feeds this room.",
			Why: "this device offers no stable identifier, only its current ALSA card id",
		})
	}

	sortBySeverity(out)
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// lowLevel reports a level below the tier's mic-level threshold but above
// silence — the "low but usable" band §6.4 names in its example summary.
func lowLevel(st model.AudioStreamState, tier qualifier.Tier) bool {
	th := qualifier.ThresholdsForTier(tier)
	return !st.Silence && st.LevelDBFS < th.LowLevelDBFS && st.LevelDBFS > th.SilenceDBFS
}

// highNoise reports a noise floor that can genuinely be blamed on NOISE. The
// gap check matters here more than anywhere: §6.4 step 4 has the installer play
// a test TONE, and a continuous tone has no quiet windows, so its estimated
// floor sits right at the tone's own level. Comparing the floor to the
// threshold alone would tell every installer playing a quiet tone to go fit a
// ground-loop isolator. Blame noise only when the signal clearly rides above
// the floor, exactly as the qualifier does (§6.3).
func highNoise(st model.AudioStreamState, tier qualifier.Tier) bool {
	th := qualifier.ThresholdsForTier(tier)
	if st.Silence || st.NoiseFloorDBFS <= th.HighNoiseFloorDBFS {
		return false
	}
	return (st.LevelDBFS - st.NoiseFloorDBFS) >= th.MinDynamicRangeDB
}

func sortBySeverity(recs []Recommendation) {
	rank := map[Severity]int{SeverityBlocking: 0, SeverityImportant: 1, SeverityAdvisory: 2}
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && rank[recs[j].Severity] < rank[recs[j-1].Severity]; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

// Summary is the plain-language block §6.4 step 8 requires, in the field order
// the spec's example uses. Every field is a short human phrase, never a number
// a booth tech has to interpret.
type Summary struct {
	InputDetected   string `json:"input_detected"`
	SignalLevel     string `json:"signal_level"`
	Clipping        string `json:"clipping"`
	NoiseFloor      string `json:"noise_floor"`
	StereoChannels  string `json:"stereo_channels"`
	RecommendedMode string `json:"recommended_mode"`
	Warning         string `json:"warning,omitempty"`
	Verdict         string `json:"verdict"`
}

// Lines renders the summary as aligned "Field: value" lines, matching the
// §6.4 example block.
func (s Summary) Lines() []string {
	rows := [][2]string{
		{"Input detected", s.InputDetected},
		{"Signal level", s.SignalLevel},
		{"Clipping", s.Clipping},
		{"Noise floor", s.NoiseFloor},
		{"Stereo channels", s.StereoChannels},
		{"Recommended mode", s.RecommendedMode},
	}
	if s.Warning != "" {
		rows = append(rows, [2]string{"Warning", s.Warning})
	}
	rows = append(rows, [2]string{"Verdict", s.Verdict})

	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%-*s %s", width+1, r[0]+":", r[1]))
	}
	return out
}

// summarize turns the measurements into the §6.4 step 8 block.
func summarize(s Session, r Result) Summary {
	sum := Summary{Verdict: verdictPhrase(r.Verdict)}

	if len(r.Streams) == 0 {
		sum.InputDetected = "No"
		sum.SignalLevel = "No signal"
		sum.Clipping = "—"
		sum.NoiseFloor = "—"
		sum.StereoChannels = "Unknown"
		sum.RecommendedMode = "None — no input measured"
		return sum
	}

	// Report the worst stream: the summary must not read healthier than the
	// weakest thing it is describing.
	st := worstStream(r.Streams)

	switch {
	case !st.DeviceConnected:
		sum.InputDetected = "No — device not connected"
	case st.Silence:
		sum.InputDetected = "No — no signal on the input"
	default:
		sum.InputDetected = "Yes"
	}

	sum.SignalLevel = levelPhrase(st, s.Tier)
	if st.Clipping {
		sum.Clipping = fmt.Sprintf("Yes — peaks at %.1f dBFS", st.LevelDBFS)
	} else {
		sum.Clipping = "No"
	}
	sum.NoiseFloor = noisePhrase(st, s.Tier)
	sum.StereoChannels = stereoPhrase(r.Feasible, st)
	sum.RecommendedMode = modePhrase(s, r)

	// The single most important thing to act on becomes the Warning line.
	for _, rec := range r.Recommendations {
		if rec.Severity == SeverityBlocking || rec.Severity == SeverityImportant {
			sum.Warning = capitalize(rec.Why)
			break
		}
	}
	return sum
}

func worstStream(streams []model.AudioStreamState) model.AudioStreamState {
	out := streams[0]
	for _, s := range streams[1:] {
		if s.Status.Rank() > out.Status.Rank() {
			out = s
		}
	}
	return out
}

func levelPhrase(st model.AudioStreamState, tier qualifier.Tier) string {
	th := qualifier.ThresholdsForTier(tier)
	switch {
	case st.Silence:
		return "No signal"
	case st.Clipping:
		return fmt.Sprintf("Too hot — %.1f dBFS", st.LevelDBFS)
	case st.LevelDBFS < th.SilenceDBFS:
		return fmt.Sprintf("Far too low — %.1f dBFS", st.LevelDBFS)
	case st.LevelDBFS < th.LowLevelDBFS:
		return "Low but usable"
	case st.LevelDBFS > th.HotDBFS:
		return "Hot but usable"
	default:
		return "Within normal range"
	}
}

func noisePhrase(st model.AudioStreamState, tier qualifier.Tier) string {
	th := qualifier.ThresholdsForTier(tier)
	switch {
	case st.Silence:
		return "Not measurable — no signal"
	case !highNoise(st, tier):
		// Either the floor is genuinely low, or the signal does not ride far
		// enough above it to attribute the floor to noise at all (a continuous
		// test tone). Say the honest thing rather than claiming a clean floor.
		if st.NoiseFloorDBFS > th.HighNoiseFloorDBFS {
			return "Not measurable — a continuous tone hides the noise floor"
		}
		return "Low"
	case st.NoiseFloorDBFS > th.HighNoiseFloorDBFS+10:
		return "High"
	default:
		return "Moderate"
	}
}

func stereoPhrase(f Feasibility, st model.AudioStreamState) string {
	switch {
	case f.Unknown || st.Channels == 0:
		return "Unknown — the device reports no channel count until it is opened"
	case st.Channels >= 2:
		return "Yes"
	default:
		return "No — mono only"
	}
}

// modePhrase recommends what this input can actually serve, which is the line a
// booth tech acts on when the requested mapping does not fit the hardware.
func modePhrase(s Session, r Result) string {
	if r.Verdict == model.QualFailed && !r.Feasible.OK {
		if r.Feasible.Detected == 1 {
			return "VI-N only or HI only — this input is mono"
		}
		return "None — the selected mapping cannot be satisfied"
	}
	if r.Verdict == model.QualFailed {
		return "None until the fault above is corrected"
	}
	label := string(s.Mode)
	if s.Mode == "" {
		label = "not selected"
	}
	if r.Verdict == model.QualMarginal {
		return label + " — usable with the warning above"
	}
	return label
}

func verdictPhrase(q model.QualifierState) string {
	switch q {
	case model.QualExcellent:
		return "Excellent — ready for a show"
	case model.QualUsable:
		return "Usable — ready for a show"
	case model.QualMarginal:
		return "Marginal — usable, but fix the warning before relying on it"
	default:
		return "Failed — do not offer this stream to patrons"
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ModeLabel is the human label for a mapping in the admin selector (§6.5).
func ModeLabel(m mapper.Mode) string {
	switch m {
	case mapper.MonoHI:
		return "Mono input → HI"
	case mapper.MonoVIN:
		return "Mono input → VI-N"
	case mapper.StereoLeftHIRightVIN:
		return "Stereo: Left = HI, Right = VI-N"
	case mapper.StereoLeftVINRightHI:
		return "Stereo: Left = VI-N, Right = HI"
	case mapper.SummedMonoHI:
		return "Stereo summed to mono → HI"
	case mapper.SummedMonoVIN:
		return "Stereo summed to mono → VI-N"
	case mapper.StereoPairHI:
		return "Stereo pair as one stream → HI"
	case mapper.StereoPairVIN:
		return "Stereo pair as one stream → VI-N"
	case mapper.MultiDevice:
		return "Two devices: Device A = HI, Device B = VI-N"
	default:
		return string(m)
	}
}
