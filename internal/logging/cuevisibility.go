package logging

import (
	"sync"
	"time"
)

// CueVisibilityTracker turns cue visibility transitions into the one event
// §9.4 calls out by name and by rationale:
//
//	caption.cue.visible_no_text records that a cue was shown and for how long,
//	not its words — enough to debug a "captions look wrong" report without
//	storing content.
//
// It is the sanctioned way to answer "did a cue display, and for how long?" from
// the logs. It NEVER receives or emits cue text: the caller passes an opaque cue
// identifier, which the tracker hashes before it is emitted, so even the id is
// not recoverable from a log. This is the diagnostic capability §9.4 trades FOR
// the no-content rule; without it operators are pushed to work around that rule.
//
// It is safe for concurrent use: the caption fan-out drives several languages.
//
// SEAM: nothing constructs a tracker yet, so caption.cue.visible_no_text is still
// emitted by no running process. The call site is Controller.LoadReel's OnChange
// closure in internal/caption (a different lane), which already receives the
// timing.CueEvent{Visible, Cue} this needs. Until that call is added the event
// remains declared-but-unemitted — the mechanism exists, the wiring does not.
type CueVisibilityTracker struct {
	mu    sync.Mutex
	log   *Logger
	salt  string
	now   func() time.Time
	shown map[visKey]time.Time
}

// visKey identifies one visible cue: an id is only unique within its language.
type visKey struct {
	language string
	cueID    string
}

// CueVisibilityOptions configures a tracker. The zero value is usable.
type CueVisibilityOptions struct {
	// Salt scopes the cue-id hash so the same id is not linkable across unrelated
	// domains (§9.4 "opaque/hashed IDs"). Defaults to "cue".
	Salt string
	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// NewCueVisibilityTracker builds a tracker that emits through log. A nil logger
// is replaced with Nop() so a caller can leave observability off without
// branching at every call site.
func NewCueVisibilityTracker(log *Logger, opts CueVisibilityOptions) *CueVisibilityTracker {
	if log == nil {
		log = Nop()
	}
	salt := opts.Salt
	if salt == "" {
		salt = "cue"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &CueVisibilityTracker{log: log, salt: salt, now: now, shown: map[visKey]time.Time{}}
}

// Observe records a cue visibility transition for a language.
//
// visible=true starts the clock for that cue; visible=false emits
// caption.cue.visible_no_text carrying only the hashed cue id, the language, and
// how long the cue was on screen. Repeated transitions in the same direction are
// ignored, so a re-render or a duplicate tick does not double-count, and a
// hide with no matching show emits nothing (there is no duration to report).
func (t *CueVisibilityTracker) Observe(language, cueID string, visible bool) {
	if t == nil {
		return
	}
	k := visKey{language: language, cueID: cueID}

	t.mu.Lock()
	if visible {
		if _, already := t.shown[k]; !already {
			t.shown[k] = t.now()
		}
		t.mu.Unlock()
		return
	}
	start, was := t.shown[k]
	if was {
		delete(t.shown, k)
	}
	now := t.now()
	t.mu.Unlock()

	if !was {
		return
	}
	t.emit(k, now.Sub(start))
}

// Blank ends every currently visible cue, emitting each one's duration. It is the
// show-end / composition-change / comms-loss path: the cue really did stop being
// displayed then, and that duration is exactly what the event exists to record.
func (t *CueVisibilityTracker) Blank() {
	if t == nil {
		return
	}
	t.mu.Lock()
	now := t.now()
	pending := make(map[visKey]time.Duration, len(t.shown))
	for k, start := range t.shown {
		pending[k] = now.Sub(start)
		delete(t.shown, k)
	}
	t.mu.Unlock()

	for k, d := range pending {
		t.emit(k, d)
	}
}

// VisibleCount reports how many cues are currently marked visible. It exists for
// tests and diagnostics; it is a count, never content.
func (t *CueVisibilityTracker) VisibleCount() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.shown)
}

// emit writes the content-free event. Durations are clamped at zero so a clock
// step backwards cannot produce a negative display time.
func (t *CueVisibilityTracker) emit(k visKey, d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	t.log.Info(EvCueVisibleNoText,
		Field("language", k.language),
		Field("cue_id", HashID(t.salt, k.cueID)),
		Field("duration_ms", ms),
	)
}
