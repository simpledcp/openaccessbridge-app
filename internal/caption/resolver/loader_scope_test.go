package resolver

import (
	"context"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/rpl"
	"github.com/simpledcp/captionbridge/internal/model"
)

// oneReelRPL builds a single-reel RPL whose one resource carries the given ID and
// locator, at 24 fps / 1440 units (60000 ms).
func oneReelRPL(resID, locator string) *rpl.RPL {
	return &rpl.RPL{Reels: []rpl.Reel{{ReelNumber: 1, Resources: []rpl.Resource{{
		ID: resID, Kind: model.TrackClosedCaption, Language: "en", Locator: locator,
		EditRate: rateUnits(24), IntrinsicDurationUnits: 1440,
	}}}}}
}

// TestCacheKeyedByScopeAndLocator: two shows that reuse the SAME resource ID must
// never see each other's timed text. Keyed by resource ID alone (the old
// behavior), show B rendered show A's captions (§5.3 transient cache).
func TestCacheKeyedByScopeAndLocator(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{
		"showA.xml": []byte(ttDoc("en", "Feature A", 0, 5000)),
		"showB.xml": []byte(ttDoc("en", "Feature B", 0, 5000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)

	cases := []struct {
		name    string
		scope   string
		locator string
		want    string
	}{
		{"show A", "playout-1", "showA.xml", "Feature A"},
		{"show B reuses the resource ID", "playout-2", "showB.xml", "Feature B"},
		{"show A again from cache", "playout-1", "showA.xml", "Feature A"},
	}
	for _, tc := range cases {
		cues, _, err := loader.Load(context.Background(), WindowRequest{
			RPL: oneReelRPL("res-shared", tc.locator), Languages: []string{"en"}, Scope: tc.scope,
		})
		if err != nil {
			t.Fatalf("%s: Load: %v", tc.name, err)
		}
		got := cues["en"]
		if len(got) != 1 || got[0].Text != tc.want {
			t.Errorf("%s: cue = %+v, want text %q", tc.name, got, tc.want)
		}
	}
}

// TestLoaderPurgeClearsTransientCache: guardrail #5 — nothing fetched survives a
// show boundary, and the machine can reach the cache to make that true (§5.3
// "eviction at reel and show boundaries").
func TestLoaderPurgeClearsTransientCache(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{"a.xml": []byte(ttDoc("en", "Cue", 0, 1000))}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	if _, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: oneReelRPL("r1", "a.xml"), Languages: []string{"en"}, Scope: "playout-1",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loader.CachedBytes() == 0 {
		t.Fatalf("expected the fetched asset to be cached")
	}
	loader.Purge()
	if got := loader.CachedBytes(); got != 0 {
		t.Fatalf("cached bytes after purge = %d, want 0", got)
	}
	// Purge is safe on a loader with no cache configured.
	NewLoader(NewPipeline(NewPluggable(mb, nil, nil), nil, nil), 0).Purge()
}

// TestRetainWindowEvictsPastReelsOnly: reel-boundary eviction (§5.3) must drop the
// reel already played without evicting the look-ahead reel it just pre-fetched —
// otherwise the boundary is exactly where a re-fetch stall would land.
func TestRetainWindowEvictsPastReelsOnly(t *testing.T) {
	reel := func(n int, resID, loc string) rpl.Reel {
		return rpl.Reel{ReelNumber: n, Resources: []rpl.Resource{{
			ID: resID, Kind: model.TrackClosedCaption, Language: "en", Locator: loc,
			EditRate: rateUnits(24), IntrinsicDurationUnits: 1440,
		}}}
	}
	r := &rpl.RPL{Reels: []rpl.Reel{
		reel(1, "r1", "r1.xml"), reel(2, "r2", "r2.xml"), reel(3, "r3", "r3.xml"),
	}}
	mb := &MemoryFetcher{Files: map[string][]byte{
		"r1.xml": []byte(ttDoc("en", "R1", 0, 1000)),
		"r2.xml": []byte(ttDoc("en", "R2", 0, 1000)),
		"r3.xml": []byte(ttDoc("en", "R3", 0, 1000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 1)

	if _, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: r, CurrentReelIdx: 0, Languages: []string{"en"}, Scope: "seg",
	}); err != nil {
		t.Fatalf("load window 0: %v", err)
	}
	if got := loader.Pipeline.Cache.Len(); got != 2 {
		t.Fatalf("cached assets after window {0,1} = %d, want 2", got)
	}
	// Cross into reel 2: reel 1 goes, the pre-fetched reel 2 stays.
	loader.RetainWindow("seg", 1)
	if _, ok := loader.Pipeline.Cache.Get(ReelScope("seg", 0) + "|r1|r1.xml"); ok {
		t.Errorf("reel 1 asset should have been evicted at the reel boundary")
	}
	if _, ok := loader.Pipeline.Cache.Get(ReelScope("seg", 1) + "|r2|r2.xml"); !ok {
		t.Errorf("reel 2 asset (still in the window) must not be evicted")
	}
	// Another segment's content never survives a boundary.
	loader.Pipeline.Cache.Put(ReelScope("other-show", 0)+"|x|x.xml", []byte("stale"))
	loader.RetainWindow("seg", 1)
	if _, ok := loader.Pipeline.Cache.Get(ReelScope("other-show", 0) + "|x|x.xml"); ok {
		t.Errorf("another segment's content survived the boundary eviction")
	}
}

// TestLoaderBaseOffsetAnchorsSegment: an RPL is one timeline SEGMENT, so the
// feature RPL that follows a 60 s trailer anchors its cues after the trailer.
func TestLoaderBaseOffsetAnchorsSegment(t *testing.T) {
	mb := &MemoryFetcher{Files: map[string][]byte{"f.xml": []byte(ttDoc("en", "Feature", 1000, 5000))}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	cues, _, err := loader.Load(context.Background(), WindowRequest{
		RPL: oneReelRPL("r1", "f.xml"), Languages: []string{"en"}, BaseOffsetMS: 60000,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cues["en"]
	if len(got) != 1 || got[0].StartMS != 61000 || got[0].EndMS != 65000 {
		t.Fatalf("segment-anchored cue = %+v, want [61000,65000)", got)
	}
}

// TestLoaderSubtractsReelEntryPoint: cue times are asset-relative, so a reel that
// starts partway into its asset shifts back by the entry point (§5.3 anchoring).
func TestLoaderSubtractsReelEntryPoint(t *testing.T) {
	r := oneReelRPL("r1", "t.xml")
	r.Reels[0].EditRate = rateUnits(24)
	r.Reels[0].EntryPointUnits = 48 // 2000 ms
	r.Reels[0].DurationUnits = 1440
	mb := &MemoryFetcher{Files: map[string][]byte{"t.xml": []byte(ttDoc("en", "Trimmed", 5000, 6000))}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil), 0)
	cues, _, err := loader.Load(context.Background(), WindowRequest{RPL: r, Languages: []string{"en"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cues["en"]
	if len(got) != 1 || got[0].StartMS != 3000 || got[0].EndMS != 4000 {
		t.Fatalf("trimmed cue = %+v, want [3000,4000)", got)
	}
}
