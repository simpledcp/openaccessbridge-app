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
