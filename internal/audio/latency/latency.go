// Package latency is the §11.4 latency pass, reduced to what this host can
// honestly measure.
//
// §15 names HI latency as the plan's largest risk and §6.7 flags the sub-150 ms
// HI target as "aggressive and may be unachievable", with the phone-side jitter
// buffer as the dominant, least-controllable term. Until the §6.7 WebRTC path
// existed there was no peer to measure at all, so every row of §6.7's latency
// table was a quoted typical rather than a reading.
//
// WHAT THIS MEASURES. The APPLIANCE-side path only, one contributor at a time,
// so a real figure can later be COMPOSED with the phone-side terms instead of
// guessed:
//
//   - the pure-Go stages (channel map, level analysis) — direct CPU timings of
//     the same mapper.Extract / qualifier.Analyze calls the pipeline makes;
//   - Opus encode transit — a 10 ms block handed to the encoder until the RTP
//     packet carrying it is read back out. MEASURED, not quoted from §6.7's
//     "~10–20 ms" row;
//   - WebRTC fan-out transit — that same packet from the encoder's socket to a
//     Pion subscriber, through the real webrtcpeer.Hub, a real SRTP session and
//     a real ICE host-candidate hop;
//   - the appliance floor — the same packet end to end across both of those.
//
// WHAT THIS IS NOT, stated here because it is the whole point of the pass. This
// is a Go-peer-to-Go-peer measurement on ONE machine. It is a FLOOR for the
// appliance's own contribution and it says NOTHING about glass-to-ear. There is
// no ADC (BLOCKER B3), no Wi-Fi, no browser, no phone and no Opus decoder in the
// path (BLOCKER B2). Nothing here may be compared to §6.7's 100–150 ms HI target
// as though the target had been met or missed: the dominant term of that budget
// — the phone-side jitter buffer at 40–300+ ms — is not in this measurement at
// all. Every Result carries that boundary with it (see Unmeasured and Summary)
// so a number cannot be quoted away from its caveat.
//
// Audio stays transient content throughout (§9.4, guardrail #5): the harness
// times packets and counts them, and never writes a sample or an SDP body
// anywhere.
package latency

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Stage names. They are the keys an operator or a regression guard looks a
// contributor up by, and they are deliberately NOT named after §6.7's table
// rows: a row of that table is a typical, and these are readings.
const (
	// StageChannelMap is §6.5's mapper on the DELIVERY path: one Extract per
	// capture frame, which is the only mapping between a captured frame and the
	// encoder.
	StageChannelMap = "channel_map"
	// StageLevelAnalysis is the §6.3 qualifier tap: one Extract plus one Analyze
	// per analysis block.
	StageLevelAnalysis = "level_analysis"
	// StageOpusEncode is the Opus encode + RTP packetization transit.
	StageOpusEncode = "opus_encode_transit"
	// StageWebRTCFanout is the appliance-side transport: fan-out, SRTP and the
	// ICE host-candidate hop to a subscriber on this machine.
	StageWebRTCFanout = "webrtc_fanout_transit"
	// StageApplianceFloor is both streaming stages measured on ONE packet, which
	// is why it is a measurement and not a sum of percentiles.
	StageApplianceFloor = "appliance_floor"
)

// Distribution is one stage's measured spread. §11.4 asks for "the distribution
// (not just best case)", and §6.7's concern is jitter, which a mean hides
// exactly — so p50 and p95 are reported and no mean is offered.
type Distribution struct {
	N   int           `json:"n"`
	Min time.Duration `json:"min"`
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	Max time.Duration `json:"max"`
}

// Stage is one measured contributor, carrying the exact bracket its two
// timestamps span. The bracket travels WITH the number: a stage quoted without
// it is the fabrication this pass exists to avoid.
type Stage struct {
	Name string `json:"name"`
	// Bracket says precisely which two events the measurement spans.
	Bracket string `json:"bracket"`
	// Excludes names what is NOT inside this stage, so it cannot be read as
	// more than it is.
	Excludes string       `json:"excludes"`
	D        Distribution `json:"distribution"`
}

// Result is one harness run.
type Result struct {
	Config Config  `json:"config"`
	Stages []Stage `json:"stages"`

	// StreamingMeasured is false when no Opus encoder could run on this host, in
	// which case the streaming stages are absent rather than zero — an absent
	// measurement must never render as 0 ms.
	StreamingMeasured bool `json:"streaming_measured"`
	// SkipReason says why the streaming half did not run, when it did not.
	SkipReason string `json:"skip_reason,omitempty"`

	// FramingMS is the STRUCTURAL delay this harness does not and cannot
	// stopwatch: a 10 ms Opus frame cannot be emitted before 10 ms of audio
	// exists, so the first sample of every block waits a whole capture period
	// before delivery begins. It is stated separately and never folded into a
	// measured figure.
	FramingMS int `json:"framing_ms"`
	// EstimateMS is what the §6.6 dashboard reports for this configuration
	// (pipeline.EncoderPathLatencyMS). Printed beside the measurements so a
	// reader can see that the dashboard figure is a structural budget rather
	// than a reading.
	EstimateMS int `json:"encode_path_estimate_ms"`

	// OpusPreSkipSamples is the encoder's own lookahead, MEASURED from the RTP
	// clock: the first timestamp stride the encoder emitted was short by this
	// many samples while every later stride was exactly one frame. 0 means the
	// pattern did not appear and nothing is claimed.
	OpusPreSkipSamples int `json:"opus_preskip_samples"`

	// Counters of what actually moved, so a run that measured nothing cannot
	// present itself as a clean result.
	PacketsEncoded  int `json:"packets_encoded"`
	PacketsReceived int `json:"packets_received"`
	// EncoderPCMDropped is pipeline/encoder back-pressure: blocks the encoder
	// would not take. Non-zero means a gap in a listener's audio AND that some
	// correlation windows were resynchronised.
	EncoderPCMDropped uint64 `json:"encoder_pcm_dropped"`
	// SeqResyncs counts RTP sequence discontinuities at the encoder's own
	// socket. The write↔packet correlation resynchronises on each one rather
	// than silently drifting by a frame.
	SeqResyncs int `json:"seq_resyncs"`
	// Unjoined counts received packets whose encoder-egress timestamp was not
	// held, so they contributed to no transport figure.
	Unjoined int `json:"unjoined"`
}

// Unmeasured is every term of §6.7's latency budget that this harness does NOT
// contain, with the blocker that gates it. It is a method rather than a field so
// it cannot be dropped from a Result by constructing one differently, and
// Summary always prints it.
func (r Result) Unmeasured() []string {
	return []string{
		"Analog capture + ALSA buffer (§6.7: 5–20 ms) — this host has no capture " +
			"hardware; the source is a synthetic tone (BLOCKER B3).",
		"Local Wi-Fi transport (§6.7: 5–30 ms) — both peers are processes on this " +
			"one machine. No AP, no RF, no contention.",
		"Browser jitter buffer (§6.7: 40–300+ ms, THE DOMINANT TERM) — there is no " +
			"browser here. A Pion receiver hands packets up as they arrive and " +
			"buffers nothing, which is exactly why this is a floor (BLOCKER B2).",
		"Opus decode — nothing in this harness decodes the payload; packets are " +
			"timed and counted, never decoded.",
		"Phone audio output (§6.7: 10–40 ms wired; Bluetooth adds 100–300 ms) — " +
			"there is no phone (BLOCKER B2).",
	}
}

// ComposedApplianceFloor is the appliance-side floor with the structural
// framing term added to the MEASURED p95 of the end-to-end appliance stage. It
// exists because the point of this pass is to let a real figure be COMPOSED with
// the phone-side terms later rather than guessed — but it is still only the
// appliance's own contribution, and every term in Unmeasured() has to be ADDED
// to it before anything may be said about a patron's ear. ok is false when the
// streaming half did not run, in which case there is no total to give.
func (r Result) ComposedApplianceFloor() (time.Duration, bool) {
	s, found := r.Stage(StageApplianceFloor)
	if !r.StreamingMeasured || !found || s.D.N == 0 {
		return 0, false
	}
	return time.Duration(r.FramingMS)*time.Millisecond + s.D.P95, true
}

// Stage returns a measured stage by name.
func (r Result) Stage(name string) (Stage, bool) {
	for _, s := range r.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// headline is the sentence that must accompany every number this package
// produces. §11.4 asks for the achievable HI latency "honestly"; the honest
// answer from this host is a floor plus a precise statement of what is missing.
const headline = "APPLIANCE-SIDE FLOOR — NOT a glass-to-ear latency."

// Summary renders a run for an operator, a CI log or a doc. It leads with the
// boundary of the claim and ends with what is still unmeasured, because a number
// from this harness quoted without both is a claim the run did not make. It
// reports timings and counts only — never audio, never SDP (§9.4).
func (r Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "§11.4 latency pass: %s\n", headline)
	b.WriteString(
		"  Measured Go-peer-to-Go-peer on one machine: it bounds the appliance's OWN\n" +
			"  contribution from below and says nothing about what a patron hears. It must\n" +
			"  not be compared to §6.7's 100–150 ms HI target as though that target had\n" +
			"  been met: the dominant term of that budget is the phone-side jitter buffer\n" +
			"  and it is not in these numbers at all.\n\n")

	fmt.Fprintf(&b, "  config: %d ms frame, %d Hz, %d ch lane, %s sampled after %s warm-up\n\n",
		r.Config.FrameMS, r.Config.SampleRate, r.Config.Channels, r.Config.Duration, r.Config.Warmup)

	b.WriteString("  MEASURED (appliance side only)\n")
	for _, s := range r.Stages {
		fmt.Fprintf(&b, "    %-22s n=%-6d p50 %-10s p95 %-10s min %-10s max %s\n",
			s.Name, s.D.N, dur(s.D.P50), dur(s.D.P95), dur(s.D.Min), dur(s.D.Max))
		fmt.Fprintf(&b, "      spans:    %s\n", s.Bracket)
		fmt.Fprintf(&b, "      excludes: %s\n", s.Excludes)
	}
	if !r.StreamingMeasured {
		fmt.Fprintf(&b, "    opus/webrtc stages: NOT MEASURED — %s\n", r.SkipReason)
	}

	b.WriteString("\n  STRUCTURAL (stated, not stopwatched)\n")
	fmt.Fprintf(&b, "    framing delay        %d ms — a %d ms Opus frame cannot be emitted before\n"+
		"                         %d ms of audio exists, so the FIRST sample of each block waits\n"+
		"                         that long. Not added into any measured figure above.\n",
		r.FramingMS, r.Config.FrameMS, r.Config.FrameMS)
	fmt.Fprintf(&b, "    dashboard estimate   %d ms — pipeline.EncoderPathLatencyMS for this\n"+
		"                         configuration (capture period + Opus frame). A budget, not a reading.\n",
		r.EstimateMS)
	if r.OpusPreSkipSamples > 0 {
		fmt.Fprintf(&b, "    opus pre-skip        %d samples (%.1f ms) — MEASURED from the RTP clock: the\n"+
			"                         encoder's first timestamp stride was short by this much while every\n"+
			"                         later stride was exactly one frame. It is the encoder's own\n"+
			"                         lookahead; where it lands in a glass-to-ear budget depends on the\n"+
			"                         decoder, which is not measured here.\n",
			r.OpusPreSkipSamples, float64(r.OpusPreSkipSamples)*1000/float64(r.Config.SampleRate))
	}

	if total, ok := r.ComposedApplianceFloor(); ok {
		b.WriteString("\n  APPLIANCE-SIDE TOTAL (composed, still a floor)\n")
		fmt.Fprintf(&b, "    %s = %d ms framing + %s measured p95, for the OLDEST sample in a block.\n",
			dur(total), r.FramingMS, dur(mustStage(r, StageApplianceFloor).D.P95))
		b.WriteString("    Every term in NOT MEASURED below must be ADDED to this before anything is\n" +
			"    said about a patron's ear, and on real hardware the framing term becomes the\n" +
			"    ALSA capture period (§6.7 row 1: 5–20 ms), which is not this synthetic frame.\n")
	}

	if r.StreamingMeasured {
		fmt.Fprintf(&b, "\n  TRAFFIC  encoded=%d received=%d pcm_dropped=%d seq_resyncs=%d unjoined=%d\n",
			r.PacketsEncoded, r.PacketsReceived, r.EncoderPCMDropped, r.SeqResyncs, r.Unjoined)
	}

	b.WriteString("\n  NOT MEASURED HERE (every remaining row of §6.7's budget)\n")
	for _, u := range r.Unmeasured() {
		fmt.Fprintf(&b, "    - %s\n", u)
	}
	b.WriteString("    Closing these needs the §11.4 loopback rig and a phone fleet (B2, B3).\n")
	return b.String()
}

// mustStage is a Summary-local lookup for a stage the caller has already
// established exists.
func mustStage(r Result, name string) Stage {
	s, _ := r.Stage(name)
	return s
}

// dur renders a duration at a resolution a reader can compare across stages
// without counting zeros; the pure-Go stages land in microseconds and the
// streaming ones in milliseconds.
func dur(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1e3)
	default:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	}
}

// newDistribution summarises samples. An EMPTY input gives the zero
// Distribution with N=0, which Summary renders as "-" rather than as 0 ms: a
// stage that produced no samples has no reading, and reporting one would be the
// cleared-sentinel fabrication this codebase has already been caught by once.
func newDistribution(samples []time.Duration) Distribution {
	if len(samples) == 0 {
		return Distribution{}
	}
	s := make([]time.Duration, len(samples))
	copy(s, samples)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return Distribution{
		N:   len(s),
		Min: s[0],
		P50: percentile(s, 50),
		P95: percentile(s, 95),
		Max: s[len(s)-1],
	}
}

// percentile is NEAREST-RANK on ascending samples: the smallest observed value
// at or below which at least p% of the samples fall. It is deliberately not
// interpolating — an interpolated p95 reports a duration that was never
// observed, and every figure this package prints must be one that happened.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
