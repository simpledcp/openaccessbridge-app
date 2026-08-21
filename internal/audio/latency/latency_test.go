package latency

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestLatencyApplianceFloor is the §11.4 pass as CI runs it: a short run that
// pins the APPLIANCE-side stages inside a sane bound. It is a REGRESSION GUARD,
// not a claim about the product — see the bounds' comments, and see Summary for
// what the numbers do and do not cover.
//
// For a long run (the `make latency` target):
//
//	OPENACCESSBRIDGE_LATENCY_SECONDS=60 go test ./internal/audio/latency/ -run Latency -v -timeout 20m
func TestLatencyApplianceFloor(t *testing.T) {
	cfg := DefaultConfig()
	if v := os.Getenv("OPENACCESSBRIDGE_LATENCY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("OPENACCESSBRIDGE_LATENCY_SECONDS=%q is not a positive number of seconds", v)
		}
		cfg.Duration = time.Duration(n) * time.Second
	}
	if testing.Short() {
		// The streaming half spawns a real gst-launch encoder and negotiates a
		// real peer connection; -short keeps the pure-Go stages and skips it.
		cfg.AnalysisOnly = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Warmup+cfg.Duration+90*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg)
	t.Log("\n" + res.Summary())

	// --- the pure-Go stages always run --------------------------------------
	//
	// §6.7's table budgets "<5 ms" for level analysis + channel map. These
	// bounds are that row with generous headroom for a loaded CI box under
	// -race; they catch an algorithmic regression (an accidental O(n²) in the
	// mapper, a qualifier that grew a per-sample allocation), not a slow minute.
	assertUnder(t, res, StageChannelMap, 2*time.Millisecond)
	assertUnder(t, res, StageLevelAnalysis, 15*time.Millisecond)

	if testing.Short() {
		if res.StreamingMeasured {
			t.Error("an analysis-only run reported the streaming stages as measured")
		}
		return
	}
	if err != nil {
		// No encoder or no host candidate on this machine: the streaming stages
		// have no reading and none is invented.
		t.Skipf("appliance-side streaming path not measurable here: %v", err)
	}

	// --- the streaming stages -----------------------------------------------
	//
	// Measured p50 on this dev host is well under a millisecond for both legs.
	// The bounds are ~50x that: a real regression (a buffering element added to
	// the encoder pipeline, a fan-out that queues) blows straight through them
	// while scheduling noise does not.
	assertUnder(t, res, StageOpusEncode, 25*time.Millisecond)
	assertUnder(t, res, StageWebRTCFanout, 25*time.Millisecond)
	assertUnder(t, res, StageApplianceFloor, 40*time.Millisecond)

	// A run that measured nothing would satisfy every bound above, so pin that
	// audio really crossed the whole path.
	if res.PacketsEncoded == 0 || res.PacketsReceived == 0 {
		t.Fatalf("no packets moved: encoded=%d received=%d", res.PacketsEncoded, res.PacketsReceived)
	}
	floor, _ := res.Stage(StageApplianceFloor)
	// At a 10 ms frame the sampling window should yield ~100 joined packets per
	// second. Requiring a third of that leaves room for a slow start without
	// letting a two-packet "distribution" pass as one.
	if want := int(cfg.Duration/time.Millisecond) / 30; floor.D.N < want {
		t.Errorf("only %d packets joined across the sampling window, want at least %d — a p95 over that few is not a distribution",
			floor.D.N, want)
	}
	if res.EncoderPCMDropped > 0 {
		t.Errorf("the encoder dropped %d PCM blocks during the run: a listener heard a gap, and the run is not a clean steady state", res.EncoderPCMDropped)
	}
}

func assertUnder(t *testing.T, res Result, stage string, bound time.Duration) {
	t.Helper()
	s, ok := res.Stage(stage)
	if !ok {
		t.Fatalf("stage %q was not measured", stage)
	}
	if s.D.N == 0 {
		t.Fatalf("stage %q reported no samples", stage)
	}
	if s.D.P95 > bound {
		t.Errorf("%s p95 = %v, over the regression bound %v (p50 %v, max %v)", stage, s.D.P95, bound, s.D.P50, s.D.Max)
	}
}

// The whole value of this pass is an honest floor plus a precise statement of
// what is still unmeasured, so the statement is pinned like any other behaviour:
// a number from this package must never be printable without it.
func TestSummaryAlwaysCarriesTheBoundaryOfTheClaim(t *testing.T) {
	res := Result{Config: DefaultConfig(), StreamingMeasured: true}
	s := res.Summary()

	for _, must := range []string{
		"APPLIANCE-SIDE FLOOR",
		"NOT a glass-to-ear latency",
		"jitter buffer",
		"100–150 ms HI target",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("Summary does not say %q — a reader could quote a number from it as an end-to-end figure", must)
		}
	}
	for _, u := range res.Unmeasured() {
		if !strings.Contains(s, u) {
			t.Errorf("Summary omits an unmeasured term of §6.7's budget:\n  %s", u)
		}
	}
	// The dominant term must be named as dominant, not merely listed.
	if !strings.Contains(s, "THE DOMINANT TERM") {
		t.Error("the phone-side jitter buffer is not flagged as the dominant term of §6.7's budget")
	}
	// And the blockers that gate the rest must be named where a reader will see
	// them, or "unmeasured" reads as "unimportant".
	for _, b := range []string{"B2", "B3"} {
		if !strings.Contains(s, b) {
			t.Errorf("Summary does not name BLOCKER %s", b)
		}
	}
}

// An absent measurement must render as "no reading", never as 0 ms. This
// codebase has already shipped a cleared sentinel printed as a reading once.
func TestAStageWithNoSamplesReportsNoReadingNotZero(t *testing.T) {
	if got := newDistribution(nil); got.N != 0 || got.P50 != 0 {
		t.Fatalf("empty distribution = %+v, want the zero value", got)
	}
	if got := dur(0); got != "-" {
		t.Errorf("dur(0) = %q, want %q so an unmeasured stage cannot read as zero milliseconds", got, "-")
	}
	res := Result{Config: DefaultConfig(), SkipReason: "no gst-launch on this host"}
	if s := res.Summary(); !strings.Contains(s, "NOT MEASURED — no gst-launch on this host") {
		t.Error("a run without an encoder does not say so; its stages could be read as zero")
	}
}

// Nearest-rank, never interpolated: every figure printed must be a duration that
// was actually observed.
func TestPercentileIsNearestRankOverObservedValues(t *testing.T) {
	in := []time.Duration{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	d := newDistribution(in)
	if d.N != 10 || d.Min != 10 || d.Max != 100 {
		t.Fatalf("distribution = %+v", d)
	}
	if d.P50 != 50 {
		t.Errorf("P50 = %v, want 50 (nearest-rank over 10 samples)", d.P50)
	}
	if d.P95 != 100 {
		t.Errorf("P95 = %v, want 100 (nearest-rank: ceil(0.95*10) = 10)", d.P95)
	}
	// Single sample: every percentile is that sample.
	one := newDistribution([]time.Duration{7})
	if one.P50 != 7 || one.P95 != 7 || one.Min != 7 || one.Max != 7 {
		t.Errorf("single-sample distribution = %+v", one)
	}
	// The input must not be reordered under the caller.
	src := []time.Duration{3, 1, 2}
	newDistribution(src)
	if src[0] != 3 {
		t.Errorf("newDistribution sorted the caller's slice: %v", src)
	}
}

// The write↔packet correlation is the load-bearing assumption of the encode
// measurement. A gap in the encoder's RTP sequence must RESYNCHRONISE the
// ordinal, because a silent one-frame drift would understate the floor by a
// whole Opus frame for the rest of the run.
func TestOrdinalResynchronisesAfterASequenceGap(t *testing.T) {
	tp := newTap(DefaultConfig())
	now := time.Now()
	for i := 0; i < 6; i++ {
		tp.writes = append(tp.writes, now.Add(time.Duration(i)*10*time.Millisecond))
	}
	// Packets 0 and 1 arrive; packet 2 is lost; packet 3 arrives.
	tp.note(rtp.Header{SequenceNumber: 100, Timestamp: 1000}, now)
	tp.note(rtp.Header{SequenceNumber: 101, Timestamp: 1480}, now)
	tp.note(rtp.Header{SequenceNumber: 103, Timestamp: 2440}, now)

	if tp.resyncs != 1 {
		t.Errorf("resyncs = %d, want 1 (one sequence discontinuity)", tp.resyncs)
	}
	got, ok := tp.egress[2440]
	if !ok {
		t.Fatal("the packet after the gap was not recorded")
	}
	if got.ord != 3 {
		t.Errorf("packet after a one-packet gap correlated to write %d, want 3 — the ordinal drifted", got.ord)
	}
	// The lost packet's stride is not one frame, so no pre-skip is claimed.
	if tp.strideOK {
		t.Error("a run with a lost packet still claims the clean RTP ramp the pre-skip reading depends on")
	}
}

// A packet whose audio had not been written yet cannot be correlated to it. If
// the join ever produces a negative transit the correlation is wrong, and a
// wrong correlation must abort rather than report a shorter floor than the
// appliance has.
func TestJoinRefusesAnImpossibleTransit(t *testing.T) {
	tp := newTap(DefaultConfig())
	base := time.Now()
	tp.writes = append(tp.writes, base)
	// Egress recorded BEFORE the write it is attributed to.
	tp.egress[480] = egressSample{at: base.Add(-5 * time.Millisecond), ord: 0}

	var res Result
	err := tp.join([]arrival{{ts: 480, at: base.Add(time.Millisecond)}},
		base.Add(-time.Second), base.Add(time.Second), &res)
	if err == nil {
		t.Fatal("join accepted a packet that left the encoder before its audio went in")
	}
	if !strings.Contains(err.Error(), "correlation") {
		t.Errorf("error does not name the correlation as the fault: %v", err)
	}
	if len(res.Stages) != 0 {
		t.Error("a refused join still produced stages")
	}
}

// Only packets whose egress fell inside the sampling window count: the warm-up
// carries gst-launch start-up, which is not the steady state.
func TestJoinKeepsOnlyTheSamplingWindow(t *testing.T) {
	tp := newTap(DefaultConfig())
	base := time.Now()
	for i := 0; i < 3; i++ {
		tp.writes = append(tp.writes, base.Add(time.Duration(i)*10*time.Millisecond))
	}
	// Packet 0 is warm-up; packets 1 and 2 are inside the window.
	tp.egress[480] = egressSample{at: base.Add(1 * time.Millisecond), ord: 0}
	tp.egress[960] = egressSample{at: base.Add(11 * time.Millisecond), ord: 1}
	tp.egress[1440] = egressSample{at: base.Add(21 * time.Millisecond), ord: 2}

	var res Result
	err := tp.join([]arrival{
		{ts: 480, at: base.Add(2 * time.Millisecond)},
		{ts: 960, at: base.Add(12 * time.Millisecond)},
		{ts: 1440, at: base.Add(22 * time.Millisecond)},
		{ts: 99999, at: base.Add(30 * time.Millisecond)}, // never seen at the encoder
	}, base.Add(5*time.Millisecond), base.Add(time.Second), &res)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	s, ok := res.Stage(StageApplianceFloor)
	if !ok {
		t.Fatal("no appliance floor stage")
	}
	if s.D.N != 2 {
		t.Errorf("joined %d packets, want 2 — the warm-up packet was not discarded", s.D.N)
	}
	if res.Unjoined != 1 {
		t.Errorf("Unjoined = %d, want 1 (a packet the encoder never emitted)", res.Unjoined)
	}
}

// The pre-skip reading is derived from the RTP clock, so it must appear only
// when the clock really was a short first stride followed by exact frames.
func TestPreSkipIsClaimedOnlyFromACleanRTPRamp(t *testing.T) {
	mk := func(strides ...uint32) *tap {
		tp := newTap(DefaultConfig())
		now := time.Now()
		ts := uint32(1000)
		tp.note(rtp.Header{SequenceNumber: 1, Timestamp: ts}, now)
		for i, s := range strides {
			ts += s
			tp.note(rtp.Header{SequenceNumber: uint16(2 + i), Timestamp: ts}, now)
		}
		return tp
	}
	var res Result
	res.Config = DefaultConfig()
	clean := mk(360, 480, 480, 480)
	clean.writes = make([]time.Time, 8)
	if err := clean.join(nil, time.Now(), time.Now(), &res); err == nil {
		t.Fatal("join with no arrivals should report that nothing could be joined")
	}
	if res.OpusPreSkipSamples != 120 {
		t.Errorf("pre-skip = %d samples, want 120 (480 − a 360-tick first stride)", res.OpusPreSkipSamples)
	}

	var res2 Result
	res2.Config = DefaultConfig()
	ragged := mk(360, 480, 470, 490)
	ragged.writes = make([]time.Time, 8)
	_ = ragged.join(nil, time.Now(), time.Now(), &res2)
	if res2.OpusPreSkipSamples != 0 {
		t.Errorf("pre-skip = %d from a ragged RTP clock, want 0 — nothing may be claimed from it", res2.OpusPreSkipSamples)
	}

	var res3 Result
	res3.Config = DefaultConfig()
	full := mk(480, 480, 480)
	full.writes = make([]time.Time, 8)
	_ = full.join(nil, time.Now(), time.Now(), &res3)
	if res3.OpusPreSkipSamples != 0 {
		t.Errorf("pre-skip = %d with no short first stride, want 0", res3.OpusPreSkipSamples)
	}
}

// The harness must never let a synthetic source wear a real device's identity —
// the exact fabrication this codebase was caught by (B3).
func TestTheSourceDeclaresItselfSynthetic(t *testing.T) {
	for _, must := range []string{"synthetic", "B3"} {
		if !strings.Contains(syntheticDeviceID, must) {
			t.Errorf("the harness source id %q does not say %q", syntheticDeviceID, must)
		}
	}
}

// The composed appliance-side total is the number most likely to be lifted out
// of this report, so it must not exist without the sentence that bounds it — and
// it must not exist at all when nothing was measured.
func TestComposedTotalCannotAppearWithoutItsBoundary(t *testing.T) {
	empty := Result{Config: DefaultConfig(), FramingMS: 10}
	if _, ok := empty.ComposedApplianceFloor(); ok {
		t.Error("a run that measured no streaming stage still offered a composed total")
	}

	res := Result{Config: DefaultConfig(), FramingMS: 10, StreamingMeasured: true}
	res.Stages = append(res.Stages, Stage{
		Name: StageApplianceFloor,
		D:    newDistribution([]time.Duration{500 * time.Microsecond, 700 * time.Microsecond}),
	})
	total, ok := res.ComposedApplianceFloor()
	if !ok {
		t.Fatal("no composed total from a measured run")
	}
	if want := 10*time.Millisecond + 700*time.Microsecond; total != want {
		t.Errorf("composed total = %v, want %v (framing + measured p95)", total, want)
	}
	s := res.Summary()
	if !strings.Contains(s, "still a floor") {
		t.Error("the composed total is not labelled a floor")
	}
	if !strings.Contains(s, "must be ADDED to this") {
		t.Error("the composed total does not say the unmeasured terms must be added to it")
	}
	if !strings.Contains(s, "ALSA capture period") {
		t.Error("the composed total does not say the framing term is a synthetic frame, not a real ALSA period")
	}
}
