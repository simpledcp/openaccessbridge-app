// Package qualifier is the four-state audio input qualification engine
// (buildplan.md §6.3). It actively measures a block of captured PCM and emits one
// of Excellent / Usable / Marginal / Failed plus plain-language reasons — it never
// merely accepts whatever it is handed (guardrail #7). It is PURE DSP: no hardware,
// no ALSA, no logging of audio content. The same routine runs during the
// calibration wizard and continuously during playback.
//
// Measurements (§6.3): peak and RMS dBFS, sample-saturation clipping, sustained
// silence, a low-percentile noise-floor estimate, and DC offset. Classification
// is conservative — any one defect demotes the verdict — so a degraded input is
// surfaced before showtime rather than failing silently during a show.
package qualifier

import (
	"math"
	"sort"

	"github.com/simpledcp/captionbridge/internal/model"
)

// minDBFS is the floor we report for digital silence so dB arithmetic stays finite.
const minDBFS = -120.0

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

	// Supporting knobs.
	ClipSampleFraction float64 // fraction of near-full-scale samples that also counts as clipping.
	ExcellentMinDBFS   float64 // lower bound of the pristine RMS band.
	HotDBFS            float64 // RMS above this (without clipping) is hot-but-usable.
	NoiseFloorPct      float64 // percentile of short-window RMS used as the noise-floor estimate.
	NoiseWindowMS      int     // short-window length for the noise-floor estimate.
	MinDynamicRangeDB  float64 // signal-minus-floor gap required before a high floor is blamed on noise.
}

// DefaultThresholds returns field-tested defaults for a consumer USB/line input
// (§6.2 tier 1–2). They err toward warning early: overload within 0.5 dB of full
// scale, silence below −60 dBFS, mic-level below −40 dBFS.
func DefaultThresholds() Thresholds {
	return Thresholds{
		ClipDBFS:           -0.5,
		SilenceDBFS:        -60,
		LowLevelDBFS:       -40,
		HighNoiseFloorDBFS: -45,
		DCOffsetAbs:        0.02,
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
func (t Thresholds) normalized() Thresholds {
	d := DefaultThresholds()
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
	return t
}

// Input is one analysis block for a single configured input. Samples are floats in
// [-1, 1], interleaved when Channels > 1. The block is treated as representative of
// the moment (sustained silence/clipping are judged over the block).
type Input struct {
	Samples          []float64
	SampleRate       int
	Channels         int
	RequiredChannels int // channels the configured mapping needs; 0 = no requirement.
	Present          bool
	DeviceConnected  bool
	DuringPlayback   bool // true once the source is meant to be playing — silence is then a fault.
	InputDeviceID    string
	StreamType       model.StreamType
	SourceChannel    model.SourceChannel
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

	// --- Presence / device health (§6.3): the hardest fail. ---
	if !in.Present || !in.DeviceConnected {
		st.Status = model.QualFailed
		st.Reasons = []string{"no input detected"}
		return st
	}

	channels := in.Channels
	if channels <= 0 {
		channels = 1
	}
	// --- Mono/stereo capability: an impossible mapping fails before showtime. ---
	if in.RequiredChannels > channels {
		st.Status = model.QualFailed
		st.Reasons = []string{"input appears mono, but this profile requires two channels"}
		return st
	}

	m := measure(in.Samples, channels, in.SampleRate, th)
	st.LevelDBFS = m.rmsDBFS
	st.NoiseFloorDBFS = m.noiseFloorDBFS
	st.DCOffset = m.dc
	st.Clipping = m.clipping
	st.Silence = m.silent

	// --- Sustained silence: a fault only once the source is supposed to be playing. ---
	if m.silent {
		if in.DuringPlayback {
			st.Status = model.QualFailed
			st.Reasons = []string{"configured input is silent during playback"}
		} else {
			st.Status = model.QualMarginal
			st.Reasons = []string{"no signal detected; play your source to qualify the input"}
		}
		return st
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

// measure computes peak/RMS/DC, sample-saturation clipping, sustained silence, and
// a low-percentile noise-floor estimate. Per-frame channels are averaged to a mono
// analysis stream for level/DC/floor; clipping scans every raw sample so a single
// saturated channel still counts.
func measure(samples []float64, channels, sampleRate int, th Thresholds) metrics {
	var m metrics
	if len(samples) == 0 || channels <= 0 {
		m.peakDBFS, m.rmsDBFS, m.noiseFloorDBFS = minDBFS, minDBFS, minDBFS
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
		m.rmsDBFS, m.noiseFloorDBFS = minDBFS, minDBFS
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
	m.silent = m.rmsDBFS < th.SilenceDBFS
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
		return minDBFS
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
		return minDBFS
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

// linToDB converts a linear magnitude to dBFS, clamped at minDBFS.
func linToDB(lin float64) float64 {
	if lin <= 0 {
		return minDBFS
	}
	db := 20 * math.Log10(lin)
	if db < minDBFS {
		return minDBFS
	}
	return db
}

// dbToLin converts dBFS to a linear magnitude.
func dbToLin(db float64) float64 { return math.Pow(10, db/20) }
