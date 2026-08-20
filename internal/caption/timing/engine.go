// Package timing is the caption timing engine (buildplan.md §5.5): the single most
// important caption-quality component. It continuously maps the server playback
// timeline to a local monotonic clock and resolves the currently-active cue —
// never a "sleep until next cue" loop.
//
// Approach: on each timeline update we capture an anchor (server_position,
// local_monotonic_timestamp); between updates we predict the server position from
// the monotonic delta. The active cue is resolved by interval lookup and pushed
// ONLY on change (cue-in / cue-out / text change). Boundaries are scheduled with a
// single re-arming timer aimed at the next cue edge, so the fan-out stays quiet
// and the phone UI stable — there is no fixed tick and no blocking sleep loop.
package timing

import (
	"sort"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
)

// Drift/latency budget defaults (§5.5 "configurable drift/latency budget", D9):
// target captions on screen within 400 ms of the reference, warn above 500 ms.
// Both sit inside the plan's 250–500 ms working figure and are overridable via
// Options so a venue (or a calibration against a real server) can tighten them.
const (
	DefaultTargetBudgetMS int64 = 400
	DefaultWarnBudgetMS   int64 = 500
)

// Clock returns the current time including a monotonic reading. time.Now
// satisfies this; tests inject a fake clock. Monotonic deltas are immune to NTP
// steps (§5.5).
type Clock func() time.Time

// CueEvent is emitted when the active cue changes.
type CueEvent struct {
	Visible bool      // true = a cue is on screen; false = cleared
	Cue     model.Cue // the active cue when Visible
	Reason  string    // cue_in | cue_out | text_change | timeline_event | cleared
}

// Engine maps a server timeline to active cues. It is safe for concurrent use.
type Engine struct {
	mu    sync.Mutex
	clock Clock

	anchorServerMS int64     // server position at the anchor
	anchorMono     time.Time // local monotonic timestamp at the anchor
	rate           float64   // 1.0 playing, 0.0 paused/stopped/expired
	state          string    // playing | paused | stopped | lease_expired | idle
	commsLost      bool

	cues        []model.Cue // sorted by StartMS, for the current reel+language
	language    string
	activeIndex int  // index into cues of the active cue, or -1
	visible     bool // whether a cue is currently shown
	driftMS     int64

	// warnBudgetMS is the configured drift/latency budget; drift beyond it is
	// surfaced (D9). 0 disables the warning. targetBudgetMS is the design goal
	// carried alongside for diagnostics.
	warnBudgetMS   int64
	targetBudgetMS int64

	onChange func(CueEvent)

	// timer scheduling for production push-on-change.
	timer   *time.Timer
	stopCh  chan struct{}
	running bool
}

// Options configures an Engine.
type Options struct {
	Clock    Clock
	OnChange func(CueEvent)
	// WarnBudgetMS / TargetBudgetMS are the configurable drift budget (§5.5).
	// Zero takes the D9 defaults.
	WarnBudgetMS   int64
	TargetBudgetMS int64
}

// New constructs an Engine. If Clock is nil, time.Now is used.
func New(opts Options) *Engine {
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	wb := opts.WarnBudgetMS
	if wb == 0 {
		wb = DefaultWarnBudgetMS
	}
	tb := opts.TargetBudgetMS
	if tb == 0 {
		tb = DefaultTargetBudgetMS
	}
	return &Engine{
		clock:          clk,
		rate:           0,
		state:          "idle",
		activeIndex:    -1,
		onChange:       opts.OnChange,
		warnBudgetMS:   wb,
		targetBudgetMS: tb,
	}
}

// SetCues replaces the active cue set (one reel + language). Cues are copied and
// sorted by start time. The active cue is re-resolved.
func (e *Engine) SetCues(language string, cues []model.Cue) {
	e.mu.Lock()
	cp := make([]model.Cue, len(cues))
	copy(cp, cues)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].StartMS < cp[j].StartMS })
	e.cues = cp
	e.language = language
	e.activeIndex = -1
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// --- Timeline events (§5.5) ---

// UpdateTimeline re-anchors to a fresh server position while playing, measuring
// drift against the prior prediction (§5.5 re-anchoring).
func (e *Engine) UpdateTimeline(serverPosMS int64) {
	e.mu.Lock()
	if e.rate != 0 && !e.anchorMono.IsZero() {
		predicted := e.positionLocked()
		e.driftMS = predicted - serverPosMS
	}
	e.anchorServerMS = serverPosMS
	e.anchorMono = e.clock()
	// A fresh timeline message re-anchors and resumes from every state that is
	// merely WAITING for one (§5.5): idle (never anchored, or the composition
	// changed), stopped, and lease_expired — an Update Timeline can only arrive
	// over a live lease, so its arrival means the lease was renewed and captions
	// must come back. "paused" is deliberately excluded: only Resume unfreezes.
	switch e.state {
	case "idle", "stopped", "lease_expired":
		e.state = "playing"
		e.rate = 1
	case "playing":
		e.rate = 1
	}
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// Play starts playback at serverPosMS.
func (e *Engine) Play(serverPosMS int64) {
	e.transition("playing", 1, &serverPosMS)
}

// Pause freezes prediction at the current position.
func (e *Engine) Pause() {
	e.mu.Lock()
	pos := e.positionLocked()
	e.anchorServerMS = pos
	e.anchorMono = e.clock()
	e.rate = 0
	e.state = "paused"
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// Resume re-anchors at the frozen position and continues.
func (e *Engine) Resume() {
	e.mu.Lock()
	e.anchorMono = e.clock()
	e.rate = 1
	e.state = "playing"
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// Stop halts playback and blanks captions.
func (e *Engine) Stop() {
	e.mu.Lock()
	e.rate = 0
	e.state = "stopped"
	e.activeIndex = -1
	ev := e.blankLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// Seek discards the prediction and re-anchors at the new position (§5.5).
func (e *Engine) SeekTo(serverPosMS int64) {
	e.mu.Lock()
	e.anchorServerMS = serverPosMS
	e.anchorMono = e.clock()
	e.activeIndex = -1 // force a fresh resolution; a seek is not a cue transition
	if e.state == "idle" || e.state == "stopped" {
		e.state = "playing"
		e.rate = 1
	}
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// CompositionChanged signals an SPL/CPL change — a different composition or
// playlist arriving underneath the current lease, without a Terminate Lease
// (§5.5 explicit timeline events). It takes seek semantics to the limit: the
// previous composition's prediction is discarded entirely (anchor cleared,
// captions blanked) so the incoming cue set is never resolved against the old
// composition's timeline. The engine returns to "idle" and re-anchors on the
// next Update Timeline.
//
// The outgoing composition's CUE SET is discarded with the anchor. It has to be:
// clearing only the anchor leaves the old cues loaded, and the resolver has no
// way to tell them apart from the new composition's, so the next Update Timeline
// would put the PREVIOUS show's captions back on screen — the exact wrong-content
// failure this event exists to prevent. It matters whenever the incoming reel has
// not landed yet (fetch in flight, or the fetch failed): the honest state is "no
// captions", which the patron status already says, not the last show's text.
// SetCues installs the new composition's cues.
func (e *Engine) CompositionChanged() {
	e.mu.Lock()
	e.anchorServerMS = 0
	e.anchorMono = time.Time{}
	e.rate = 0
	e.state = "idle"
	e.driftMS = 0 // drift against a discarded timeline means nothing
	e.activeIndex = -1
	e.cues = nil
	ev := e.blankLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// CommsLost records a momentary communications interruption. Per §5.2 item 2 /
// §5.5, prediction CONTINUES from the last anchor — captions keep showing across a
// brief blip; only LeaseExpired blanks them.
func (e *Engine) CommsLost() {
	e.mu.Lock()
	e.commsLost = true
	e.mu.Unlock()
}

// CommsRestored clears the comms-loss flag (a fresh UpdateTimeline re-anchors).
func (e *Engine) CommsRestored() {
	e.mu.Lock()
	e.commsLost = false
	e.mu.Unlock()
}

// LeaseExpired freezes prediction and blanks captions — only now, after the lease
// genuinely expires, not on mere comms loss (§5.2 item 2).
func (e *Engine) LeaseExpired() {
	e.mu.Lock()
	e.rate = 0
	e.state = "lease_expired"
	e.activeIndex = -1
	ev := e.blankLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

func (e *Engine) transition(state string, rate float64, posMS *int64) {
	e.mu.Lock()
	if posMS != nil {
		e.anchorServerMS = *posMS
	} else {
		e.anchorServerMS = e.positionLocked()
	}
	e.anchorMono = e.clock()
	e.rate = rate
	e.state = state
	ev := e.reevaluateLocked("timeline_event")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
}

// --- Position + resolution ---

// Position returns the predicted server timeline position in ms.
func (e *Engine) Position() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.positionLocked()
}

func (e *Engine) positionLocked() int64 {
	if e.anchorMono.IsZero() {
		return e.anchorServerMS
	}
	if e.rate == 0 {
		return e.anchorServerMS
	}
	delta := e.clock().Sub(e.anchorMono).Seconds() * 1000.0 * e.rate
	return e.anchorServerMS + int64(delta+0.5)
}

// resolveActiveLocked returns the index of the cue active at posMS, or -1. Cues
// are sorted by start; it checks the rightmost cue whose StartMS <= posMS and a
// small look-back window so an overlapping earlier cue can still win.
func (e *Engine) resolveActiveLocked(posMS int64) int {
	idx := sort.Search(len(e.cues), func(i int) bool { return e.cues[i].StartMS > posMS }) - 1
	for i := idx; i >= 0 && i >= idx-8; i-- {
		if e.cues[i].ContainsMS(posMS) {
			return i
		}
	}
	return -1
}

// Evaluate recomputes the active cue from the current clock and emits an OnChange
// event if it changed. Tests drive this directly with a fake clock; production
// also calls it from the boundary timer. Returns the event that was fired (or a
// zero CueEvent with empty Reason if nothing changed).
func (e *Engine) Evaluate() CueEvent {
	e.mu.Lock()
	ev := e.reevaluateLocked("")
	e.rearmLocked()
	e.mu.Unlock()
	e.fire(ev)
	return ev
}

// reevaluateLocked recomputes active cue and returns a change event (Reason set)
// or a zero event (Reason empty) if unchanged. defaultReason labels timeline-driven
// changes; natural boundary changes use cue_in / cue_out / text_change.
func (e *Engine) reevaluateLocked(defaultReason string) CueEvent {
	// Captions are shown only while playing or paused (paused freezes the cue).
	// stopped / lease_expired / idle show nothing.
	showing := e.state == "playing" || e.state == "paused"
	newIdx := -1
	if showing {
		newIdx = e.resolveActiveLocked(e.positionLocked())
	}

	prevIdx, prevVisible := e.activeIndex, e.visible
	newVisible := newIdx != -1

	if newIdx == prevIdx && newVisible == prevVisible {
		return CueEvent{} // nothing changed
	}

	e.activeIndex = newIdx
	e.visible = newVisible

	if !newVisible {
		reason := "cue_out"
		if defaultReason != "" {
			reason = defaultReason
		}
		return CueEvent{Visible: false, Reason: reason}
	}
	reason := "cue_in"
	if prevVisible {
		reason = "text_change" // one cue replaced another with no gap
	}
	if defaultReason != "" {
		reason = defaultReason
	}
	return CueEvent{Visible: true, Cue: e.cues[newIdx], Reason: reason}
}

func (e *Engine) blankLocked(reason string) CueEvent {
	wasVisible := e.visible
	e.visible = false
	e.activeIndex = -1
	if !wasVisible {
		return CueEvent{}
	}
	return CueEvent{Visible: false, Reason: reason}
}

func (e *Engine) fire(ev CueEvent) {
	if ev.Reason == "" || e.onChange == nil {
		return
	}
	e.onChange(ev)
}

// --- Boundary scheduling (production push-on-change, no sleep loop) ---

// Start launches the boundary scheduler goroutine. It arms a single timer at the
// next cue edge; on fire it re-evaluates and re-arms. Safe to call once.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.stopCh = make(chan struct{})
	e.rearmLocked()
	e.mu.Unlock()
}

// Stop scheduler.
func (e *Engine) Close() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	if e.timer != nil {
		e.timer.Stop()
	}
	close(e.stopCh)
	e.mu.Unlock()
}

// rearmLocked computes the next boundary and arms the timer for it. Called under
// lock. No-op unless the scheduler is running.
func (e *Engine) rearmLocked() {
	if !e.running {
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.rate == 0 || len(e.cues) == 0 {
		return // nothing advancing; next event will be a timeline command
	}
	pos := e.positionLocked()
	next := e.nextBoundaryLocked(pos)
	if next < 0 {
		return
	}
	dur := time.Duration(float64(next-pos)/e.rate) * time.Millisecond
	if dur < 0 {
		dur = 0
	}
	e.timer = time.AfterFunc(dur, func() { e.Evaluate() })
}

// nextBoundaryLocked returns the next cue edge (in server ms) strictly after pos,
// or -1 if none.
func (e *Engine) nextBoundaryLocked(pos int64) int64 {
	best := int64(-1)
	consider := func(t int64) {
		if t > pos && (best == -1 || t < best) {
			best = t
		}
	}
	// Only need to look at cues near pos, but the window must LOOK BACK by the
	// same amount as resolveActiveLocked: with overlapping cues the active one
	// can start earlier than idx, and its EndMS is then the next boundary. Scan
	// forward only and that cue-out is never scheduled — the caption freezes on
	// screen until the next timeline message (§5.5 push on cue-out).
	idx := sort.Search(len(e.cues), func(i int) bool { return e.cues[i].StartMS > pos }) - 1
	if idx < 0 {
		idx = 0
	}
	lo := idx - 8
	if lo < 0 {
		lo = 0
	}
	for i := lo; i < len(e.cues) && i <= idx+8; i++ {
		consider(e.cues[i].StartMS)
		consider(e.cues[i].EndMS)
	}
	return best
}

// Status is a diagnostic snapshot of the engine.
type Status struct {
	PositionMS int64
	State      string
	Playing    bool
	Visible    bool
	DriftMS    int64
	Language   string
	CueCount   int
	// The configured drift budget and whether the last measurement broke it, so
	// diagnostics can show the number against the limit it is judged by (§5.5).
	WarnBudgetMS   int64
	TargetBudgetMS int64
	OverBudget     bool
}

// Status returns a diagnostic snapshot (no caption text).
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Status{
		PositionMS:     e.positionLocked(),
		State:          e.state,
		Playing:        e.rate != 0,
		Visible:        e.visible,
		DriftMS:        e.driftMS,
		Language:       e.language,
		CueCount:       len(e.cues),
		WarnBudgetMS:   e.warnBudgetMS,
		TargetBudgetMS: e.targetBudgetMS,
		OverBudget:     e.overBudgetLocked(),
	}
}

// DriftMS returns the last measured drift (predicted − server) at the most recent
// re-anchor. Positive means the local prediction was ahead of the server.
func (e *Engine) DriftMS() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.driftMS
}

// OverBudget reports whether the absolute drift exceeds the configured warn budget.
func (e *Engine) OverBudget() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.overBudgetLocked()
}

func (e *Engine) overBudgetLocked() bool {
	d := e.driftMS
	if d < 0 {
		d = -d
	}
	return e.warnBudgetMS > 0 && d > e.warnBudgetMS
}

// WarnBudgetMS returns the configured drift warn threshold (§5.5 / D9).
func (e *Engine) WarnBudgetMS() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.warnBudgetMS
}
