package rpl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func load(t *testing.T, name string) *RPL {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	r, err := ParseBytes(b)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", name, err)
	}
	return r
}

func TestParseMultiLang(t *testing.T) {
	r := load(t, "rpl_multilang.xml")
	if r.PlayoutID != "1" {
		t.Fatalf("playout id = %q", r.PlayoutID)
	}
	if len(r.Reels) != 2 {
		t.Fatalf("reels = %d, want 2", len(r.Reels))
	}
	if r.Reels[0].ReelNumber != 1 || r.Reels[1].ReelNumber != 2 {
		t.Fatalf("reel numbers = %d,%d", r.Reels[0].ReelNumber, r.Reels[1].ReelNumber)
	}
	if len(r.Reels[0].Resources) != 3 {
		t.Fatalf("reel1 resources = %d, want 3", len(r.Reels[0].Resources))
	}

	langs := r.Languages()
	if len(langs) != 2 || langs[0] != "en-US" || langs[1] != "es-419" {
		t.Fatalf("languages = %v", langs)
	}

	// Reel durations / offsets: reel1 = 1440 units @ 24fps = 60000ms.
	if d := r.Reels[0].DurationMS(); d != 60000 {
		t.Fatalf("reel1 duration = %d, want 60000", d)
	}
	if off := r.ReelOffsetMS(1); off != 60000 {
		t.Fatalf("reel2 offset = %d, want 60000", off)
	}
	if off := r.ReelOffsetMS(0); off != 0 {
		t.Fatalf("reel1 offset = %d, want 0", off)
	}
}

func TestSelectResourcePrefersClosedCaption(t *testing.T) {
	r := load(t, "rpl_multilang.xml")
	// Reel 1 has en-US ClosedCaption AND en-US ClosedSubtitle; CC must win.
	res, ok := r.Reels[0].SelectResource("en-US")
	if !ok {
		t.Fatalf("expected an en-US resource")
	}
	if res.Kind != model.TrackClosedCaption {
		t.Fatalf("selected kind = %v, want ClosedCaption", res.Kind)
	}
	if res.Locator != "captions/reel1_en.xml" {
		t.Fatalf("selected locator = %q", res.Locator)
	}
	// es-419 has only a subtitle track.
	es, ok := r.Reels[0].SelectResource("es-419")
	if !ok || es.Kind != model.TrackClosedSubtitle {
		t.Fatalf("es selection = %+v ok=%v", es, ok)
	}
	// A missing language returns false.
	if _, ok := r.Reels[0].SelectResource("de"); ok {
		t.Fatalf("unexpected resource for de")
	}
}

func TestEmptyReelResourcesNotError(t *testing.T) {
	r := load(t, "rpl_empty_reel.xml")
	if r.PlayoutID != "0" {
		t.Fatalf("playout id = %q, want 0", r.PlayoutID)
	}
	if len(r.Reels) != 1 {
		t.Fatalf("reels = %d, want 1", len(r.Reels))
	}
	if len(r.Reels[0].Resources) != 0 {
		t.Fatalf("expected 0 resources (no captions for this segment), got %d", len(r.Reels[0].Resources))
	}
	if len(r.Languages()) != 0 {
		t.Fatalf("expected no languages for empty RPL")
	}
}

func TestResourceLocatorForms(t *testing.T) {
	r := load(t, "rpl_multilang.xml")
	// Server-relative reference and an https:// locator both round-trip raw.
	if got := r.Reels[0].Resources[0].Locator; got != "captions/reel1_en.xml" {
		t.Fatalf("server-relative locator = %q", got)
	}
	if got := r.Reels[0].Resources[2].Locator; got != "https://mediablock.local/aux/reel1_es.xml" {
		t.Fatalf("https locator = %q", got)
	}
}

func TestParseMalformedErrors(t *testing.T) {
	if _, err := ParseBytes([]byte(`<AuxResourcePresentationList><Reel><Resource`)); err == nil {
		t.Fatalf("expected error for malformed RPL")
	}
}
