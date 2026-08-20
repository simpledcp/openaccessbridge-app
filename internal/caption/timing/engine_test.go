package timing

import (
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(10000, 0)} }
func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

type recorder struct {
	mu     sync.Mutex
	events []CueEvent
}

func (r *recorder) on(ev CueEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}
func (r *recorder) last() (CueEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return CueEvent{}, false
	}
	return r.events[len(r.events)-1], true
}
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func testCues() []model.Cue {
	return []model.Cue{
		{StartMS: 1000, EndMS: 2000, Text: "A", Language: "en"},
		{StartMS: 3000, EndMS: 4000, Text: "B", Language: "en"},
	}
}

func newEngine(clk *fakeClock, rec *recorder) *Engine {
	return New(Options{Clock: clk.now, OnChange: rec.on})
}

// nextBoundaryAt reads the scheduler's next-boundary decision for a position,
// which is otherwise only observable through real-timer wall-clock behavior.
func nextBoundaryAt(e *Engine, pos int64) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nextBoundaryLocked(pos)
}

func TestActiveCueResolutionAcrossBoundaries(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0) // anchor at server pos 0, clock = base

	// At pos 0: no cue (first at 1000).
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("expected nothing visible at pos 0, got %+v", ev)
	}

	clk.advance(1000 * time.Millisecond) // pos 1000
	e.Evaluate()
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "A" || ev.Reason != "cue_in" {
		t.Fatalf("expected cue_in A at 1000, got %+v", ev)
	}

	clk.advance(999 * time.Millisecond) // pos 1999
	n := rec.count()
	e.Evaluate()
	if rec.count() != n {
		t.Fatalf("unexpected event mid-cue: %+v", rec.events)
	}

	clk.advance(1 * time.Millisecond) // pos 2000 → A ends [1000,2000)
	e.Evaluate()
	if ev, _ := rec.last(); ev.Visible || ev.Reason != "cue_out" {
		t.Fatalf("expected cue_out at 2000, got %+v", ev)
	}

	clk.advance(1000 * time.Millisecond) // pos 3000 → B
	e.Evaluate()
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "B" {
		t.Fatalf("expected B at 3000, got %+v", ev)
	}
}

func TestPauseFreezesResumeReanchors(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(1500 * time.Millisecond) // pos 1500 → A visible
	e.Evaluate()
	if ev, _ := rec.last(); ev.Cue.Text != "A" {
		t.Fatalf("expected A, got %+v", ev)
	}

	e.Pause()
	if got := e.Position(); got != 1500 {
		t.Fatalf("paused position = %d, want 1500", got)
	}
	clk.advance(10 * time.Second) // huge wall advance while paused
	if got := e.Position(); got != 1500 {
		t.Fatalf("position advanced while paused: %d", got)
	}
	e.Evaluate()
	if ev, _ := rec.last(); ev.Cue.Text != "A" {
		t.Fatalf("cue should still be A while paused, got %+v", ev)
	}

	e.Resume()
	clk.advance(600 * time.Millisecond) // pos 1500+600 = 2100 → A ended at 2000
	e.Evaluate()
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("expected A cleared after resume past end, got %+v", ev)
	}
}

func TestSeekDiscardsPrediction(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(1500 * time.Millisecond)
	e.Evaluate() // A visible

	e.SeekTo(3500) // jump into B's interval
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "B" {
		t.Fatalf("expected B after seek to 3500, got %+v", ev)
	}
	if got := e.Position(); got != 3500 {
		t.Fatalf("position after seek = %d, want 3500", got)
	}
}

func TestStopBlanks(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(1500 * time.Millisecond)
	e.Evaluate() // A visible
	e.Stop()
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("expected blank after stop, got %+v", ev)
	}
}

// TestCommsLossContinuesUntilLeaseExpiry is the §5.2-item-2 reliability behavior:
// a momentary comms loss must NOT blank captions; only an actual lease expiry does.
func TestCommsLossContinuesUntilLeaseExpiry(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(1200 * time.Millisecond) // A visible at 1200
	e.Evaluate()
	if ev, _ := rec.last(); ev.Cue.Text != "A" {
		t.Fatalf("expected A, got %+v", ev)
	}

	e.CommsLost()
	clk.advance(300 * time.Millisecond) // pos 1500 still within A; prediction continues
	e.Evaluate()
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "A" {
		t.Fatalf("captions must keep showing across comms loss, got %+v", ev)
	}

	e.LeaseExpired()
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("captions must blank on lease expiry, got %+v", ev)
	}
}

func TestDriftMeasurement(t *testing.T) {
	clk := newFakeClock()
	e := New(Options{Clock: clk.now, WarnBudgetMS: 500})
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(10 * time.Second) // predicted pos = 10000
	e.UpdateTimeline(9800)        // server says 9800 → drift +200
	if d := e.DriftMS(); d != 200 {
		t.Fatalf("drift = %d, want 200", d)
	}
	if e.OverBudget() {
		t.Fatalf("200ms drift should be within 500ms budget")
	}
	clk.advance(2 * time.Second) // predicted ~11800
	e.UpdateTimeline(11000)      // drift +800 → over budget
	if !e.OverBudget() {
		t.Fatalf("800ms drift should exceed 500ms budget, got %d", e.DriftMS())
	}
}

// TestOverlappingCueOutIsScheduled is the §5.5 push-on-change regression: with an
// overlapping pair the boundary scheduler must still arm on the OUTER cue's end,
// or the caption freezes on screen until the next timeline message arrives.
func TestOverlappingCueOutIsScheduled(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	// B is nested inside A; sorted by start the outer cue comes first.
	e.SetCues("en", []model.Cue{
		{StartMS: 0, EndMS: 300, Text: "A", Language: "en"},
		{StartMS: 50, EndMS: 100, Text: "B", Language: "en"},
	})
	e.Play(0)

	tests := []struct {
		atMS    int64
		want    int64 // expected next boundary, -1 for none
		visible string
	}{
		{atMS: 0, want: 50, visible: "A"},
		{atMS: 50, want: 100, visible: "B"},
		{atMS: 100, want: 300, visible: "A"}, // A's cue-out must still be scheduled
		{atMS: 300, want: -1, visible: ""},
	}
	for _, tt := range tests {
		clk.advance(time.Duration(tt.atMS-e.Position()) * time.Millisecond)
		e.Evaluate()
		if got := nextBoundaryAt(e, tt.atMS); got != tt.want {
			t.Errorf("nextBoundary at %d = %d, want %d", tt.atMS, got, tt.want)
		}
		ev, _ := rec.last()
		got := ""
		if ev.Visible {
			got = ev.Cue.Text
		}
		if got != tt.visible {
			t.Errorf("at %d visible = %q, want %q", tt.atMS, got, tt.visible)
		}
	}
	if st := e.Status(); st.Visible {
		t.Fatalf("captions must clear at A's end, got %+v", st)
	}
}

// TestLeaseExpiryRecoversOnFreshTimeline is §5.5: a fresh Update Timeline (only
// possible over a live lease) re-anchors and resumes. Without it a renewed lease
// leaves the phones dark until the next Play.
func TestLeaseExpiryRecoversOnFreshTimeline(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(1000) // inside cue A
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "A" {
		t.Fatalf("expected A at 1000, got %+v", ev)
	}

	e.LeaseExpired()
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("lease expiry must blank, got %+v", ev)
	}

	// Lease renewed: the server sends a fresh timeline position.
	e.UpdateTimeline(1500)
	st := e.Status()
	if st.State != "playing" || !st.Playing || !st.Visible {
		t.Fatalf("fresh timeline must recover from lease expiry, got %+v", st)
	}
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "A" {
		t.Fatalf("expected A back after renewal, got %+v", ev)
	}
	if d := e.DriftMS(); d != 0 {
		t.Fatalf("re-anchor after a blank must not invent drift, got %d", d)
	}
}

// TestCompositionChangedDiscardsPrediction is the §5.5 SPL/CPL-change event: the
// new composition's cues must never be resolved against the old composition's
// anchor, so the prediction is discarded (seek semantics) until a fresh timeline.
func TestCompositionChangedDiscardsPrediction(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", testCues())
	e.Play(0)
	clk.advance(3500 * time.Millisecond) // pos 3500 → cue B visible
	e.Evaluate()
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "B" {
		t.Fatalf("expected B at 3500, got %+v", ev)
	}

	e.CompositionChanged()
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("composition change must blank, got %+v", ev)
	}
	if st := e.Status(); st.PositionMS != 0 || st.Playing {
		t.Fatalf("prediction must be discarded, got %+v", st)
	}

	// The new composition's cues load; they must NOT resolve at the stale 3500.
	e.SetCues("en", []model.Cue{{StartMS: 3000, EndMS: 4000, Text: "NEW", Language: "en"}})
	clk.advance(10 * time.Second)
	e.Evaluate()
	if st := e.Status(); st.Visible {
		t.Fatalf("new cues must not resolve against the old anchor, got %+v", st)
	}

	// The next Update Timeline re-anchors and captions return.
	e.UpdateTimeline(3200)
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "NEW" {
		t.Fatalf("expected NEW after re-anchor, got %+v", ev)
	}
}

// TestCompositionChangedDiscardsTheOldCueSet is the other half of the §5.5
// SPL/CPL event: clearing only the anchor is not enough. If the incoming reel has
// not landed yet — fetch in flight, or the fetch failed — the outgoing
// composition's cues are still loaded, and the next Update Timeline resolves
// against them and puts the PREVIOUS show's captions back on screen.
func TestCompositionChangedDiscardsTheOldCueSet(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	e := newEngine(clk, rec)
	e.SetCues("en", []model.Cue{{StartMS: 1000, EndMS: 9000, Text: "OLDSHOW", Language: "en"}})
	e.Play(0)
	clk.advance(2 * time.Second)
	e.Evaluate()
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "OLDSHOW" {
		t.Fatalf("setup: expected OLDSHOW at 2000, got %+v", ev)
	}

	e.CompositionChanged()
	if st := e.Status(); st.CueCount != 0 {
		t.Fatalf("the outgoing composition's cues must be discarded, got %+v", st)
	}

	// The new reel has NOT loaded. A fresh timeline arrives anyway.
	e.UpdateTimeline(2000)
	if st := e.Status(); st.Visible {
		t.Fatalf("the previous composition's caption must not come back, got %+v", st)
	}
	if ev, _ := rec.last(); ev.Visible {
		t.Fatalf("no cue may be pushed before the new reel loads, got %+v", ev)
	}

	// Once the new composition's cues arrive they resolve normally.
	e.SetCues("en", []model.Cue{{StartMS: 1500, EndMS: 4000, Text: "NEWSHOW", Language: "en"}})
	if ev, _ := rec.last(); !ev.Visible || ev.Cue.Text != "NEWSHOW" {
		t.Fatalf("expected NEWSHOW once the reel lands, got %+v", ev)
	}
}

// TestBudgetDefaultsAndOverride pins D9: the warn budget defaults to 500 ms and
// the target to 400 ms, both configurable, both surfaced to diagnostics.
func TestBudgetDefaultsAndOverride(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		wantWarn   int64
		wantTarget int64
	}{
		{name: "d9_defaults", opts: Options{}, wantWarn: DefaultWarnBudgetMS, wantTarget: DefaultTargetBudgetMS},
		{name: "configured", opts: Options{WarnBudgetMS: 250, TargetBudgetMS: 200}, wantWarn: 250, wantTarget: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			tt.opts.Clock = clk.now
			e := New(tt.opts)
			st := e.Status()
			if st.WarnBudgetMS != tt.wantWarn || st.TargetBudgetMS != tt.wantTarget {
				t.Fatalf("budgets = %d/%d, want %d/%d",
					st.WarnBudgetMS, st.TargetBudgetMS, tt.wantWarn, tt.wantTarget)
			}
			e.SetCues("en", testCues())
			e.Play(0)
			clk.advance(10 * time.Second)
			e.UpdateTimeline(10000 - tt.wantWarn - 1) // drift = budget + 1
			if !e.OverBudget() || !e.Status().OverBudget {
				t.Fatalf("drift %d must exceed budget %d", e.DriftMS(), tt.wantWarn)
			}
		})
	}
}

// TestSchedulerPushesOnRealTimer is a small real-time smoke test of the boundary
// scheduler: with the real clock and a near-future cue, OnChange fires without any
// manual Evaluate (proving push-on-change works end to end).
func TestSchedulerPushesOnRealTimer(t *testing.T) {
	rec := &recorder{}
	e := New(Options{OnChange: rec.on}) // real time.Now clock
	e.Start()
	defer e.Close()
	now := e.Position() // ~0
	e.SetCues("en", []model.Cue{{StartMS: now + 40, EndMS: now + 5000, Text: "X"}})
	e.Play(now)
	deadline := time.After(2 * time.Second)
	for {
		if ev, ok := rec.last(); ok && ev.Visible && ev.Cue.Text == "X" {
			return // success: timer fired and pushed the cue-in
		}
		select {
		case <-deadline:
			t.Fatalf("scheduler did not push cue-in within 2s (events=%d)", rec.count())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
