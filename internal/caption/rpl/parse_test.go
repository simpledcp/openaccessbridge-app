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

// TestReelLevelDurationAnchorsUncaptionedReel covers the reel whose ResourceList
// is legitimately empty (§5.2 item 5): its duration can only come from the
// reel-level <Duration>, and without it every following reel anchors at the wrong
// composition offset (§5.3).
func TestReelLevelDurationAnchorsUncaptionedReel(t *testing.T) {
	r := load(t, "rpl_uncaptioned_middle_reel.xml")
	if len(r.Reels) != 3 {
		t.Fatalf("reels = %d, want 3", len(r.Reels))
	}
	if n := len(r.Reels[1].Resources); n != 0 {
		t.Fatalf("reel2 resources = %d, want 0 (uncaptioned reel)", n)
	}
	for i, want := range []int64{60000, 60000, 60000} {
		if got := r.Reels[i].DurationMS(); got != want {
			t.Errorf("reel%d duration = %d, want %d", i+1, got, want)
		}
	}
	for i, want := range []int64{0, 60000, 120000} {
		if got := r.ReelOffsetMS(i); got != want {
			t.Errorf("reel%d offset = %d, want %d", i+1, got, want)
		}
	}
	if got, want := r.TotalDurationMS(), int64(180000); got != want {
		t.Errorf("total duration = %d, want %d", got, want)
	}
	// Reel 3 is trimmed 48 units (2000 ms) into its asset; a cue at asset time t
	// therefore lands at reelOffset + t - 2000 on the composition timeline.
	if got, want := r.Reels[2].EntryPointMS(), int64(2000); got != want {
		t.Errorf("reel3 entry point = %d, want %d", got, want)
	}
	res, ok := r.Reels[2].SelectResource("en-US")
	if !ok {
		t.Fatalf("expected an en-US resource on reel 3")
	}
	if got, want := r.Reels[2].TrimMS(res), int64(2000); got != want {
		t.Errorf("reel3 trim = %d, want %d (reel entry point, resource has none)", got, want)
	}
}

// TestReelDurationFallsBackToResources: an RPL with no reel-level <Duration> keeps
// the previous heuristic (longest resource) so existing content still anchors.
func TestReelDurationFallsBackToResources(t *testing.T) {
	r := load(t, "rpl_multilang.xml")
	if r.Reels[0].DurationUnits != 0 {
		t.Fatalf("fixture unexpectedly declares a reel-level duration")
	}
	if got := r.Reels[0].DurationMS(); got != 60000 {
		t.Fatalf("reel1 duration = %d, want 60000 from the longest resource", got)
	}
}

// TestReelIndexAt is the inverse of ReelOffsetMS: the mapping that lets the ACS
// fetch the current + next reel and skip reels already past (§5.3).
func TestReelIndexAt(t *testing.T) {
	r := load(t, "rpl_uncaptioned_middle_reel.xml")
	cases := []struct {
		name string
		pos  int64
		want int
	}{
		{"before start clamps to first", -5000, 0},
		{"start of reel 1", 0, 0},
		{"inside reel 1", 2000, 0},
		{"boundary into reel 2", 60000, 1},
		{"inside uncaptioned reel 2", 65000, 1},
		{"boundary into reel 3", 120000, 2},
		{"inside reel 3", 125000, 2},
		{"past the end clamps to last", 999000, 2},
	}
	for _, tc := range cases {
		if got := r.ReelIndexAt(tc.pos); got != tc.want {
			t.Errorf("%s: ReelIndexAt(%d) = %d, want %d", tc.name, tc.pos, got, tc.want)
		}
	}
	// An RPL with no reels never panics and never invents an index.
	empty := &RPL{}
	if got := empty.ReelIndexAt(1000); got != 0 {
		t.Errorf("empty RPL ReelIndexAt = %d, want 0", got)
	}
	// A reel of unknown duration cannot contain a position and is stepped over.
	unknown := &RPL{Reels: []Reel{{ReelNumber: 1}, {ReelNumber: 2}}}
	if got := unknown.ReelIndexAt(5000); got != 1 {
		t.Errorf("unknown-duration ReelIndexAt = %d, want 1 (last)", got)
	}
}
