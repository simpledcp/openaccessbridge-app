package caption

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
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
