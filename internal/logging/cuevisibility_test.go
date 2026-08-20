package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock returns a clock function over a mutable instant.
func fakeClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

// decodeLines parses every JSON log line the buffer holds.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, ln)
		}
		out = append(out, m)
	}
	return out
}

// TestCueVisibility_EmitsDurationNeverText is the §9.4 capability the taxonomy
// exists for: "records that a cue was shown and for how long, not its words".
func TestCueVisibility_EmitsDurationNeverText(t *testing.T) {
	now := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	log := New(Options{Component: "core", Writer: &buf, JSON: true})
	tr := NewCueVisibilityTracker(log, CueVisibilityOptions{Now: fakeClock(&now)})

	tr.Observe("en", "urn:uuid:cue-42", true)
	now = now.Add(2500 * time.Millisecond)
	tr.Observe("en", "urn:uuid:cue-42", false)

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want exactly one event, got %d: %s", len(lines), buf.String())
	}
	m := lines[0]
	if m["event"] != string(EvCueVisibleNoText) {
		t.Errorf("event = %v, want %v", m["event"], EvCueVisibleNoText)
	}
	if m["duration_ms"] != float64(2500) {
		t.Errorf("duration_ms = %v, want 2500", m["duration_ms"])
	}
	if m["language"] != "en" {
		t.Errorf("language = %v, want en", m["language"])
	}
	// The cue id is hashed: correlatable, not reversible, and never the raw id.
	id, _ := m["cue_id"].(string)
	if id == "" || strings.Contains(id, "cue-42") || strings.Contains(id, "urn") {
		t.Errorf("cue_id must be an opaque hash, got %q", id)
	}
	if id != HashID("cue", "urn:uuid:cue-42") {
		t.Errorf("cue_id = %q, want the salted hash", id)
	}
	// Nothing resembling cue content may appear anywhere in the line.
	for k := range m {
		if strings.Contains(strings.ToLower(k), "text") {
			t.Errorf("a *_text field reached the event: %q", k)
		}
	}
}

// TestCueVisibility_Transitions covers the bookkeeping: only a matched
// show→hide pair emits, repeats do not double-count, and separate languages and
// cues are tracked independently.
func TestCueVisibility_Transitions(t *testing.T) {
	type step struct {
		lang, id string
		visible  bool
		advance  time.Duration
	}
	cases := []struct {
		name          string
		steps         []step
		wantEvents    int
		wantDurations []float64
		wantVisible   int
	}{
		{
			name:  "hide without a show emits nothing",
			steps: []step{{lang: "en", id: "a", visible: false}},
		},
		{
			name: "repeated show does not restart the clock",
			steps: []step{
				{lang: "en", id: "a", visible: true, advance: time.Second},
				{lang: "en", id: "a", visible: true, advance: time.Second},
				{lang: "en", id: "a", visible: false},
			},
			wantEvents: 1, wantDurations: []float64{2000},
		},
		{
			name: "repeated hide emits once",
			steps: []step{
				{lang: "en", id: "a", visible: true, advance: 500 * time.Millisecond},
				{lang: "en", id: "a", visible: false},
				{lang: "en", id: "a", visible: false},
			},
			wantEvents: 1, wantDurations: []float64{500},
		},
		{
			name: "the same id in two languages is two cues",
			steps: []step{
				{lang: "en", id: "a", visible: true},
				{lang: "es", id: "a", visible: true, advance: time.Second},
				{lang: "en", id: "a", visible: false, advance: time.Second},
				{lang: "es", id: "a", visible: false},
			},
			wantEvents: 2, wantDurations: []float64{1000, 2000},
		},
		{
			name: "a cue still on screen has not been reported yet",
			steps: []step{
				{lang: "en", id: "a", visible: true},
				{lang: "en", id: "b", visible: true, advance: time.Second},
			},
			wantVisible: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			var buf bytes.Buffer
			tr := NewCueVisibilityTracker(
				New(Options{Component: "core", Writer: &buf, JSON: true}),
				CueVisibilityOptions{Now: fakeClock(&now)})

			for _, s := range tc.steps {
				tr.Observe(s.lang, s.id, s.visible)
				now = now.Add(s.advance)
			}
			lines := decodeLines(t, &buf)
			if len(lines) != tc.wantEvents {
				t.Fatalf("events = %d, want %d: %s", len(lines), tc.wantEvents, buf.String())
			}
			for i, want := range tc.wantDurations {
				if lines[i]["duration_ms"] != want {
					t.Errorf("event %d duration_ms = %v, want %v", i, lines[i]["duration_ms"], want)
				}
			}
			if got := tr.VisibleCount(); got != tc.wantVisible {
				t.Errorf("VisibleCount() = %d, want %d", got, tc.wantVisible)
			}
		})
	}
}

// TestCueVisibility_BlankReportsEveryVisibleCue: show end / composition change /
// comms loss really does stop the display, so each on-screen cue's duration is
// reported rather than lost.
func TestCueVisibility_BlankReportsEveryVisibleCue(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	tr := NewCueVisibilityTracker(
		New(Options{Component: "core", Writer: &buf, JSON: true}),
		CueVisibilityOptions{Now: fakeClock(&now)})

	tr.Observe("en", "a", true)
	tr.Observe("es", "b", true)
	now = now.Add(3 * time.Second)
	tr.Blank()

	if got := len(decodeLines(t, &buf)); got != 2 {
		t.Fatalf("Blank should report both visible cues, got %d events", got)
	}
	if tr.VisibleCount() != 0 {
		t.Error("Blank must clear the visible set")
	}
	// A second Blank has nothing left to report.
	buf.Reset()
	tr.Blank()
	if buf.Len() != 0 {
		t.Errorf("a second Blank emitted %q", buf.String())
	}
}

// TestCueVisibility_ClockStepBackClampsToZero: a clock that steps backwards must
// not produce a negative display time.
func TestCueVisibility_ClockStepBackClampsToZero(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var buf bytes.Buffer
	tr := NewCueVisibilityTracker(
		New(Options{Component: "core", Writer: &buf, JSON: true}),
		CueVisibilityOptions{Now: fakeClock(&now)})

	tr.Observe("en", "a", true)
	now = now.Add(-5 * time.Second)
	tr.Observe("en", "a", false)

	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["duration_ms"] != float64(0) {
		t.Fatalf("want one event with duration_ms 0, got %s", buf.String())
	}
}

// TestCueVisibility_Concurrent exercises the fan-out's real usage: several
// languages driving the tracker at once. Run with -race.
func TestCueVisibility_Concurrent(t *testing.T) {
	tr := NewCueVisibilityTracker(Nop(), CueVisibilityOptions{})
	var wg sync.WaitGroup
	for _, lang := range []string{"en", "es", "fr", "de"} {
		wg.Add(1)
		go func(lang string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Observe(lang, "cue", true)
				tr.Observe(lang, "cue", false)
			}
		}(lang)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tr.Blank()
			_ = tr.VisibleCount()
		}
	}()
	wg.Wait()
}

// TestCueVisibility_NilSafe: a nil tracker and a nil logger are both no-ops, so a
// caller can leave observability off without branching at every call site.
func TestCueVisibility_NilSafe(t *testing.T) {
	var tr *CueVisibilityTracker
	tr.Observe("en", "a", true)
	tr.Blank()
	if tr.VisibleCount() != 0 {
		t.Error("a nil tracker should report no visible cues")
	}
	ok := NewCueVisibilityTracker(nil, CueVisibilityOptions{})
	ok.Observe("en", "a", true)
	ok.Observe("en", "a", false)
}
