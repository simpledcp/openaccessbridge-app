package timedtext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func parseFixture(t *testing.T, name string) *Document {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	doc, err := ParseBytes(b)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", name, err)
	}
	return doc
}

func TestParseSMPTE428_7(t *testing.T) {
	doc := parseFixture(t, "smpte_428-7_24fps.xml")
	if doc.Format != FormatST428 {
		t.Fatalf("format = %v, want %v", doc.Format, FormatST428)
	}
	if doc.Language != "en-US" {
		t.Fatalf("language = %q, want en-US", doc.Language)
	}
	if doc.TimeCodeRate != 24 {
		t.Fatalf("rate = %v, want 24", doc.TimeCodeRate)
	}
	want := []model.Cue{
		{StartMS: 1000, EndMS: 4500, Text: "Hello world"},
		{StartMS: 5000, EndMS: 7000, Text: "[MUSIC PLAYING]\nSecond line here"},
		{StartMS: 8000, EndMS: 10000, Text: "Line with italic run inside"},
	}
	assertCues(t, doc.Cues, want)
}

func TestParseInterop(t *testing.T) {
	doc := parseFixture(t, "interop_cinecanvas.xml")
	if doc.Format != FormatInterop {
		t.Fatalf("format = %v, want %v", doc.Format, FormatInterop)
	}
	if doc.TimeCodeRate != InteropTicksPerSecond {
		t.Fatalf("rate = %v, want %v", doc.TimeCodeRate, InteropTicksPerSecond)
	}
	want := []model.Cue{
		{StartMS: 1000, EndMS: 4500, Text: "Hello interop"},
		{StartMS: 6200, EndMS: 9800, Text: "- First speaker\n- Second speaker"},
	}
	assertCues(t, doc.Cues, want)
}

func TestParseSMPTE428_10_ClosedDerivedRate(t *testing.T) {
	doc := parseFixture(t, "smpte_428-10_closed_25fps.xml")
	if doc.Format != FormatST428 {
		t.Fatalf("format = %v", doc.Format)
	}
	// No TimeCodeRate element → derived from EditRate 25 1.
	if doc.TimeCodeRate != 25 {
		t.Fatalf("derived rate = %v, want 25", doc.TimeCodeRate)
	}
	if doc.Language != "es-419" {
		t.Fatalf("language = %q, want es-419", doc.Language)
	}
	want := []model.Cue{
		{StartMS: 2000, EndMS: 5000, Text: "Primera línea\nSegunda línea"},
	}
	assertCues(t, doc.Cues, want)
}

// TestParseStartTimeNormalizesToTimeline is the §5.4 "correct start/end timing
// relative to the reel/timeline" requirement: an asset authored on the usual
// 01:00:00:00 timecode axis must land at the same timeline-relative ms as the
// identical asset authored from zero, not an hour late.
func TestParseStartTimeNormalizesToTimeline(t *testing.T) {
	doc := parseFixture(t, "smpte_428-7_starttime_24fps.xml")
	if doc.StartTimeMS != 3600000 {
		t.Fatalf("StartTimeMS = %d, want 3600000", doc.StartTimeMS)
	}
	want := []model.Cue{
		{StartMS: 1000, EndMS: 4500, Text: "Hello world"},
		{StartMS: 5000, EndMS: 7000, Text: "[MUSIC PLAYING]"},
		// Straddles the origin: the in-point clamps to 0 rather than going negative.
		{StartMS: 0, EndMS: 500, Text: "Straddles the origin"},
		// The cue entirely before the origin is dropped (see warnings below).
	}
	assertCues(t, doc.Cues, want)
	if !hasWarning(doc.Warnings, "cue_before_start_time") {
		t.Errorf("expected cue_before_start_time warning, got %v", doc.Warnings)
	}
	if !hasWarning(doc.Warnings, "cue_clamped_to_start_time") {
		t.Errorf("expected cue_clamped_to_start_time warning, got %v", doc.Warnings)
	}
}

// TestParseStartTimeRateResolvedLate guards the staging order: a <StartTime> that
// appears before the rate elements must still convert at the resolved rate.
func TestParseStartTimeRateResolvedLate(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?>
<SubtitleReel xmlns="x"><Language>en</Language><StartTime>00:00:10:00</StartTime>
<SubtitleList><Font>
<Subtitle TimeIn="00:00:12:00" TimeOut="00:00:14:00"><Text>late rate</Text></Subtitle>
</Font></SubtitleList><EditRate>25 1</EditRate></SubtitleReel>`
	doc, err := ParseBytes([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if doc.StartTimeMS != 10000 {
		t.Fatalf("StartTimeMS = %d, want 10000", doc.StartTimeMS)
	}
	assertCues(t, doc.Cues, []model.Cue{{StartMS: 2000, EndMS: 4000, Text: "late rate"}})
}

// TestParseZeroStartTimeUnchanged pins the no-regression case: the shipped
// fixtures declare 00:00:00:00 (or nothing) and must be untouched by the origin
// normalization.
func TestParseZeroStartTimeUnchanged(t *testing.T) {
	for _, name := range []string{
		"smpte_428-7_24fps.xml",
		"smpte_428-10_closed_25fps.xml",
		"interop_cinecanvas.xml",
	} {
		doc := parseFixture(t, name)
		if doc.StartTimeMS != 0 {
			t.Errorf("%s: StartTimeMS = %d, want 0", name, doc.StartTimeMS)
		}
	}
}

// TestParseCarriesOpaqueStyleAndPosition is the §5.4 "carry style/position as
// opaque pass-through fields" requirement: styling is not rendered, but it must
// survive the parse so a later fidelity pass has something to interpret.
func TestParseCarriesOpaqueStyleAndPosition(t *testing.T) {
	tests := []struct {
		fixture  string
		style    map[string]string
		position map[string]string
	}{
		{
			fixture:  "smpte_428-7_24fps.xml",
			style:    map[string]string{"id": "theFont", "color": "FFFFFFFF", "weight": "normal", "size": "42"},
			position: map[string]string{"valign": "bottom", "vposition": "8"},
		},
		{
			fixture:  "interop_cinecanvas.xml",
			style:    map[string]string{"id": "Arial", "color": "FFFFFFFF", "effect": "border", "size": "42"},
			position: map[string]string{"valign": "bottom", "vposition": "10"},
		},
		{
			fixture:  "smpte_428-10_closed_25fps.xml",
			style:    map[string]string{"id": "f1", "size": "40"},
			position: nil, // this fixture declares no positioning
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			doc := parseFixture(t, tt.fixture)
			if len(doc.Cues) == 0 {
				t.Fatalf("no cues parsed")
			}
			c := doc.Cues[0]
			for k, want := range tt.style {
				if got := c.Style[k]; got != want {
					t.Errorf("style[%q] = %q, want %q (style=%v)", k, got, want, c.Style)
				}
			}
			for k, want := range tt.position {
				if got := c.Position[k]; got != want {
					t.Errorf("position[%q] = %q, want %q (position=%v)", k, got, want, c.Position)
				}
			}
			if tt.position == nil && len(c.Position) != 0 {
				t.Errorf("expected no position, got %v", c.Position)
			}
			// Timing/identity attributes are not styling and must not leak in.
			for _, k := range []string{"timein", "timeout", "spotnumber"} {
				if _, ok := c.Style[k]; ok {
					t.Errorf("style must not carry %q: %v", k, c.Style)
				}
			}
		})
	}
}

// TestParseAllCuesBeforeStartTimeWarnsLoudly: if the origin swallows EVERY cue
// the asset's <StartTime> does not mean what this PROVISIONAL grammar assumes and
// the reel loses all captions. That is a different failure class from one stray
// early cue and must be reported as such, not as n per-cue warnings (§5.4).
func TestParseAllCuesBeforeStartTimeWarnsLoudly(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?>
<SubtitleReel xmlns="x"><Language>en</Language><EditRate>24 1</EditRate>
<TimeCodeRate>24</TimeCodeRate><StartTime>01:00:00:00</StartTime>
<SubtitleList><Font>
<Subtitle TimeIn="00:00:01:00" TimeOut="00:00:04:00"><Text>one</Text></Subtitle>
<Subtitle TimeIn="00:00:05:00" TimeOut="00:00:07:00"><Text>two</Text></Subtitle>
</Font></SubtitleList></SubtitleReel>`
	doc, err := ParseBytes([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(doc.Cues) != 0 {
		t.Fatalf("cues = %+v, want none", doc.Cues)
	}
	if !hasWarning(doc.Warnings, "all_cues_before_start_time") {
		t.Errorf("expected all_cues_before_start_time, got %v", doc.Warnings)
	}
}

// TestParseOneEarlyCueIsNotAWholeAssetFailure is the counterpart: a single
// dropped cue must NOT raise the asset-wide class.
func TestParseOneEarlyCueIsNotAWholeAssetFailure(t *testing.T) {
	doc := parseFixture(t, "smpte_428-7_starttime_24fps.xml")
	if hasWarning(doc.Warnings, "all_cues_before_start_time") {
		t.Errorf("one early cue must not read as a whole-asset failure: %v", doc.Warnings)
	}
}

func hasWarning(warnings []string, want string) bool {
	for _, w := range warnings {
		if w == want {
			return true
		}
	}
	return false
}

func TestParseMalformedXMLErrors(t *testing.T) {
	_, err := ParseBytes([]byte(`<SubtitleReel><Subtitle TimeIn=`))
	if err == nil {
		t.Fatalf("expected error for malformed XML")
	}
}

func TestParseUnknownRootErrors(t *testing.T) {
	_, err := ParseBytes([]byte(`<?xml version="1.0"?><SomethingElse/>`))
	if err == nil {
		t.Fatalf("expected error for unknown root element")
	}
}

func TestParseBadCueSkippedNotFatal(t *testing.T) {
	// A document with one good cue and one with an invalid TimeOut: the good cue
	// survives, the bad one is dropped with a warning (degrade, not crash — §5.4).
	xml := `<?xml version="1.0"?>
<SubtitleReel xmlns="x"><Language>en</Language><EditRate>24 1</EditRate><TimeCodeRate>24</TimeCodeRate>
<SubtitleList><Font>
<Subtitle TimeIn="00:00:01:00" TimeOut="00:00:02:00"><Text>good</Text></Subtitle>
<Subtitle TimeIn="00:00:03:00" TimeOut="garbage"><Text>bad</Text></Subtitle>
</Font></SubtitleList></SubtitleReel>`
	doc, err := ParseBytes([]byte(xml))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(doc.Cues) != 1 || doc.Cues[0].Text != "good" {
		t.Fatalf("expected 1 good cue, got %+v", doc.Cues)
	}
	if len(doc.Warnings) == 0 {
		t.Fatalf("expected a warning for the bad cue")
	}
}

func assertCues(t *testing.T, got, want []model.Cue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("cue count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].StartMS != want[i].StartMS || got[i].EndMS != want[i].EndMS || got[i].Text != want[i].Text {
			t.Errorf("cue[%d] = {%d,%d,%q}, want {%d,%d,%q}",
				i, got[i].StartMS, got[i].EndMS, got[i].Text,
				want[i].StartMS, want[i].EndMS, want[i].Text)
		}
	}
}
