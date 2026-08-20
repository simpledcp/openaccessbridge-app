// Package soak is the §11.6 soak/reliability layer: "multi-hour runs with show
// changes, reconnects, and device hot-plug; memory/leak watch (content must
// purge at show end)".
//
// A cinema appliance runs unattended for weeks. The failures that matter at
// that timescale are not the ones a scenario suite catches — they are the slow
// ones: a cache that grows a little every show, an engine that is never retired,
// a goroutine leaked per reconnect, caption content that survives a show
// boundary. Each is invisible in a single pass and fatal by the fourth week, and
// the last one is a guardrail-#5 violation (no caption content persists) rather
// than merely a leak.
//
// The soak drives the real caption path — ACSP state machine, resolver, cache,
// timing engines, fan-out — over many show cycles on a fake clock, so a
// "multi-hour" run takes seconds and is deterministic enough for CI. Cycle count
// is the knob: CI runs a short soak on every build; an operator can run a long
// one before a pilot.
package soak

import (
	"fmt"
	"runtime"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/sim"
)

// Config parameterizes a soak run.
type Config struct {
	// Cycles is how many complete show cycles to run. Each cycle is a full
	// show: connect, lease, timeline, RPL, playback across reels, then a show
	// change — plus the disturbance for that cycle (see Cycle).
	Cycles int
	// Languages is how many caption languages each show carries. More languages
	// means more transient content per show, which is what must purge.
	Languages int
	// ReelsPerShow is how many reels each show's RPL declares.
	ReelsPerShow int
	// ReelMS is each reel's duration on the composition timeline.
	ReelMS int64
	// TimelineCadence is how often the server pushes Update Timeline.
	TimelineCadence time.Duration
	// WarmupCycles run before the first memory sample, so one-time allocations
	// (parser tables, buffers reaching steady size) are not read as a leak.
	WarmupCycles int
}

// DefaultConfig is the short soak CI runs on every build. It is long enough to
// expose per-cycle growth and short enough to keep the suite fast.
func DefaultConfig() Config {
	return Config{
		Cycles:          40,
		Languages:       3,
		ReelsPerShow:    4,
		ReelMS:          20_000,
		TimelineCadence: 2 * time.Second,
		WarmupCycles:    3,
	}
}

// Sample is a resource reading taken between cycles.
type Sample struct {
	Cycle      int
	HeapBytes  uint64
	Goroutines int
	CacheBytes int64
	// InShowCacheBytes is the transient cache's high-water mark DURING the show,
	// before the boundary purge. It must be non-zero, or the purge assertion is
	// only proving that nothing was ever loaded.
	InShowCacheBytes int64
	// Subscribers is the fan-out's live subscriber count. Patrons come and go
	// across a soak; the hub must not accumulate them.
	Subscribers int
}

// Result is what a soak run observed.
type Result struct {
	Cycles    int
	First     Sample
	Last      Sample
	Peak      Sample
	Failures  []string
	Purges    int // show boundaries at which the transient cache was verified empty
	Reconnect int // reconnect disturbances exercised
	ShowChg   int // show changes exercised
}

// Failed reports whether any invariant broke during the run.
func (r Result) Failed() bool { return len(r.Failures) > 0 }

// Summary renders the run for an operator or a CI log. It reports counts and
// sizes only — never caption text (§9.4).
func (r Result) Summary() string {
	return fmt.Sprintf(
		"soak: %d cycles (%d show changes, %d reconnects, %d verified purges)\n"+
			"  heap:        %s → %s (peak %s)\n"+
			"  goroutines:  %d → %d (peak %d)\n"+
			"  cache bytes: %d → %d after each show (in-show peak %d)\n"+
			"  subscribers: %d → %d\n"+
			"  failures:    %d",
		r.Cycles, r.ShowChg, r.Reconnect, r.Purges,
		humanBytes(r.First.HeapBytes), humanBytes(r.Last.HeapBytes), humanBytes(r.Peak.HeapBytes),
		r.First.Goroutines, r.Last.Goroutines, r.Peak.Goroutines,
		r.First.CacheBytes, r.Last.CacheBytes, r.Peak.InShowCacheBytes,
		r.First.Subscribers, r.Last.Subscribers,
		len(r.Failures))
}

func humanBytes(b uint64) string {
	switch {
	case b > 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b > 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Run executes the soak and returns what it observed. It never panics on an
// invariant break: it records the failure and keeps going, so one bad cycle
// does not hide the trend in the ones after it.
func Run(cfg Config) Result {
	cfg = withDefaults(cfg)
	res := Result{Cycles: cfg.Cycles}

	h := sim.NewHarness(sim.HarnessOptions{
		Files:              map[string][]byte{},
		PreferredLanguages: languages(cfg.Languages),
		FetchAheadReels:    1,
	})

	// Hold a patron subscriber open across the whole soak, the way a real
	// listener stays on the page between reels — the engines and hub must serve
	// it for the entire run without accumulating state per show.
	primary := languages(cfg.Languages)[0]
	h.Caption(primary) // registers the long-lived subscriber

	fail := func(cycle int, format string, args ...any) {
		res.Failures = append(res.Failures,
			fmt.Sprintf("cycle %d: %s", cycle, fmt.Sprintf(format, args...)))
	}

	for cycle := 0; cycle < cfg.WarmupCycles+cfg.Cycles; cycle++ {
		counted := cycle >= cfg.WarmupCycles
		inShowCache := runCycle(h, cfg, cycle, counted, &res, fail)

		if !counted {
			continue
		}
		n := cycle - cfg.WarmupCycles
		s := sample(h, n)
		// CacheBytes is the post-boundary reading (must be 0); InShowCacheBytes
		// is the high-water mark that proves content was there to purge.
		s.InShowCacheBytes = inShowCache
		if n == 0 {
			res.First = s
			res.Peak = s
		}
		res.Last = s
		if s.HeapBytes > res.Peak.HeapBytes {
			res.Peak.HeapBytes = s.HeapBytes
		}
		if s.Goroutines > res.Peak.Goroutines {
			res.Peak.Goroutines = s.Goroutines
		}
		if s.CacheBytes > res.Peak.CacheBytes {
			res.Peak.CacheBytes = s.CacheBytes
		}
		if s.InShowCacheBytes > res.Peak.InShowCacheBytes {
			res.Peak.InShowCacheBytes = s.InShowCacheBytes
		}
	}

	h.Ctrl.Close()
	return res
}

func withDefaults(cfg Config) Config {
	d := DefaultConfig()
	if cfg.Cycles <= 0 {
		cfg.Cycles = d.Cycles
	}
	if cfg.Languages <= 0 {
		cfg.Languages = d.Languages
	}
	if cfg.ReelsPerShow <= 0 {
		cfg.ReelsPerShow = d.ReelsPerShow
	}
	if cfg.ReelMS <= 0 {
		cfg.ReelMS = d.ReelMS
	}
	if cfg.TimelineCadence <= 0 {
		cfg.TimelineCadence = d.TimelineCadence
	}
	return cfg
}

// runCycle drives one complete show, then the disturbance for this cycle, then
// the show boundary at which content must be gone. It returns the cache's
// in-show high-water mark: sampling only after the boundary always reads zero,
// which would make the purge assertions vacuous.
func runCycle(h *sim.Harness, cfg Config, cycle int, counted bool, res *Result, fail func(int, string, ...any)) int64 {
	playout := fmt.Sprintf("show-%d", cycle)
	// The media block serves each show's assets; it does not hand us the
	// previous show's forever. Retiring them keeps the FIXTURE from growing and
	// being misread as the appliance leaking.
	retireShowFiles(h, cycle-1, cfg)
	loadShow(h, cfg, cycle, playout)
	inShow := h.Cache.Used()

	// Play the show through, re-anchoring at the server's cadence.
	showMS := cfg.ReelMS * int64(cfg.ReelsPerShow)
	h.RunTimeline(cfg.TimelineCadence, showMS/2, playout)

	// Rotate the disturbance so a long run exercises each repeatedly rather
	// than exercising one and calling it a soak.
	switch cycle % 3 {
	case 0:
		// Reconnect mid-show: comms drop, then the full new-connection sequence.
		// §5.2 #2 says captions keep showing across the blip; what matters here
		// is that reconnecting repeatedly does not accumulate anything.
		h.Machine.CommsLost()
		h.AdvanceTick(2 * time.Second)
		h.Machine.Connected()
		h.Send(sim.Announce("soak-server", "Sim Auditorium", ""))
		h.Send(sim.GetNewLease(30_000))
		h.Send(sim.UpdateTimeline(showMS/2, playout, acsp.PlayPlaying))
		h.Send(sim.SetRPLInline(rplFor(h, cfg, cycle, playout)))
		h.Send(sim.SetOutputMode(true))
		if counted {
			res.Reconnect++
		}
	case 1:
		// A patron switching language mid-show, repeatedly. Each switch may
		// create an engine; none of them may outlive the show.
		langs := languages(cfg.Languages)
		for _, l := range langs {
			h.Caption(l)
			h.AdvanceEval(500 * time.Millisecond)
		}
	case 2:
		// Output mode toggling — the server reporting play/stop around an
		// uncaptioned trailer.
		h.Send(sim.SetOutputMode(false))
		h.AdvanceEval(time.Second)
		h.Send(sim.SetOutputMode(true))
		h.AdvanceEval(time.Second)
	}

	if used := h.Cache.Used(); used > inShow {
		inShow = used
	}
	h.RunTimeline(cfg.TimelineCadence, showMS, playout)
	if used := h.Cache.Used(); used > inShow {
		inShow = used
	}

	// The show boundary: Terminate Lease must drop the whole show — cues,
	// engines, and every byte of transient caption content (guardrail #5).
	h.Send(sim.TerminateLease())
	h.AdvanceEval(time.Second)
	if counted {
		res.ShowChg++
		if used := h.Cache.Used(); used != 0 {
			fail(cycle, "transient cache still holds %d bytes of caption content after the show ended "+
				"(guardrail #5: content must purge at show end)", used)
		} else {
			res.Purges++
		}
		if txt := h.Caption(languages(cfg.Languages)[0]); txt != "" {
			fail(cycle, "a caption is still being served after the show ended: %d chars", len(txt))
		}
	}
	return inShow
}

// retireShowFiles drops a finished show's assets from the fake media block.
// Without it the FIXTURE grows by one file per reel per language per cycle and
// a long soak reports its own bookkeeping as an appliance leak.
func retireShowFiles(h *sim.Harness, cycle int, cfg Config) {
	if cycle < 0 {
		return
	}
	for r := 0; r < cfg.ReelsPerShow; r++ {
		for _, lang := range languages(cfg.Languages) {
			delete(h.Fetcher.Files, fmt.Sprintf("tt_c%d_r%d_%s.xml", cycle%2, r, lang))
		}
	}
}

// loadShow runs the canonical new-connection sequence for a fresh show.
func loadShow(h *sim.Harness, cfg Config, cycle int, playout string) {
	h.Machine.Connected()
	h.Send(sim.Announce("soak-server", "Sim Auditorium", ""))
	h.Send(sim.GetNewLease(30_000))
	h.Send(sim.UpdateTimeline(0, playout, acsp.PlayPlaying))
	h.Send(sim.SetRPLInline(rplFor(h, cfg, cycle, playout)))
	h.Send(sim.SetOutputMode(true))
}

// rplFor builds a distinct show for this cycle and installs its timed text.
//
// Locator and resource IDs deliberately ALTERNATE over a two-show namespace
// rather than being unique per cycle. That is both more realistic — a real
// media block reuses resource IDs across shows — and stricter: it exercises the
// cache's per-show scoping, so a stale entry surfaces as the previous show's
// text rather than as a slow leak. It also keeps the simulator's own
// per-locator fetch counters bounded, so a long run measures the appliance's
// retention and not the harness's bookkeeping.
func rplFor(h *sim.Harness, cfg Config, cycle int, playout string) []byte {
	langs := languages(cfg.Languages)
	reels := make([]sim.ReelSpec, 0, cfg.ReelsPerShow)
	for r := 0; r < cfg.ReelsPerShow; r++ {
		var resources []sim.ResSpec
		for _, lang := range langs {
			locator := fmt.Sprintf("tt_c%d_r%d_%s.xml", cycle%2, r, lang)
			h.Fetcher.Files[locator] = sim.TimedText(lang, cuesFor(cycle, r, lang, cfg.ReelMS))
			resources = append(resources, sim.ResSpec{
				ID:       fmt.Sprintf("res-c%d-r%d-%s", cycle%2, r, lang),
				Type:     "ClosedCaption",
				Language: lang,
				Locator:  locator,
			})
		}
		reels = append(reels, sim.ReelSpec{
			Number: r + 1, DurationMS: cfg.ReelMS, Resources: resources,
		})
	}
	return sim.RPL(playout, reels)
}

// cuesFor makes a reel's worth of cues, dense enough that a leak shows up in
// the heap but small enough to keep a long soak quick.
func cuesFor(cycle, reel int, lang string, reelMS int64) []sim.CueSpec {
	const cueCount = 12
	step := reelMS / cueCount
	out := make([]sim.CueSpec, 0, cueCount)
	for i := 0; i < cueCount; i++ {
		start := int64(i) * step
		out = append(out, sim.CueSpec{
			StartMS: start,
			EndMS:   start + step/2,
			Text:    fmt.Sprintf("c%d r%d %s cue %d", cycle, reel, lang, i),
		})
	}
	return out
}

func languages(n int) []string {
	all := []string{"en", "es-419", "fr", "de", "pt-BR", "ja"}
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// sample forces a GC and reads the resource counters. The GC matters: without
// it the heap reading is dominated by uncollected garbage and says nothing
// about what is actually retained.
func sample(h *sim.Harness, cycle int) Sample {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Sample{
		Cycle:       cycle,
		HeapBytes:   ms.HeapAlloc,
		Goroutines:  runtime.NumGoroutine(),
		CacheBytes:  h.Cache.Used(),
		Subscribers: h.Hub.ClientCount(),
	}
}

// CheckGrowth is the leak assertion: over a soak, retained resources must be
// FLAT, not merely bounded. It returns the failures found, so a caller can add
// them to a Result or report them from a test.
//
// Heap gets a tolerance because a Go heap legitimately fluctuates with
// allocator behavior; goroutines and cache bytes do not — a per-cycle goroutine
// leak or any retained caption content is a real fault, so both are exact.
func CheckGrowth(r Result, heapTolerance float64) []string {
	var out []string
	if r.Last.Goroutines > r.First.Goroutines {
		out = append(out, fmt.Sprintf(
			"goroutines grew %d → %d across %d cycles — something is leaked per show",
			r.First.Goroutines, r.Last.Goroutines, r.Cycles))
	}
	if r.Last.CacheBytes != 0 {
		out = append(out, fmt.Sprintf(
			"transient cache holds %d bytes at the end of the soak, want 0 (guardrail #5)",
			r.Last.CacheBytes))
	}
	if r.First.HeapBytes > 0 {
		growth := float64(r.Last.HeapBytes) / float64(r.First.HeapBytes)
		if growth > heapTolerance {
			out = append(out, fmt.Sprintf(
				"heap grew %.2fx (%s → %s) across %d cycles, over the %.2fx tolerance",
				growth, humanBytes(r.First.HeapBytes), humanBytes(r.Last.HeapBytes),
				r.Cycles, heapTolerance))
		}
	}
	return out
}

// warningCodes is a convenience for a caller that wants to assert the soak
// surfaced the degradations it induced rather than swallowing them.
func warningCodes(ws []model.Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}
