package resolver

import (
	"context"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/rpl"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// ttDocStartTime builds a doc whose <StartTime> origin falls after every cue, so
// the parser drops them all and records both the per-cue class and the
// whole-asset class (§5.4).
func ttDocStartTime(lang, text string, inMS, outMS, startMS int) string {
	tc := func(ms int) string { return formatTC(0, 0, ms/1000, ms%1000) }
	return `<?xml version="1.0"?><SubtitleReel xmlns="x"><Language>` + lang +
		`</Language><EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate>` +
		`<StartTime>` + tc(startMS) + `</StartTime><SubtitleList><Font>` +
		`<Subtitle TimeIn="` + tc(inMS) + `" TimeOut="` + tc(outMS) + `"><Text>` + text + `</Text></Subtitle>` +
		`</Font></SubtitleList></SubtitleReel>`
}

// TestParseWarningsReachTheCaller: a timed-text asset that parses with
// degradation must not lose that fact between the parser and the caller. Every
// StartTime/edit-rate warning class the parser raises depends on this channel;
// without it, parser-level degradation is silent in production (guardrail #7).
func TestParseWarningsReachTheCaller(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{
		// Every cue ends before the origin: the reel loses all its captions.
		"degraded.xml": []byte(ttDocStartTime("en", "Line", 1000, 2000, 9000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	var got []ParseWarning
	loader.OnParseWarning = func(w ParseWarning) { got = append(got, w) }

	cues, fetchWarnings, err := loader.Load(context.Background(), WindowRequest{
		RPL: oneReelRPL("res-1", "degraded.xml"), Languages: []string{"en"}, Scope: "playout-1",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cues["en"]) != 0 {
		t.Fatalf("the fixture is meant to lose every cue, got %d", len(cues["en"]))
	}
	if len(got) != 1 {
		t.Fatalf("expected one parse warning for the one degraded asset, got %d", len(got))
	}
	w := got[0]
	if w.Language != "en" || w.ReelNumber != 1 {
		t.Errorf("the warning must be attributed to its reel/language, got %+v", w)
	}
	if !hasClass(w.Classes, "all_cues_before_start_time") {
		t.Errorf("expected the whole-asset class, got %v", w.Classes)
	}
	if w.Count < 2 {
		t.Errorf("expected the raw warning count to survive, got %d", w.Count)
	}
	// A degraded asset that DID serve must not be reported as one that would not
	// serve at all: that list drives the "server did not serve every caption
	// asset" warning, and mislabeling would send a booth chasing the wrong fault.
	if len(fetchWarnings) != 0 {
		t.Errorf("parse degradation is not a fetch failure, got %v", fetchWarnings)
	}
}

// TestParseWarningClassesAreDeduped: a hundred cues clamped to the same origin is
// ONE condition with a count, not a hundred conditions.
func TestParseWarningClassesAreDeduped(t *testing.T) {
	doc := `<?xml version="1.0"?><SubtitleReel xmlns="x"><Language>en</Language>` +
		`<EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate><StartTime>00:00:09:000</StartTime>` +
		`<SubtitleList><Font>` +
		`<Subtitle TimeIn="00:00:01:000" TimeOut="00:00:02:000"><Text>a</Text></Subtitle>` +
		`<Subtitle TimeIn="00:00:03:000" TimeOut="00:00:04:000"><Text>b</Text></Subtitle>` +
		`<Subtitle TimeIn="00:00:05:000" TimeOut="00:00:06:000"><Text>c</Text></Subtitle>` +
		`</Font></SubtitleList></SubtitleReel>`
	mb := &MemoryFetcher{Files: map[string][]byte{"d.xml": []byte(doc)}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	var got ParseWarning
	loader.OnParseWarning = func(w ParseWarning) { got = w }

	if _, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: oneReelRPL("res-1", "d.xml"), Languages: []string{"en"}, Scope: "s",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 1; i < len(got.Classes); i++ {
		if got.Classes[i-1] >= got.Classes[i] {
			t.Fatalf("classes must be distinct and sorted, got %v", got.Classes)
		}
	}
	if got.Count != 4 { // three cue_before_start_time + all_cues_before_start_time
		t.Errorf("the raw count carries the detail, got %d (classes %v)", got.Count, got.Classes)
	}
}

// TestCleanAssetRaisesNoParseWarning: the channel must stay quiet for a healthy
// asset, or the dashboard learns to ignore it.
func TestCleanAssetRaisesNoParseWarning(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{"ok.xml": []byte(ttDoc("en", "Line", 1000, 2000))}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	raised := 0
	loader.OnParseWarning = func(ParseWarning) { raised++ }
	if _, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: oneReelRPL("res-1", "ok.xml"), Languages: []string{"en"}, Scope: "s",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if raised != 0 {
		t.Errorf("a clean asset must raise nothing, got %d", raised)
	}
}

// TestLoaderSubtractsPerResourceEntryPoint: two languages in ONE reel can carry
// different EntryPoints — the offset into each asset at which the reel starts.
// Applying the reel offset alone (or one resource's entry point to both) puts
// one language's cues at the wrong composition time (§5.3/§5.4 anchoring).
func TestLoaderSubtractsPerResourceEntryPoint(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{
		"en.xml": []byte(ttDoc("en", "English", 5000, 6000)),
		"es.xml": []byte(ttDoc("es", "Espanol", 3000, 4000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	r := &rpl.RPL{Reels: []rpl.Reel{{
		ReelNumber: 1,
		// Reel-level entry point is 0; each resource declares its own trim.
		EditRate: rateUnits(1000), DurationUnits: 60000,
		Resources: []rpl.Resource{
			{ID: "en", Kind: model.TrackClosedCaption, Language: "en", Locator: "en.xml",
				EditRate: rateUnits(1000), EntryPointUnits: 4000, IntrinsicDurationUnits: 60000},
			{ID: "es", Kind: model.TrackClosedCaption, Language: "es", Locator: "es.xml",
				EditRate: rateUnits(1000), EntryPointUnits: 1000, IntrinsicDurationUnits: 60000},
		},
	}}}
	cues, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: r, Languages: []string{"en", "es"}, Scope: "s",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cues["en"]; len(got) != 1 || got[0].StartMS != 1000 {
		t.Errorf("en cue should sit at 5000-4000=1000, got %+v", got)
	}
	if got := cues["es"]; len(got) != 1 || got[0].StartMS != 2000 {
		t.Errorf("es cue should sit at 3000-1000=2000, got %+v", got)
	}
}

func hasClass(classes []string, want string) bool {
	for _, c := range classes {
		if strings.EqualFold(c, want) {
			return true
		}
	}
	return false
}

// TestLoadOutcomeReportsWhatTheLoadFound: a warning about the content being
// served can only be retired on evidence, and the evidence is a later load that
// found nothing wrong. The outcome is reported for EVERY load, including the
// clean ones — a hook that only fires on failure can raise a condition but never
// clear it (guardrail #7).
func TestLoadOutcomeReportsWhatTheLoadFound(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{
		"bad.xml": []byte(ttDocStartTime("en", "Line", 1000, 2000, 9000)),
		"ok.xml":  []byte(ttDoc("en", "Line", 1000, 2000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	var got []LoadOutcome
	var order []string
	loader.OnParseWarning = func(ParseWarning) { order = append(order, "warn") }
	loader.OnLoadComplete = func(o LoadOutcome) {
		order = append(order, "complete")
		got = append(got, o)
	}

	for _, tc := range []struct {
		name     string
		locator  string
		scope    string
		degraded int
		missing  int
	}{
		{"degraded", "bad.xml", "playout-1", 1, 0},
		{"clean", "ok.xml", "playout-2", 0, 0},
		{"missing", "gone.xml", "playout-3", 0, 1},
	} {
		got, order = nil, nil
		if _, _, err := loader.Load(context.Background(), WindowRequest{
			RPL: oneReelRPL("res-1", tc.locator), Languages: []string{"en"}, Scope: tc.scope,
		}); err != nil {
			t.Fatalf("%s: Load: %v", tc.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: expected exactly one outcome per load, got %d", tc.name, len(got))
		}
		o := got[0]
		if o.Scope != tc.scope {
			t.Errorf("%s: scope = %q, want %q", tc.name, o.Scope, tc.scope)
		}
		if o.DegradedAssets != tc.degraded || o.MissingAssets != tc.missing {
			t.Errorf("%s: outcome = %+v, want degraded=%d missing=%d", tc.name, o, tc.degraded, tc.missing)
		}
		// The outcome must land AFTER this load's own per-asset warnings, or a
		// caller clearing on a clean count would erase the raise it just made.
		if len(order) == 0 || order[len(order)-1] != "complete" {
			t.Errorf("%s: the outcome must be reported last, got %v", tc.name, order)
		}
	}
}
