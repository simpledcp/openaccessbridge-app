package fanout

import (
	"context"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
)

func mustNext(t *testing.T, s *Subscriber) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return m
}

func TestSubscribeGetsCurrentStateImmediately(t *testing.T) {
	h := NewHub(Options{Auditorium: "Auditorium 3"})
	h.PublishCue("en", true, model.Cue{Text: "Hello", ResourceID: "r1", StartMS: 1000})

	// Stateless reconnect: a new subscriber immediately gets the current cue.
	s, err := h.Subscribe("en")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m := mustNext(t, s)
	if !m.Visible || m.Text != "Hello" || m.Auditorium != "Auditorium 3" {
		t.Fatalf("expected current cue on subscribe, got %+v", m)
	}
}

func TestLanguageIsolation(t *testing.T) {
	h := NewHub(Options{})
	en, _ := h.Subscribe("en")
	es, _ := h.Subscribe("es-419")
	_ = mustNext(t, en) // drain initial
	_ = mustNext(t, es)

	h.PublishCue("en", true, model.Cue{Text: "English"})
	m := mustNext(t, en)
	if m.Text != "English" {
		t.Fatalf("en sub got %+v", m)
	}
	// es sub must not receive the English cue.
	h.PublishCue("es-419", true, model.Cue{Text: "Español"})
	m2 := mustNext(t, es)
	if m2.Text != "Español" {
		t.Fatalf("es sub got %+v", m2)
	}
}

func TestSwitchLanguageWithoutReconnect(t *testing.T) {
	h := NewHub(Options{})
	h.PublishCue("en", true, model.Cue{Text: "English"})
	h.PublishCue("fr", true, model.Cue{Text: "Français"})
	s, _ := h.Subscribe("en")
	if m := mustNext(t, s); m.Text != "English" {
		t.Fatalf("initial en = %+v", m)
	}
	h.SetLanguage(s, "fr")
	if m := mustNext(t, s); m.Text != "Français" || m.Language != "fr" {
		t.Fatalf("after switch = %+v", m)
	}
}

func TestCoalesceToLatest(t *testing.T) {
	h := NewHub(Options{})
	s, _ := h.Subscribe("en")
	_ = mustNext(t, s) // drain initial

	// Publish several cues faster than the consumer reads; it should converge to
	// the latest, never block the publisher.
	h.PublishCue("en", true, model.Cue{Text: "one"})
	h.PublishCue("en", true, model.Cue{Text: "two"})
	h.PublishCue("en", true, model.Cue{Text: "three"})
	m := mustNext(t, s)
	if m.Text != "three" {
		t.Fatalf("expected coalesced latest 'three', got %q", m.Text)
	}
}

func TestStatusBroadcastReachesAllLanguages(t *testing.T) {
	h := NewHub(Options{})
	en, _ := h.Subscribe("en")
	es, _ := h.Subscribe("es")
	_ = mustNext(t, en)
	_ = mustNext(t, es)

	h.PublishStatus(model.PatronCaptionReconnecting)
	if m := mustNext(t, en); m.Status != string(model.PatronCaptionReconnecting) {
		t.Fatalf("en status = %q", m.Status)
	}
	if m := mustNext(t, es); m.Status != string(model.PatronCaptionReconnecting) {
		t.Fatalf("es status = %q", m.Status)
	}
}

func TestMaxClientsCap(t *testing.T) {
	h := NewHub(Options{MaxClients: 2})
	if _, err := h.Subscribe("en"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("en"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("en"); err != ErrTooManyClients {
		t.Fatalf("expected ErrTooManyClients, got %v", err)
	}
}

func TestClientCountTracking(t *testing.T) {
	var counts []int
	h := NewHub(Options{OnCount: func(n int) { counts = append(counts, n) }})
	s, _ := h.Subscribe("en")
	if h.ClientCount() != 1 {
		t.Fatalf("count = %d", h.ClientCount())
	}
	h.Unsubscribe(s)
	if h.ClientCount() != 0 {
		t.Fatalf("count after unsubscribe = %d", h.ClientCount())
	}
	// Unsubscribe should also wake a blocked Next with ErrClosed.
	ctx := context.Background()
	if _, err := s.Next(ctx); err != ErrClosed {
		t.Fatalf("expected ErrClosed after unsubscribe, got %v", err)
	}
}
