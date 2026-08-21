// Package mapper is the HI / VI-N channel mapping model (buildplan.md §6.5): the
// single place between capture and encode that decides which captured channel(s)
// feed which named accessibility stream. It is PURE logic — no I/O, no logging,
// no audio retained beyond the caller's buffers.
//
// Low-end inputs may offer only one mono signal or a single stereo pair, so
// mapping must be flexible: mono → one stream; stereo → L/R split (either
// direction), summed-mono, or stereo-as-one; or two separate USB devices. A
// mapping the detected hardware cannot satisfy (e.g. a stereo L/R split on a
// mono device) is reported as an error here so it becomes a Failed qualifier
// state surfaced BEFORE a show, never a silent fault during one (§6.5).
package mapper

import (
	"fmt"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Mode is an admin-selectable channel mapping (§6.5). Its string value is the
// canonical admin token and round-trips through ParseMode/String.
type Mode string

const (
	// MonoHI / MonoVIN: a mono input feeds a single named stream.
	MonoHI  Mode = "mono:HI"
	MonoVIN Mode = "mono:VI-N"

	// Stereo L/R split, either direction. These canonical strings are the
	// admin tokens called out in §6.5 / the config schema.
	StereoLeftHIRightVIN Mode = "stereo:L=HI,R=VI-N"
	StereoLeftVINRightHI Mode = "stereo:L=VI-N,R=HI"

	// Summed-mono: average L+R into one mono stream.
	SummedMonoHI  Mode = "summed:HI"
	SummedMonoVIN Mode = "summed:VI-N"

	// Stereo-as-one: the whole stereo pair feeds one named stream.
	StereoPairHI  Mode = "stereo-pair:HI"
	StereoPairVIN Mode = "stereo-pair:VI-N"

	// MultiDevice: two separate (mono) USB inputs, Device A = HI, Device B = VI-N.
	MultiDevice Mode = "multi-device"
)

// StreamMap binds one named stream to the captured channel(s) that feed it.
type StreamMap struct {
	Stream model.StreamType
	Source model.SourceChannel
}

// Plan is the full per-stream mapping a mode resolves to.
type Plan struct {
	Maps []StreamMap
}

// modeMaps is the §6.5 table: the fixed per-stream mapping each mode produces.
// It is the single source of truth for both PlanFor and ParseMode.
var modeMaps = map[Mode][]StreamMap{
	MonoHI:               {{Stream: model.StreamHI, Source: model.ChannelMono}},
	MonoVIN:              {{Stream: model.StreamVIN, Source: model.ChannelMono}},
	StereoLeftHIRightVIN: {{Stream: model.StreamHI, Source: model.ChannelLeft}, {Stream: model.StreamVIN, Source: model.ChannelRight}},
	StereoLeftVINRightHI: {{Stream: model.StreamVIN, Source: model.ChannelLeft}, {Stream: model.StreamHI, Source: model.ChannelRight}},
	SummedMonoHI:         {{Stream: model.StreamHI, Source: model.ChannelSummed}},
	SummedMonoVIN:        {{Stream: model.StreamVIN, Source: model.ChannelSummed}},
	StereoPairHI:         {{Stream: model.StreamHI, Source: model.ChannelStereo}},
	StereoPairVIN:        {{Stream: model.StreamVIN, Source: model.ChannelStereo}},
	MultiDevice:          {{Stream: model.StreamHI, Source: model.ChannelMono}, {Stream: model.StreamVIN, Source: model.ChannelMono}},
}

// String returns the canonical admin token for the mode.
func (m Mode) String() string { return string(m) }

// ParseMode parses an admin mapping token (e.g. "stereo:L=HI,R=VI-N"). It is the
// inverse of String for every defined Mode; surrounding whitespace is tolerated.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.TrimSpace(s))
	if _, ok := modeMaps[m]; ok {
		return m, nil
	}
	return "", fmt.Errorf("mapper: unrecognized mapping mode %q (§6.5)", s)
}

// PlanFor resolves a mode to its per-stream mapping, validating it against the
// detected input. It ERRORS when the hardware cannot satisfy the mode — notably
// a stereo L/R split, summed, or stereo-pair request on a mono (1-channel)
// input — so the condition surfaces as Failed before a show, not mid-show (§6.5).
func PlanFor(mode Mode, inputChannels int) (Plan, error) {
	maps, ok := modeMaps[mode]
	if !ok {
		return Plan{}, fmt.Errorf("mapper: unknown mode %q (§6.5)", mode)
	}
	if inputChannels < 1 {
		return Plan{}, fmt.Errorf("mapper: mode %s needs an input with at least 1 channel, got %d (§6.5)", mode, inputChannels)
	}
	// MultiDevice's two named streams live on two independent mono devices, so a
	// single input-channel count cannot describe both; require only that an input
	// is present (the per-device shape is qualified elsewhere).
	if mode == MultiDevice {
		return clonePlan(maps), nil
	}
	for _, sm := range maps {
		if err := requireSource(sm.Source, inputChannels); err != nil {
			return Plan{}, fmt.Errorf("mapper: mode %s impossible on a %d-channel input: %w (§6.5 stereo-on-mono => Failed before show)", mode, inputChannels, err)
		}
	}
	return clonePlan(maps), nil
}

// clonePlan returns a Plan with a fresh copy of the mapping slice so callers
// cannot mutate the shared modeMaps table.
func clonePlan(maps []StreamMap) Plan {
	cp := make([]StreamMap, len(maps))
	copy(cp, maps)
	return Plan{Maps: cp}
}

// requireSource reports whether a source channel is derivable from an input with
// the given channel count. Mono is exactly one lane; left/right/summed/stereo all
// need a stereo (>=2 channel) input.
func requireSource(src model.SourceChannel, channels int) error {
	switch src {
	case model.ChannelMono:
		if channels != 1 {
			return fmt.Errorf("%s source requires a 1-channel (mono) input, got %d", src, channels)
		}
	case model.ChannelLeft, model.ChannelRight, model.ChannelSummed, model.ChannelStereo:
		if channels < 2 {
			return fmt.Errorf("%s source requires a stereo (>=2 channel) input, got %d", src, channels)
		}
	default:
		return fmt.Errorf("unknown source channel %q", src)
	}
	return nil
}

// Extract pulls the named channel(s) out of an interleaved frame buffer of
// `channels` lanes. left/right pick a lane; summed averages L+R; stereo passes
// the L/R pair through as 2-channel interleaved; mono passes a mono buffer
// through. It errors if the source is impossible for the channel count or the
// buffer is malformed. No sample values appear in any error (audio is content).
func Extract(source model.SourceChannel, interleaved []float64, channels int) ([]float64, error) {
	if channels < 1 {
		return nil, fmt.Errorf("mapper: invalid channel count %d", channels)
	}
	if len(interleaved)%channels != 0 {
		return nil, fmt.Errorf("mapper: interleaved length %d is not a multiple of %d channels", len(interleaved), channels)
	}
	if err := requireSource(source, channels); err != nil {
		return nil, fmt.Errorf("mapper: cannot extract %s: %w", source, err)
	}
	frames := len(interleaved) / channels
	switch source {
	case model.ChannelMono:
		out := make([]float64, len(interleaved))
		copy(out, interleaved)
		return out, nil
	case model.ChannelLeft:
		out := make([]float64, frames)
		for i := 0; i < frames; i++ {
			out[i] = interleaved[i*channels]
		}
		return out, nil
	case model.ChannelRight:
		out := make([]float64, frames)
		for i := 0; i < frames; i++ {
			out[i] = interleaved[i*channels+1]
		}
		return out, nil
	case model.ChannelSummed:
		out := make([]float64, frames)
		for i := 0; i < frames; i++ {
			out[i] = (interleaved[i*channels] + interleaved[i*channels+1]) / 2
		}
		return out, nil
	case model.ChannelStereo:
		out := make([]float64, frames*2)
		for i := 0; i < frames; i++ {
			out[2*i] = interleaved[i*channels]
			out[2*i+1] = interleaved[i*channels+1]
		}
		return out, nil
	default:
		return nil, fmt.Errorf("mapper: unknown source channel %q", source)
	}
}
