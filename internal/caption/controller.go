// Package caption wires the caption path together: it drives one timing engine
// per language from a single server timeline and fans the resulting active-cue
// changes out to patron browsers (buildplan.md §5). It is the integration point
// for both the Phase-1 offline player (fake show clock) and the ST 430-10 state
// machine (real lease/timeline), so the timeline-handling and multi-language
// fan-out live in exactly one place.
package caption

import (
	"sort"
	"sync"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/caption/timing"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Controller maps a single timeline onto per-language timing engines and the hub.
// A cue-less "reference" engine tracks the timeline so a language engine created
// mid-show (e.g. when the RPL arrives after Update Timeline) inherits the current
// play state and position rather than starting idle.
type Controller struct {
	mu      sync.Mutex
	hub     *fanout.Hub
	clock   timing.Clock
	budget  int64
	ref     *timing.Engine
	engines map[string]*timing.Engine
	langs   []string
	started bool
}

// Options configures a Controller.
type Options struct {
	Hub          *fanout.Hub
	Clock        timing.Clock // nil → time.Now
	WarnBudgetMS int64
}

// NewController builds a Controller.
func NewController(opts Options) *Controller {
	return &Controller{
		hub:     opts.Hub,
		clock:   opts.Clock,
		budget:  opts.WarnBudgetMS,
		ref:     timing.New(timing.Options{Clock: opts.Clock, WarnBudgetMS: opts.WarnBudgetMS}),
		engines: make(map[string]*timing.Engine),
	}
}

// LoadReel replaces the active cue set for the current reel/composition, keyed by
// language. Each language gets its own timing engine driven by the shared
// timeline. Languages absent from the new set have their engines retired. A newly
// created engine is seeded from the timeline reference so it shows the correct cue
// immediately even if the RPL arrived after playback started.
func (c *Controller) LoadReel(cuesByLang map[string][]model.Cue) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for lang, eng := range c.engines {
		if _, ok := cuesByLang[lang]; !ok {
			eng.Close()
			delete(c.engines, lang)
			// The retired engine will never push again, so the hub must blank
			// the language itself or its subscribers keep the last reel's cue
			// on screen forever (§5.2 item 5).
			c.hub.ClearLanguage(lang)
		}
	}

	c.langs = c.langs[:0]
	ref := c.ref.Status()
	for lang, cues := range cuesByLang {
		c.langs = append(c.langs, lang)
		eng, existed := c.engines[lang]
		if !existed {
			l := lang
			eng = timing.New(timing.Options{
				Clock:        c.clock,
				WarnBudgetMS: c.budget,
				OnChange: func(ev timing.CueEvent) {
					c.hub.PublishCue(l, ev.Visible, ev.Cue)
				},
			})
			if c.started {
				eng.Start()
			}
			c.engines[lang] = eng
		}
		// The ST 429-12 grade rides on the cues; publish it so the patron copy is
		// right ("Closed captions" vs "Subtitles available") from reel load, not
		// only once the first cue fires (§5.4).
		c.hub.SetTrackKind(lang, trackKind(cues))
		eng.SetCues(lang, cues)
		if !existed {
			seedFromRef(eng, ref)
		}
	}
	sort.Strings(c.langs)
}

// trackKind reports the most accessibility-preferred track kind present in a cue
// set, so a language backed by a ClosedCaption track is labeled caption-grade
// even if the reel mixes kinds (§5.4 track-selection model).
func trackKind(cues []model.Cue) model.TrackKind {
	best := model.TrackUnknown
	for _, c := range cues {
		if c.Kind.AccessibilityRank() < best.AccessibilityRank() {
			best = c.Kind
		}
	}
	return best
}

// seedFromRef applies the reference engine's play state/position to a fresh engine.
func seedFromRef(eng *timing.Engine, ref timing.Status) {
	switch ref.State {
	case "playing":
		eng.Play(ref.PositionMS)
	case "paused":
		eng.Play(ref.PositionMS)
		eng.Pause()
	}
}

// applyTimeline forwards a timeline operation to the reference engine and every
// language engine.
func (c *Controller) applyTimeline(fn func(*timing.Engine)) {
	c.mu.Lock()
	engs := make([]*timing.Engine, 0, len(c.engines)+1)
	engs = append(engs, c.ref)
	for _, e := range c.engines {
		engs = append(engs, e)
	}
	c.mu.Unlock()
	for _, e := range engs {
		fn(e)
	}
}

// contentEngines returns only the per-language engines (excludes the reference).
func (c *Controller) contentEngines() []*timing.Engine {
	c.mu.Lock()
	defer c.mu.Unlock()
	engs := make([]*timing.Engine, 0, len(c.engines))
	for _, e := range c.engines {
		engs = append(engs, e)
	}
	return engs
}

// Start launches the boundary schedulers so cue changes are pushed in real time.
func (c *Controller) Start() {
	c.mu.Lock()
	c.started = true
	c.ref.Start()
	for _, eng := range c.engines {
		eng.Start()
	}
	c.mu.Unlock()
}

// Close stops all engines.
func (c *Controller) Close() {
	c.mu.Lock()
	c.ref.Close()
	for _, eng := range c.engines {
		eng.Close()
	}
	c.mu.Unlock()
}

// --- Timeline events, forwarded to the reference + every language engine (§5.5) ---

// Play starts playback at posMS and surfaces an active patron status.
func (c *Controller) Play(posMS int64) {
	c.applyTimeline(func(e *timing.Engine) { e.Play(posMS) })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// UpdateTimeline re-anchors all engines to a fresh server position.
func (c *Controller) UpdateTimeline(posMS int64) {
	c.applyTimeline(func(e *timing.Engine) { e.UpdateTimeline(posMS) })
}

// Pause freezes captions.
func (c *Controller) Pause() {
	c.applyTimeline(func(e *timing.Engine) { e.Pause() })
	c.hub.PublishStatus(model.PatronCaptionWaiting)
}

// Resume continues captions.
func (c *Controller) Resume() {
	c.applyTimeline(func(e *timing.Engine) { e.Resume() })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// Stop halts captions.
func (c *Controller) Stop() {
	c.applyTimeline(func(e *timing.Engine) { e.Stop() })
	c.hub.PublishStatus(model.PatronCaptionWaiting)
}

// SeekTo jumps all engines to posMS.
func (c *Controller) SeekTo(posMS int64) {
	c.applyTimeline(func(e *timing.Engine) { e.SeekTo(posMS) })
}

// CompositionChanged handles an SPL/CPL change mid-lease (§5.5): a new
// composition arriving without a Terminate Lease. Every engine discards its
// prediction, so the incoming reel's cues are never resolved against the
// outgoing composition's timeline; the next Update Timeline re-anchors. Patrons
// see the waiting status in the meantime rather than a stale cue.
func (c *Controller) CompositionChanged() {
	c.applyTimeline(func(e *timing.Engine) { e.CompositionChanged() })
	c.hub.PublishStatus(model.PatronCaptionWaiting)
}

// CommsLost signals a momentary interruption; captions keep showing (§5.2 item 2).
func (c *Controller) CommsLost() {
	c.applyTimeline(func(e *timing.Engine) { e.CommsLost() })
	c.hub.PublishStatus(model.PatronCaptionReconnecting)
}

// CommsRestored clears the interruption.
func (c *Controller) CommsRestored() {
	c.applyTimeline(func(e *timing.Engine) { e.CommsRestored() })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// LeaseExpired blanks captions (only on genuine lease expiry, §5.2 item 2).
func (c *Controller) LeaseExpired() {
	c.applyTimeline(func(e *timing.Engine) { e.LeaseExpired() })
	c.hub.PublishStatus(model.PatronCaptionUnavailable)
}

// SetPatronStatus lets the ACSP layer drive the patron-facing status directly.
func (c *Controller) SetPatronStatus(s model.PatronCaptionStatus) {
	c.hub.PublishStatus(s)
}

// Evaluate forces an immediate active-cue re-resolution on every engine. Useful
// for the ACSP status loop and for deterministic testing with an injected clock.
func (c *Controller) Evaluate() {
	for _, e := range c.contentEngines() {
		e.Evaluate()
	}
	c.ref.Evaluate()
}

// Languages returns the currently loaded languages, sorted so the list is stable
// across calls (LoadReel's source map has no order of its own).
func (c *Controller) Languages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.langs))
	copy(out, c.langs)
	return out
}

// Timeline reports §8.2's "Timeline: playing / paused / stopped" row.
//
// It reads the REFERENCE engine, which is the one thing that tracks the server
// timeline whether or not any language is loaded, and which the ACSP layer
// drives through Play/Pause/Stop/Resume. That makes this a measurement of the
// transport state the caption path is actually running on, not an inference from
// the handshake state: a link can hold a lease and a position while the server
// has never said whether it is rolling.
//
// "idle" and "lease_expired" both report no timeline. Neither is a transport
// state the server sent, and a lease-expired link is no longer being told one at
// all, so echoing the last value would be reporting a stale fact as current
// (guardrail #7).
func (c *Controller) Timeline() model.TimelineState {
	switch c.ref.Status().State {
	case "playing":
		return model.TimelinePlaying
	case "paused":
		return model.TimelinePaused
	case "stopped":
		return model.TimelineStopped
	default:
		return model.TimelineNone
	}
}

// DriftMeasured reports whether MaxDriftMS is a MEASUREMENT rather than a
// vacuous zero.
//
// MaxDriftMS ranges over the per-language engines, so a caption path with no
// language loaded returns 0 by having nothing to range over. The dashboard row
// is an int64 with no "unset", so without this the same "0 ms" is printed for
// perfect sync and for a link that is not running at all — a plausible number
// standing in for an unknown, which is the failure the honest-measurement waves
// exist to prevent (guardrail #7).
//
// SEAM: an engine that IS loaded but has not yet re-anchored also holds drift 0,
// and internal/caption/timing records no flag for "a drift measurement has
// happened" (it is assigned only in Engine.UpdateTimeline, and only when the
// engine was already anchored and rolling). Closing that window wants a
// driftMeasured flag inside the timing engine, which this lane does not own; the
// dashboard note names the window instead of hiding it.
func (c *Controller) DriftMeasured() bool { return len(c.contentEngines()) > 0 }

// MaxDriftMS returns the largest absolute drift across language engines (§5.5).
func (c *Controller) MaxDriftMS() int64 {
	var max int64
	for _, e := range c.contentEngines() {
		d := e.DriftMS()
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}

// LastAnchorMonotonicNS is the most recent re-anchor across the language
// engines, or 0 when nothing has anchored (§5.5, §10.3).
//
// The MOST RECENT, because §10.3's field answers "when did the timeline last get
// a fix from the server" — and a tech reading it after a drift complaint wants
// the freshest anchor, not the staleest. Zero means no engine has ever anchored,
// which is a real state (nothing has played yet) and not a timestamp.
func (c *Controller) LastAnchorMonotonicNS() int64 {
	// The REFERENCE engine, for the same reason §8.2's timeline row reads it:
	// it is the thing the server's transport state actually drives. The
	// per-language content engines only exist once reels have loaded, so taking
	// the anchor from them would report "never anchored" for every show between
	// the first Update Timeline and the first loaded language — which is exactly
	// the window a tech investigating a slow start is looking at.
	return c.ref.Status().AnchorMonotonicNS
}

// WarnBudgetMS returns the configured drift warn threshold (§5.5 / D9). Zero in
// Options takes the timing package's D9 default, which is what this reports.
func (c *Controller) WarnBudgetMS() int64 { return c.ref.WarnBudgetMS() }

// OverBudget reports whether any language engine's measured drift has broken the
// configured budget. Exposed so the readiness/dashboard layer can raise a loud
// warning instead of drift being merely a number on a diagnostics panel (§5.5,
// guardrail #7 no silent degradation).
func (c *Controller) OverBudget() bool {
	for _, e := range c.contentEngines() {
		if e.OverBudget() {
			return true
		}
	}
	return false
}
