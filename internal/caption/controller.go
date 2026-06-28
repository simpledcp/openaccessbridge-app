// Package caption wires the caption path together: it drives one timing engine
// per language from a single server timeline and fans the resulting active-cue
// changes out to patron browsers (buildplan.md §5). It is the integration point
// for both the Phase-1 offline player (fake show clock) and the ST 430-10 state
// machine (real lease/timeline), so the timeline-handling and multi-language
// fan-out live in exactly one place.
package caption

import (
	"sync"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/timing"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Controller maps a single timeline onto per-language timing engines and the hub.
type Controller struct {
	mu      sync.Mutex
	hub     *fanout.Hub
	clock   timing.Clock
	budget  int64
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
		engines: make(map[string]*timing.Engine),
	}
}

// LoadReel replaces the active cue set for the current reel/composition, keyed by
// language. Each language gets its own timing engine driven by the shared
// timeline. Languages absent from the new set have their engines retired.
func (c *Controller) LoadReel(cuesByLang map[string][]model.Cue) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Retire engines for languages no longer present.
	for lang, eng := range c.engines {
		if _, ok := cuesByLang[lang]; !ok {
			eng.Close()
			delete(c.engines, lang)
		}
	}

	c.langs = c.langs[:0]
	for lang, cues := range cuesByLang {
		c.langs = append(c.langs, lang)
		eng, ok := c.engines[lang]
		if !ok {
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
		eng.SetCues(lang, cues)
	}
}

// Start launches the boundary schedulers so cue changes are pushed in real time.
func (c *Controller) Start() {
	c.mu.Lock()
	c.started = true
	for _, eng := range c.engines {
		eng.Start()
	}
	c.mu.Unlock()
}

// Close stops all engines.
func (c *Controller) Close() {
	c.mu.Lock()
	for _, eng := range c.engines {
		eng.Close()
	}
	c.mu.Unlock()
}

// --- Timeline events, forwarded to every language engine (§5.5) ---

func (c *Controller) eachEngine(fn func(*timing.Engine)) {
	c.mu.Lock()
	engs := make([]*timing.Engine, 0, len(c.engines))
	for _, e := range c.engines {
		engs = append(engs, e)
	}
	c.mu.Unlock()
	for _, e := range engs {
		fn(e)
	}
}

// Play starts playback at posMS and surfaces an active patron status.
func (c *Controller) Play(posMS int64) {
	c.eachEngine(func(e *timing.Engine) { e.Play(posMS) })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// UpdateTimeline re-anchors all engines to a fresh server position.
func (c *Controller) UpdateTimeline(posMS int64) {
	c.eachEngine(func(e *timing.Engine) { e.UpdateTimeline(posMS) })
}

// Pause freezes captions.
func (c *Controller) Pause() {
	c.eachEngine(func(e *timing.Engine) { e.Pause() })
	c.hub.PublishStatus(model.PatronCaptionWaiting)
}

// Resume continues captions.
func (c *Controller) Resume() {
	c.eachEngine(func(e *timing.Engine) { e.Resume() })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// Stop halts captions.
func (c *Controller) Stop() {
	c.eachEngine(func(e *timing.Engine) { e.Stop() })
	c.hub.PublishStatus(model.PatronCaptionWaiting)
}

// SeekTo jumps all engines to posMS.
func (c *Controller) SeekTo(posMS int64) {
	c.eachEngine(func(e *timing.Engine) { e.SeekTo(posMS) })
}

// CommsLost signals a momentary interruption; captions keep showing (§5.2 item 2).
func (c *Controller) CommsLost() {
	c.eachEngine(func(e *timing.Engine) { e.CommsLost() })
	c.hub.PublishStatus(model.PatronCaptionReconnecting)
}

// CommsRestored clears the interruption.
func (c *Controller) CommsRestored() {
	c.eachEngine(func(e *timing.Engine) { e.CommsRestored() })
	c.hub.PublishStatus(model.PatronCaptionActive)
}

// LeaseExpired blanks captions (only on genuine lease expiry, §5.2 item 2).
func (c *Controller) LeaseExpired() {
	c.eachEngine(func(e *timing.Engine) { e.LeaseExpired() })
	c.hub.PublishStatus(model.PatronCaptionUnavailable)
}

// SetPatronStatus lets the ACSP layer drive the patron-facing status directly.
func (c *Controller) SetPatronStatus(s model.PatronCaptionStatus) {
	c.hub.PublishStatus(s)
}

// Evaluate forces an immediate active-cue re-resolution on every engine. Useful
// for the ACSP status loop and for deterministic testing with an injected clock.
func (c *Controller) Evaluate() {
	c.eachEngine(func(e *timing.Engine) { e.Evaluate() })
}

// Languages returns the currently loaded languages.
func (c *Controller) Languages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.langs))
	copy(out, c.langs)
	return out
}

// MaxDriftMS returns the largest absolute drift across engines (diagnostics §5.5).
func (c *Controller) MaxDriftMS() int64 {
	var max int64
	c.eachEngine(func(e *timing.Engine) {
		d := e.DriftMS()
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	})
	return max
}
