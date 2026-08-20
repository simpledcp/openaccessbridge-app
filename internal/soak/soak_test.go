package soak

import (
	"os"
	"strconv"
	"testing"
)

// TestSoakShort is the §11.6 soak layer as CI runs it: enough show cycles to
// expose per-cycle retention, fast enough to sit in the normal suite.
//
// For a long run before a pilot, set CAPTIONBRIDGE_SOAK_CYCLES:
//
//	CAPTIONBRIDGE_SOAK_CYCLES=2000 go test ./internal/soak/ -run Soak -v -timeout 30m
func TestSoakShort(t *testing.T) {
	cfg := DefaultConfig()
	if v := os.Getenv("CAPTIONBRIDGE_SOAK_CYCLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("CAPTIONBRIDGE_SOAK_CYCLES=%q is not a positive integer", v)
		}
		cfg.Cycles = n
	}

	res := Run(cfg)
	t.Log("\n" + res.Summary())

	for _, f := range res.Failures {
		t.Error(f)
	}

	// The soak must actually have exercised what it claims to.
	if res.Purges != res.ShowChg || res.ShowChg == 0 {
		t.Errorf("verified purges (%d) should equal show changes (%d), and both must be non-zero",
			res.Purges, res.ShowChg)
	}
	if res.Reconnect == 0 {
		t.Error("the soak ran no reconnect disturbances")
	}

	// Retention must be flat. A 1.5x heap tolerance absorbs allocator noise
	// across a short run while still catching per-cycle accumulation; goroutines
	// and cached content are checked exactly.
	for _, f := range CheckGrowth(res, 1.5) {
		t.Error(f)
	}

	// Subscribers must be FLAT. The harness holds one per language for the whole
	// run, so the count is not expected to be 1 — it is expected not to grow.
	if res.Last.Subscribers > res.First.Subscribers {
		t.Errorf("fan-out subscribers grew %d → %d across %d cycles — a subscriber leaks per show",
			res.First.Subscribers, res.Last.Subscribers, res.Cycles)
	}
}

// A soak that silently does nothing would pass every assertion above, so pin
// that the harness really drives content through the caption path: with cues
// loaded, the cache must be non-empty DURING a show even though it is empty
// after one.
func TestSoakActuallyLoadsContent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Cycles = 2
	cfg.WarmupCycles = 0
	res := Run(cfg)

	if res.Peak.InShowCacheBytes == 0 {
		t.Error("the transient cache was never used — the soak is not loading caption content, " +
			"so its purge assertions prove nothing")
	}
	if res.Last.CacheBytes != 0 {
		t.Errorf("cache holds %d bytes after the final show ended", res.Last.CacheBytes)
	}
}

// CheckGrowth is the leak assertion the long soak leans on, so verify it
// actually reports each class of growth rather than always returning nothing.
func TestCheckGrowthDetectsEachLeakClass(t *testing.T) {
	base := Result{
		Cycles: 100,
		First:  Sample{HeapBytes: 1 << 20, Goroutines: 10, CacheBytes: 0},
		Last:   Sample{HeapBytes: 1 << 20, Goroutines: 10, CacheBytes: 0},
	}
	if got := CheckGrowth(base, 1.5); len(got) != 0 {
		t.Fatalf("a flat run should report nothing, got %v", got)
	}

	leakedGoroutines := base
	leakedGoroutines.Last.Goroutines = 11
	if got := CheckGrowth(leakedGoroutines, 1.5); len(got) != 1 {
		t.Errorf("one leaked goroutine must be reported exactly (no tolerance), got %v", got)
	}

	retainedContent := base
	retainedContent.Last.CacheBytes = 1
	if got := CheckGrowth(retainedContent, 1.5); len(got) != 1 {
		t.Errorf("a single retained byte of caption content must be reported (guardrail #5), got %v", got)
	}

	grownHeap := base
	grownHeap.Last.HeapBytes = 2 << 20
	if got := CheckGrowth(grownHeap, 1.5); len(got) != 1 {
		t.Errorf("a 2x heap should exceed a 1.5x tolerance, got %v", got)
	}
	if got := CheckGrowth(grownHeap, 3.0); len(got) != 0 {
		t.Errorf("a 2x heap should pass a 3.0x tolerance, got %v", got)
	}
}
