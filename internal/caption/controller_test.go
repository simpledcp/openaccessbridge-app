package caption

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/timing"
	"github.com/simpledcp/captionbridge/internal/model"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

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

func drainTo(t *testing.T, s *fanout.Subscriber, wantText string, timeout time.Duration) fanout.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last fanout.Message
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		m, err := s.Next(ctx)
		cancel()
		if err != nil {
			break
		}
		last = m
		if m.Text == wantText {
			return m
		}
	}
	return last
}

// TestControllerMultiLanguageFanout drives two languages from one timeline and
// asserts each subscriber sees its own language's cue at the same position.
func TestControllerMultiLanguageFanout(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 1"})
	c := NewController(Options{Hub: hub, Clock: clk.now})

	c.LoadReel(map[string][]model.Cue{
		"en":     {{StartMS: 1000, EndMS: 3000, Text: "Hello"}},
		"es-419": {{StartMS: 1000, EndMS: 3000, Text: "Hola"}},
	})

	en, _ := hub.Subscribe("en")
	es, _ := hub.Subscribe("es-419")
	_, _ = en.Next(context.Background()) // drain initial state
	_, _ = es.Next(context.Background())

	c.Play(0)
	clk.advance(1500 * time.Millisecond) // pos 1500 → both cues active
	c.Evaluate()

	if m := drainTo(t, en, "Hello", time.Second); m.Text != "Hello" {
		t.Fatalf("en subscriber got %+v", m)
	}
	if m := drainTo(t, es, "Hola", time.Second); m.Text != "Hola" {
		t.Fatalf("es subscriber got %+v", m)
	}
}

// TestControllerCommsLossKeepsCaptions asserts the §5.2-item-2 behavior end to end
// through the controller + hub.
func TestControllerCommsLossKeepsCaptions(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{})
	c := NewController(Options{Hub: hub, Clock: clk.now})
	c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 1000, EndMS: 5000, Text: "Holding"}}})
	sub, _ := hub.Subscribe("en")
	_, _ = sub.Next(context.Background())

	c.Play(0)
	clk.advance(1500 * time.Millisecond)
	c.Evaluate()
	if m := drainTo(t, sub, "Holding", time.Second); m.Text != "Holding" {
		t.Fatalf("expected cue, got %+v", m)
	}

	c.CommsLost() // status → reconnecting, but cue keeps showing
	clk.advance(500 * time.Millisecond)
	c.Evaluate()
	// Caption text must remain visible across comms loss.
	m := drainTo(t, sub, "Holding", time.Second)
	if !m.Visible || m.Text != "Holding" {
		t.Fatalf("captions must persist across comms loss, got %+v", m)
	}

	c.LeaseExpired() // now blank
	mm := waitVisibleFalse(t, sub, time.Second)
	if mm.Visible {
		t.Fatalf("expected blank after lease expiry, got %+v", mm)
	}
}

// TestControllerPublishesTrackGrade is §5.4 line 320 end to end: the ST 429-12
// grade stamped on the cues by the RPL layer must reach the patron so a
// subtitle-only show is not labeled "Closed captions".
func TestControllerPublishesTrackGrade(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{})
	c := NewController(Options{Hub: hub, Clock: clk.now})
	c.LoadReel(map[string][]model.Cue{
		"en": {{StartMS: 1000, EndMS: 3000, Text: "Hello", Kind: model.TrackClosedCaption}},
		"de": {{StartMS: 1000, EndMS: 3000, Text: "Hallo", Kind: model.TrackMainSubtitle}},
	})

	tests := []struct {
		lang      string
		wantKind  string
		wantGrade bool
	}{
		{lang: "en", wantKind: "closed_caption", wantGrade: true},
		{lang: "de", wantKind: "main_subtitle", wantGrade: false},
	}
	for _, tt := range tests {
		sub, _ := hub.Subscribe(tt.lang)
		m, err := sub.Next(context.Background())
		if err != nil {
			t.Fatalf("%s: Next: %v", tt.lang, err)
		}
		if m.Kind != tt.wantKind || m.CaptionGrade != tt.wantGrade || !m.LanguageAvailable {
			t.Errorf("%s: kind=%q grade=%v available=%v, want %q/%v/true",
				tt.lang, m.Kind, m.CaptionGrade, m.LanguageAvailable, tt.wantKind, tt.wantGrade)
		}
	}
}

// TestControllerRetiresDroppedLanguage: a reel that carries no track for a
// language must blank that language's subscribers, not leave the previous reel's
// cue frozen on their phones (§5.2 item 5).
func TestControllerRetiresDroppedLanguage(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{})
	c := NewController(Options{Hub: hub, Clock: clk.now})
	c.LoadReel(map[string][]model.Cue{
		"en": {{StartMS: 0, EndMS: 5000, Text: "Hello", Kind: model.TrackClosedCaption}},
		"fr": {{StartMS: 0, EndMS: 5000, Text: "Bonjour", Kind: model.TrackClosedCaption}},
	})
	sub, _ := hub.Subscribe("fr")
	_, _ = sub.Next(context.Background())
	c.Play(1000)
	if m := drainTo(t, sub, "Bonjour", time.Second); m.Text != "Bonjour" {
		t.Fatalf("expected the fr cue, got %+v", m)
	}

	// Next reel: English only.
	c.LoadReel(map[string][]model.Cue{
		"en": {{StartMS: 0, EndMS: 5000, Text: "Hello", Kind: model.TrackClosedCaption}},
	})
	m := waitVisibleFalse(t, sub, time.Second)
	if m.Visible || m.LanguageAvailable {
		t.Fatalf("dropped language must be blanked and reported, got %+v", m)
	}
}

// TestControllerCompositionChangedDiscardsPrediction is the §5.5 SPL/CPL-change
// event: a new composition arriving mid-lease must not have its cues resolved
// against the outgoing composition's timeline.
func TestControllerCompositionChangedDiscardsPrediction(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{})
	c := NewController(Options{Hub: hub, Clock: clk.now})
	c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 1000, EndMS: 9000, Text: "OldShow"}}})
	sub, _ := hub.Subscribe("en")
	_, _ = sub.Next(context.Background())
	c.Play(0)
	clk.advance(2 * time.Second)
	c.Evaluate()
	if m := drainTo(t, sub, "OldShow", time.Second); m.Text != "OldShow" {
		t.Fatalf("expected the old composition's cue, got %+v", m)
	}

	c.CompositionChanged()
	if m := waitVisibleFalse(t, sub, time.Second); m.Visible {
		t.Fatalf("composition change must blank, got %+v", m)
	}

	// The new composition loads. Its cue covers the STALE predicted position
	// (2000) but not position 0, so nothing may show until a fresh timeline.
	c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 1500, EndMS: 4000, Text: "NewShow"}}})
	clk.advance(2 * time.Second)
	c.Evaluate()
	if m := sub.Peek(); m.Visible {
		t.Fatalf("new cues must not resolve against the old anchor, got %+v", m)
	}

	c.UpdateTimeline(2000)
	if m := drainTo(t, sub, "NewShow", time.Second); m.Text != "NewShow" {
		t.Fatalf("expected NewShow after the re-anchor, got %+v", m)
	}
}

// TestControllerFailedReelLoadAfterCompositionChange: LoadReel can legitimately
// never arrive after a composition change (the RPL fetch failed). The patron must
// then see nothing rather than the previous show's captions (§5.5, guardrail #7).
func TestControllerFailedReelLoadAfterCompositionChange(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5000, 0)}
	hub := fanout.NewHub(fanout.Options{})
	c := NewController(Options{Hub: hub, Clock: clk.now})
	c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 0, EndMS: 9000, Text: "OldShow", Kind: model.TrackClosedCaption}}})
	sub, _ := hub.Subscribe("en")
	_, _ = sub.Next(context.Background())
	c.Play(1000)
	if m := drainTo(t, sub, "OldShow", time.Second); m.Text != "OldShow" {
		t.Fatalf("setup: expected the old composition's cue, got %+v", m)
	}

	c.CompositionChanged()
	if m := waitVisibleFalse(t, sub, time.Second); m.Visible {
		t.Fatalf("composition change must blank, got %+v", m)
	}

	// No LoadReel: the new composition's RPL never resolved. Timeline keeps flowing.
	c.UpdateTimeline(2000)
	clk.advance(2 * time.Second)
	c.Evaluate()
	if m := sub.Peek(); m.Visible {
		t.Fatalf("the previous composition's caption must stay off, got %+v", m)
	}
}

// TestControllerDriftBudget pins D9 on the controller: the budget is
// configurable, defaults to the D9 numbers, and over-budget drift is reportable
// rather than only measurable.
func TestControllerDriftBudget(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		wantWarn int64
	}{
		{name: "d9_default", opts: Options{}, wantWarn: timing.DefaultWarnBudgetMS},
		{name: "configured", opts: Options{WarnBudgetMS: 250}, wantWarn: 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(5000, 0)}
			tt.opts.Hub = fanout.NewHub(fanout.Options{})
			tt.opts.Clock = clk.now
			c := NewController(tt.opts)
			if got := c.WarnBudgetMS(); got != tt.wantWarn {
				t.Fatalf("WarnBudgetMS = %d, want %d", got, tt.wantWarn)
			}
			c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 0, EndMS: 60000, Text: "x"}}})
			c.Play(0)
			if c.OverBudget() {
				t.Fatalf("no drift measured yet, must not be over budget")
			}
			clk.advance(10 * time.Second)
			c.UpdateTimeline(10000 - tt.wantWarn - 1)
			if !c.OverBudget() {
				t.Fatalf("drift %d must exceed budget %d", c.MaxDriftMS(), tt.wantWarn)
			}
		})
	}
}

func waitVisibleFalse(t *testing.T, s *fanout.Subscriber, timeout time.Duration) fanout.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last fanout.Message
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		m, err := s.Next(ctx)
		cancel()
		if err != nil {
			break
		}
		last = m
		if !m.Visible {
			return m
		}
	}
	return last
}
