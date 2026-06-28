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
