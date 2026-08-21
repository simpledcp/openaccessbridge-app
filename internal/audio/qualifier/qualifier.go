// Package qualifier is the four-state audio input qualification engine
// (buildplan.md §6.3). It actively measures a block of captured PCM and emits one
// of Excellent / Usable / Marginal / Failed plus plain-language reasons — it never
// merely accepts whatever it is handed (guardrail #7). It is PURE DSP: no hardware,
// no ALSA, no logging of audio content. The same routine runs during the
// calibration wizard and continuously during playback.
//
// Measurements (§6.3): peak and RMS dBFS, sample-saturation clipping, sustained
// silence, a low-percentile noise-floor estimate, DC offset, and mono-vs-stereo
// capability. Classification is conservative — any one defect demotes the
// verdict — so a degraded input is surfaced before showtime rather than failing
// silently during a show.
//
// Sustained silence is a property of TIME, not of one block, so the caller
// tracks how long the input has been continuously silent and hands it in
// (Input.SilentForMS / Input.PrevSilent); the qualifier applies the §6.3
// duration and noise-floor rules to it. It never invents history it lacks.
package qualifier

import (
	"math"
	"sort"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// MinDBFS is the floor reported for digital silence so dB arithmetic stays
// finite. Callers use it as the "no measurement" value for level/floor fields so
// a dead input never renders as 0 dBFS — which would read as FULL SCALE.
const MinDBFS = -120.0

// saturationDBFS is the level at/above which a sample is counted as truly saturated
// (essentially full scale). It is deliberately distinct from the early-warning
// peak threshold (Thresholds.ClipDBFS, ~-0.5 dBFS): real clipping is sustained
// saturation near 0 dBFS, while a peak merely inside the early-warning band is a
// headroom warning, not clipping.
const saturationDBFS = -0.1

// Thresholds parameterizes the qualifier. All levels are dBFS (0 = full scale).
// A zero-valued field is treated as its default (see DefaultThresholds), so callers
// may override only the knobs they care about.
type Thresholds struct {
	// Required §6.3 thresholds.
	ClipDBFS           float64 // peak at/above this is overload (also see ClipSampleFraction).
	SilenceDBFS        float64 // RMS below this is silence / no signal.
	LowLevelDBFS       float64 // RMS below this (but above silence) is mic-level / too low.
	HighNoiseFloorDBFS float64 // estimated noise floor above this is "high" (when signal rides above it).
	DCOffsetAbs        float64 // |mean sample| above this flags a DC offset.

	// Sustained-silence knobs (§6.3 "duration + noise-floor context"). Silence
	// during playback is never judged from a single analysis block.
	SustainedSilenceMS int     // continuous silence before a DEAD input (no measurable floor) fails.
	QuietPassageMS     int     // continuous silence before a LIVE-but-quiet input fails.
	DeadInputFloorDBFS float64 // noise floor at/below this is digital zero: no signal at all.
	SilenceRearmDB     float64 // hysteresis: dB above SilenceDBFS needed to clear a silent state.

	// Supporting knobs.
	ClipSampleFraction float64 // fraction of near-full-scale samples that also counts as clipping.
	ExcellentMinDBFS   float64 // lower bound of the pristine RMS band.
	HotDBFS            float64 // RMS above this (without clipping) is hot-but-usable.
	NoiseFloorPct      float64 // percentile of short-window RMS used as the noise-floor estimate.
	NoiseWindowMS      int     // short-window length for the noise-floor estimate.
	MinDynamicRangeDB  float64 // signal-minus-floor gap required before a high floor is blamed on noise.
}

// DefaultThresholds returns the defaults for a consumer USB/line input — §6.2
// tier 2, the default shipped input. They err toward warning early: overload
// within 0.5 dB of full scale, silence below −60 dBFS, mic-level below −40 dBFS.
// Silence must persist for 2 s (a dead input, no measurable floor) or 30 s (a
// live line that is merely quiet) before it is reported as a fault — never on
// the strength of a single analysis block.
//
// These are engineering defaults, not measurements: calibrating them against
// real dongles/jacks/interfaces is hardware-gated (BLOCKERS B3). Per-tier
// profiles are in ThresholdsForTier (tier.go).
func DefaultThresholds() Thresholds {
	return Thresholds{
		ClipDBFS:           -0.5,
		SilenceDBFS:        -60,
		LowLevelDBFS:       -40,
		HighNoiseFloorDBFS: -45,
		DCOffsetAbs:        0.02,
		SustainedSilenceMS: 2000,
		QuietPassageMS:     30000,
		DeadInputFloorDBFS: -90,
		SilenceRearmDB:     3,
		ClipSampleFraction: 0.01,
		ExcellentMinDBFS:   -30,
		HotDBFS:            -6,
		NoiseFloorPct:      0.10,
		NoiseWindowMS:      10,
		MinDynamicRangeDB:  12,
	}
}

// normalized fills any zero-valued field from the defaults so callers can pass a
// partial Thresholds (or the zero value) and still get sane behavior.
func (t Thresholds) normalized() Thresholds { return t.WithDefaults(DefaultThresholds()) }

// WithDefaults fills any zero-valued field of t from base, so a caller can
// override only the knobs it cares about on top of a per-tier profile (§6.2)
// instead of restating the whole struct.
func (t Thresholds) WithDefaults(base Thresholds) Thresholds {
	d := base
	if t.ClipDBFS == 0 {
		t.ClipDBFS = d.ClipDBFS
	}
	if t.SilenceDBFS == 0 {
		t.SilenceDBFS = d.SilenceDBFS
	}
	if t.LowLevelDBFS == 0 {
		t.LowLevelDBFS = d.LowLevelDBFS
	}
	if t.HighNoiseFloorDBFS == 0 {
		t.HighNoiseFloorDBFS = d.HighNoiseFloorDBFS
	}
	if t.DCOffsetAbs == 0 {
		t.DCOffsetAbs = d.DCOffsetAbs
	}
	if t.ClipSampleFraction == 0 {
		t.ClipSampleFraction = d.ClipSampleFraction
	}
	if t.ExcellentMinDBFS == 0 {
		t.ExcellentMinDBFS = d.ExcellentMinDBFS
	}
	if t.HotDBFS == 0 {
		t.HotDBFS = d.HotDBFS
	}
	if t.NoiseFloorPct == 0 {
		t.NoiseFloorPct = d.NoiseFloorPct
	}
	if t.NoiseWindowMS == 0 {
		t.NoiseWindowMS = d.NoiseWindowMS
	}
	if t.MinDynamicRangeDB == 0 {
		t.MinDynamicRangeDB = d.MinDynamicRangeDB
	}
	if t.SustainedSilenceMS == 0 {
		t.SustainedSilenceMS = d.SustainedSilenceMS
	}
	if t.QuietPassageMS == 0 {
		t.QuietPassageMS = d.QuietPassageMS
	}
	if t.DeadInputFloorDBFS == 0 {
		t.DeadInputFloorDBFS = d.DeadInputFloorDBFS
	}
	if t.SilenceRearmDB == 0 {
		t.SilenceRearmDB = d.SilenceRearmDB
	}
	return t
}

// Input is one analysis block for a single configured input. Samples are floats in
// [-1, 1], interleaved when Channels > 1.
type Input struct {
	Samples    []float64
	SampleRate int
	// Channels describes the ANALYSIS LANE in Samples — what the channel mapper
	// handed over, which is 1 for every mode except stereo-pair (§6.5). It is
	// used to de-interleave the block, never to judge hardware capability.
	Channels int
	// DeviceChannels is what the capture HARDWARE offers, before the mapper
	// reduced it to a lane. The mono-vs-stereo capability check (§6.3) uses this,
	// so a legal stereo mapping — which necessarily hands over a 1-channel lane —
	// is not mistaken for a mono device. 0 falls back to Channels.
	DeviceChannels int
	// RequiredChannels is what the configured mapping needs; 0 = no requirement.
	RequiredChannels int
	Present          bool
	DeviceConnected  bool
	DuringPlayback   bool // true once the source is meant to be playing — silence is then a fault.
	// SilentForMS is how long this input has been CONTINUOUSLY silent, INCLUDING
	// this block, as tracked by the caller across blocks (§6.3 "duration +
	// noise-floor context"). Zero means the caller tracks no history — a
	// one-shot calibration analysis — and silence is then judged on this block
	// alone. A live pipeline always passes a positive value.
	SilentForMS int
	// PrevSilent reports whether the caller's previous block was judged silent.
	// With it the silence test applies SilenceRearmDB hysteresis, so a level
	// sitting on the threshold cannot flap the verdict block by block.
	PrevSilent    bool
	InputDeviceID string
	StreamType    model.StreamType
	SourceChannel model.SourceChannel
}

// Analyze qualifies one input block and returns its transient state (§6.3 / §10.2).
// Reasons are generic, content-free strings safe to log and to show staff.
func Analyze(in Input, th Thresholds) model.AudioStreamState {
	th = th.normalized()

	st := model.AudioStreamState{
		InputDeviceID:   in.InputDeviceID,
		StreamType:      in.StreamType,
		SourceChannel:   in.SourceChannel,
		SampleRate:      in.SampleRate,
		Channels:        in.Channels,
		Present:         in.Present,
		DeviceConnected: in.DeviceConnected,
	}

	// The lane width de-interleaves the block; the DEVICE width is what §6.3
	// reports as the input's mono/stereo capability. They differ whenever the
	// mapper has already reduced a stereo device to one lane (§6.5).
	channels := in.Channels
	if channels <= 0 {
		channels = 1
	}
	deviceChannels := in.DeviceChannels
	if deviceChannels <= 0 {
		deviceChannels = channels
	}
	st.Channels = deviceChannels

	// --- Presence / device health (§6.3): the hardest fail. ---
	if !in.Present || !in.DeviceConnected {
		st.Status = model.QualFailed
		st.Reasons = []string{"no input detected"}
		return st
	}

	// --- Mono/stereo capability (§6.3): judged on what the DEVICE offers, not on
	// the width of the lane the mapper produced. A stereo split legally hands
	// over one lane from a two-channel device; blaming the lane width here would
	// fail every legal stereo mapping (§6.5 reserves Failed for mappings the
	// detected hardware cannot satisfy). ---
	if in.RequiredChannels > deviceChannels {
		st.Status = model.QualFailed
		st.Reasons = []string{"input appears mono, but this profile requires two channels"}
		return st
	}

	m := measure(in.Samples, channels, in.SampleRate, in.PrevSilent, th)
	st.LevelDBFS = m.rmsDBFS
	st.NoiseFloorDBFS = m.noiseFloorDBFS
	st.DCOffset = m.dc
	st.Clipping = m.clipping
	st.Silence = m.silent

	// --- Sustained silence (§6.3): a fault only once the source is supposed to
	// be playing, and only once it has LASTED. ---
	if m.silent {
		return silenceVerdict(st, in, m, th)
	}

	// --- Marginal defects: accumulate every applicable reason, then demote. ---
	var reasons []string
	if m.clipping {
		reasons = append(reasons, "audio is clipping — reduce source level or add the inline attenuator")
	} else if m.nearFullScale {
		reasons = append(reasons, "level is very close to full scale — reduce the source for headroom")
	}
	// A high windowed-RMS floor only means "noise" when the signal clearly rides
	// above it (as speech does over its pauses). A continuous calibration tone has
	// no such gap, so its floor reflects the tone itself, not background noise.
	if m.noiseFloorDBFS > th.HighNoiseFloorDBFS && (m.rmsDBFS-m.noiseFloorDBFS) >= th.MinDynamicRangeDB {
		reasons = append(reasons, "noise floor is high")
	}
	if m.rmsDBFS < th.LowLevelDBFS {
		reasons = append(reasons,
			"mic-level input; may distort on a line-level output",
			"signal level low")
	}
	if math.Abs(m.dc) > th.DCOffsetAbs {
		reasons = append(reasons, "significant DC offset detected")
	}
	if len(reasons) > 0 {
		st.Status = model.QualMarginal
		st.Reasons = reasons
		return st
	}

	// --- Clean input: Excellent in the pristine band, otherwise Usable. ---
	if m.rmsDBFS >= th.ExcellentMinDBFS && m.rmsDBFS <= th.HotDBFS {
		st.Status = model.QualExcellent
		// Only claim what was actually established: a clean, well-levelled signal
		// with no clipping/DC and no detectable high floor. The noise floor cannot
		// be independently confirmed on a continuous tone (no quiet gaps), so the
		// reason does not assert "noise is fine" — it states the verified level.
		st.Reasons = []string{"input present; level within normal range"}
		return st
	}
	st.Status = model.QualUsable
	switch {
	case m.rmsDBFS > th.HotDBFS:
		st.Reasons = []string{"input usable; level is hot — reduce the source slightly for more headroom"}
	case m.rmsDBFS < th.ExcellentMinDBFS:
		st.Reasons = []string{"input usable; signal level is lower than ideal"}
	default:
		st.Reasons = []string{"input usable"}
	}
	return st
}

// silenceVerdict classifies a silent block against §6.3's requirement to
// "distinguish 'no signal' from 'quiet passage' via duration + noise-floor
// context".
//
// Duration: the caller reports how long the input has been continuously silent
// (Input.SilentForMS). One quiet 200 ms block is never a fault mid-show — a film
// has quiet beats and a VI-N narration feed is silent between descriptions.
//
// Noise-floor context: a floor that has collapsed to digital zero means there is
// NO signal at all — an unplugged jack or a dead cable — and fails after the
// short SustainedSilenceMS. A floor still sitting at the room/line level means a
// live input that merely has nothing on it, so it is given the far longer
// QuietPassageMS before anything is said about it.
//
// (The digital-zero test assumes an analog input, which is what tiers 0–2 ship.
// A digitally muted tier-3 AES3/Dante feed also reads as exact zeros, so a
// tier-3 deployment should lengthen SustainedSilenceMS; calibrating that needs
// hardware, BLOCKERS B3.)
//
// Stream type: HI carries the whole soundtrack, so a live line with nothing on
// it for that long is a fault. VI-N narration is INTERMITTENT BY DESIGN — the
// gap between descriptions is the normal state — so a live VI-N line that is
// merely quiet is reported as Marginal and never failed on quiet alone. A VI-N
// line with no signal at all still fails: that is §6.3's "Failed: configured VI
// input is silent during playback".
func silenceVerdict(st model.AudioStreamState, in Input, m metrics, th Thresholds) model.AudioStreamState {
	if !in.DuringPlayback {
		st.Status = model.QualMarginal
		st.Reasons = []string{"no signal detected; play your source to qualify the input"}
		return st
	}
	dead := m.noiseFloorDBFS <= th.DeadInputFloorDBFS
	need := th.QuietPassageMS
	if dead {
		need = th.SustainedSilenceMS
	}
	if in.SilentForMS > 0 && in.SilentForMS < need {
		// Quiet, but not yet sustained long enough to be a fault. Report the
		// condition out loud rather than either failing on one block or hiding
		// it — the stream stays usable, and staff can see it is being watched.
		st.Status = model.QualUsable
		st.Reasons = []string{"input is quiet; watching for sustained silence before reporting a fault"}
		return st
	}
	if dead {
		st.Status = model.QualFailed
		st.Reasons = []string{"configured input is silent during playback — no signal at all; check the cable and the source"}
		return st
	}
	if in.StreamType == model.StreamVIN {
		st.Status = model.QualMarginal
		st.Reasons = []string{"no narration on this VI-N input for a sustained period — the line is live, so this may simply be a gap between descriptions"}
		return st
	}
	st.Status = model.QualFailed
	st.Reasons = []string{"configured input is silent during playback — the line is live but carries no programme audio"}
	return st
}

// metrics holds the raw DSP results for one block.
type metrics struct {
	peakDBFS       float64
	rmsDBFS        float64
	noiseFloorDBFS float64
	dc             float64
	clipping       bool
	nearFullScale  bool
	silent         bool
}

// measure computes peak/RMS/DC, sample-saturation clipping, block silence, and a
// low-percentile noise-floor estimate. Per-frame channels are averaged to a mono
// analysis stream for level/DC/floor; clipping scans every raw sample so a single
// saturated channel still counts. channels is the LANE width, so a stereo-pair
// lane is de-interleaved into real frames rather than being windowed at half its
// true duration.
//
// prevSilent carries the caller's previous silence state so the silence test can
// apply re-arm hysteresis; the DURATION rules live in silenceVerdict.
func measure(samples []float64, channels, sampleRate int, prevSilent bool, th Thresholds) metrics {
	var m metrics
	if len(samples) == 0 || channels <= 0 {
		m.peakDBFS, m.rmsDBFS, m.noiseFloorDBFS = MinDBFS, MinDBFS, MinDBFS
		m.silent = true
		return m
	}

	frames := len(samples) / channels

	// Clipping / peak over every raw sample. Real clipping is a SUSTAINED fraction
	// of truly saturated samples (near 0 dBFS); a peak that only reaches the
	// early-warning band (ClipDBFS) without saturation is a headroom warning, not
	// clipping (so a clean sine peaking at −0.4 dBFS is not mislabeled "clipping").
	satLevel := dbToLin(saturationDBFS)
	var peak float64
	var saturated int
	for _, s := range samples {
		a := math.Abs(s)
		if a > peak {
			peak = a
		}
		if a >= satLevel {
			saturated++
		}
	}
	m.peakDBFS = linToDB(peak)
	fullScaleFrac := float64(saturated) / float64(len(samples))
	m.clipping = fullScaleFrac >= th.ClipSampleFraction
	m.nearFullScale = !m.clipping && m.peakDBFS >= th.ClipDBFS

	if frames == 0 {
		// Fewer samples than channels: degenerate, treat as silence.
		m.rmsDBFS, m.noiseFloorDBFS = MinDBFS, MinDBFS
		m.silent = true
		return m
	}

	// Mono analysis stream (per-frame channel average) for level, DC, and floor.
	mono := make([]float64, frames)
	var sum, sumSq float64
	for f := 0; f < frames; f++ {
		var acc float64
		base := f * channels
		for c := 0; c < channels; c++ {
			acc += samples[base+c]
		}
		v := acc / float64(channels)
		mono[f] = v
		sum += v
		sumSq += v * v
	}
	m.dc = sum / float64(frames)
	m.rmsDBFS = linToDB(math.Sqrt(sumSq / float64(frames)))
	// Re-arm hysteresis: once silent, the level must climb SilenceRearmDB ABOVE
	// the silence threshold before the input counts as live again, so a signal
	// hovering on the threshold does not flap the verdict every block (§6.3).
	silenceAt := th.SilenceDBFS
	if prevSilent {
		silenceAt += th.SilenceRearmDB
	}
	m.silent = m.rmsDBFS < silenceAt
	m.noiseFloorDBFS = noiseFloor(mono, sampleRate, th)
	return m
}

// noiseFloor estimates the background level as a low percentile of short-window RMS
// (§6.3). With quiet windows present (speech pauses) it tracks the true floor; with
// none, it approaches the signal level, which the caller's dynamic-range gate
// accounts for.
func noiseFloor(mono []float64, sampleRate int, th Thresholds) float64 {
	win := sampleRate * th.NoiseWindowMS / 1000
	if win < 1 || win > len(mono) {
		win = len(mono)
	}
	if win < 1 {
		return MinDBFS
	}
	var dbs []float64
	for start := 0; start+win <= len(mono); start += win {
		var sq float64
		for i := start; i < start+win; i++ {
			sq += mono[i] * mono[i]
		}
		dbs = append(dbs, linToDB(math.Sqrt(sq/float64(win))))
	}
	if len(dbs) == 0 {
		return MinDBFS
	}
	sort.Float64s(dbs)
	idx := int(th.NoiseFloorPct * float64(len(dbs)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(dbs) {
		idx = len(dbs) - 1
	}
	return dbs[idx]
}

// SineWave generates a synthetic test tone (the §11/Phase-5 kickoff requires a
// synthetic-tone source). Samples are interleaved with the same value on every
// channel; amplitude 1.0 is full scale.
func SineWave(freqHz, amplitude float64, sampleRate, channels, frames int) []float64 {
	if sampleRate <= 0 || channels <= 0 || frames <= 0 {
		return nil
	}
	out := make([]float64, frames*channels)
	w := 2 * math.Pi * freqHz / float64(sampleRate)
	for n := 0; n < frames; n++ {
		v := amplitude * math.Sin(w*float64(n))
		base := n * channels
		for c := 0; c < channels; c++ {
			out[base+c] = v
		}
	}
	return out
}

// linToDB converts a linear magnitude to dBFS, clamped at MinDBFS.
func linToDB(lin float64) float64 {
	if lin <= 0 {
		return MinDBFS
	}
	db := 20 * math.Log10(lin)
	if db < MinDBFS {
		return MinDBFS
	}
	return db
}

// dbToLin converts dBFS to a linear magnitude.
func dbToLin(db float64) float64 { return math.Pow(10, db/20) }
