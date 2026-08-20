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

// TestTrackGradeReachesPatron is §5.4 line 320: a subtitle-only show must not be
// presented as "Closed captions". The grade is resolved upstream from the ST
// 429-12 track type; the fan-out has to carry it to the phone.
func TestTrackGradeReachesPatron(t *testing.T) {
	tests := []struct {
		name      string
		kind      model.TrackKind
		wantKind  string
		wantGrade bool
	}{
		{name: "closed_caption", kind: model.TrackClosedCaption, wantKind: "closed_caption", wantGrade: true},
		{name: "closed_subtitle", kind: model.TrackClosedSubtitle, wantKind: "closed_subtitle", wantGrade: false},
		{name: "main_subtitle", kind: model.TrackMainSubtitle, wantKind: "main_subtitle", wantGrade: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(Options{})
			h.PublishCue("en", true, model.Cue{Text: "Hello", Kind: tt.kind})
			s, _ := h.Subscribe("en")
			m := mustNext(t, s)
			if m.Kind != tt.wantKind || m.CaptionGrade != tt.wantGrade {
				t.Fatalf("kind=%q caption_grade=%v, want %q/%v", m.Kind, m.CaptionGrade, tt.wantKind, tt.wantGrade)
			}
			// The grade must survive a cue-out: the patron copy stays correct
			// between cues, not just while text is on screen.
			h.PublishCue("en", false, model.Cue{})
			if m := mustNext(t, s); m.Kind != tt.wantKind || m.CaptionGrade != tt.wantGrade {
				t.Fatalf("grade lost on cue-out: %+v", m)
			}
		})
	}
}

// TestSetTrackKindLabelsBeforeFirstCue: the label must be right from the moment
// the reel loads, not only after the first cue fires.
func TestSetTrackKindLabelsBeforeFirstCue(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("es-419", model.TrackClosedSubtitle)
	s, _ := h.Subscribe("es-419")
	m := mustNext(t, s)
	if m.CaptionGrade || m.Kind != "closed_subtitle" || !m.LanguageAvailable {
		t.Fatalf("expected subtitle-grade available language, got %+v", m)
	}
}

// TestUnknownLanguageIsReportedNotBlank is the no-silent-degradation guardrail on
// the patron path: a phone asking for a language the show does not carry must be
// told so, not served a permanently empty caption area.
func TestUnknownLanguageIsReportedNotBlank(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("en-US", model.TrackClosedCaption)

	s, _ := h.Subscribe("en-GB") // no exact track for this tag
	if m := mustNext(t, s); m.LanguageAvailable {
		t.Fatalf("en-GB is not carried; expected language_available=false, got %+v", m)
	}
	if h.HasLanguage("en-GB") {
		t.Fatalf("HasLanguage(en-GB) should be false")
	}
	// Switching to a carried language clears the condition.
	h.SetLanguage(s, "en-US")
	if m := mustNext(t, s); !m.LanguageAvailable {
		t.Fatalf("en-US is carried; expected language_available=true, got %+v", m)
	}
}

// TestClearLanguageRetiresStaleCue: when a reel carries no track for a language
// its subscribers must be blanked and told, not left holding the previous reel's
// last cue forever (§5.2 item 5 "no captions for this segment").
func TestClearLanguageRetiresStaleCue(t *testing.T) {
	h := NewHub(Options{})
	h.PublishCue("fr", true, model.Cue{Text: "Bonjour", Kind: model.TrackClosedCaption})
	s, _ := h.Subscribe("fr")
	if m := mustNext(t, s); !m.Visible {
		t.Fatalf("expected the cue on subscribe, got %+v", m)
	}
	h.ClearLanguage("fr")
	m := mustNext(t, s)
	if m.Visible || m.Text != "" || m.LanguageAvailable {
		t.Fatalf("expected a blank unavailable state, got %+v", m)
	}
}

// TestCueOutDoesNotReviveRetiredLanguage: a retired engine's boundary timer can
// still be in flight when Controller.LoadReel drops its language, and it lands as
// a blank PublishCue. A cue-out proves nothing about what the show carries, so it
// must not re-advertise the language as available (guardrail #7).
func TestCueOutDoesNotReviveRetiredLanguage(t *testing.T) {
	h := NewHub(Options{})
	h.PublishCue("fr", true, model.Cue{Text: "Bonjour", Kind: model.TrackClosedCaption})
	s, _ := h.Subscribe("fr")
	_ = mustNext(t, s)

	h.ClearLanguage("fr")
	if m := mustNext(t, s); m.LanguageAvailable {
		t.Fatalf("setup: expected fr retired, got %+v", m)
	}

	// The straggler push from the closed engine.
	h.PublishCue("fr", false, model.Cue{})
	if h.HasLanguage("fr") {
		t.Fatalf("a cue-out must not revive a retired language")
	}
	if m := mustNext(t, s); m.LanguageAvailable || m.Visible {
		t.Fatalf("expected fr still retired and blank, got %+v", m)
	}
	for _, o := range h.Languages() {
		if o.Code == "fr" {
			t.Fatalf("retired language must not be offered: %+v", h.Languages())
		}
	}
}

// TestLanguagesOrderedByAccessibilityRank pins the ordering NegotiateLanguage
// relies on for D8 steps 3 and 4: closed captions first, then subtitles, with
// codes breaking ties so the patron list is stable across reloads.
func TestLanguagesOrderedByAccessibilityRank(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("zh", model.TrackMainSubtitle)
	h.SetTrackKind("es-419", model.TrackClosedSubtitle)
	h.SetTrackKind("fr", model.TrackClosedCaption)
	h.SetTrackKind("en-US", model.TrackClosedCaption)

	got := h.Languages()
	want := []string{"en-US", "fr", "es-419", "zh"}
	if len(got) != len(want) {
		t.Fatalf("languages = %+v, want %v", got, want)
	}
	for i := range want {
		if got[i].Code != want[i] {
			t.Fatalf("languages = %+v, want %v", got, want)
		}
	}
}

// TestNegotiateLanguage is the D8 / §5.4 default-language priority: (1) venue
// configured, (2) browser language (exact tag, then primary subtag), (3) first
// closed-caption track, (4) first subtitle track.
func TestNegotiateLanguage(t *testing.T) {
	available := []LanguageOption{
		{Code: "en-US", Kind: model.TrackClosedCaption},
		{Code: "fr", Kind: model.TrackClosedCaption},
		{Code: "es-419", Kind: model.TrackClosedSubtitle},
	}
	subtitleOnly := []LanguageOption{
		{Code: "de", Kind: model.TrackMainSubtitle},
		{Code: "ja", Kind: model.TrackMainSubtitle},
	}
	tests := []struct {
		name       string
		available  []LanguageOption
		configured []string
		requested  []string
		want       string
		wantOK     bool
	}{
		{name: "venue_default_wins", available: available,
			configured: []string{"fr"}, requested: []string{"en-US"}, want: "fr", wantOK: true},
		{name: "venue_default_absent_falls_through", available: available,
			configured: []string{"pt-BR"}, requested: []string{"es-419"}, want: "es-419", wantOK: true},
		{name: "browser_exact_tag", available: available,
			requested: []string{"es-419"}, want: "es-419", wantOK: true},
		{name: "browser_primary_subtag", available: available,
			requested: []string{"en-GB"}, want: "en-US", wantOK: true},
		{name: "browser_order_respected", available: available,
			requested: []string{"pt", "fr", "en-US"}, want: "fr", wantOK: true},
		{name: "wildcard_ignored", available: available,
			requested: []string{"*"}, want: "en-US", wantOK: true},
		{name: "no_match_first_closed_caption", available: available,
			requested: []string{"ja"}, want: "en-US", wantOK: true},
		{name: "no_caption_track_first_subtitle", available: subtitleOnly,
			requested: []string{"ko"}, want: "de", wantOK: true},
		{name: "nothing_available", available: nil,
			configured: []string{"en"}, requested: []string{"en"}, want: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NegotiateLanguage(tt.available, tt.configured, tt.requested)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("NegotiateLanguage = (%q,%v), want (%q,%v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestMaxClientsCap(t *testing.T) {
	h := NewHub(Options{MaxClients: 2})
	// MaxClients is readable so the wiring layer can be asserted against the
	// active deployment profile's cap (D11) rather than the cap being dead
	// config nothing checks.
	if got := h.MaxClients(); got != 2 {
		t.Fatalf("MaxClients = %d, want 2", got)
	}
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
