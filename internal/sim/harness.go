// Package sim is the ST 430-10 simulator (buildplan.md §11.1): a fake cinema
// server / IMB that emits realistic ACSP behavior so the caption path can be
// developed and regression-tested without a real server. It is the backbone of CI
// — every caption-path change runs the full scenario suite headless.
//
// The simulator drives the real acsp.Machine through its synchronous Handle/Tick
// API with an injected clock, so scenarios are deterministic and fast. The same
// content + request sequences can also drive the machine in real time over a
// ServerLink for the standalone acsp-simulator binary.
//
// PROVISIONAL: the modeled wire behavior follows public ISDCF notes (§5.1–5.2),
// seamed for the Phase 0A contract — see package acsp.
package sim

import (
	"context"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/acsp"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// T is the minimal assertion surface a scenario needs. *testing.T satisfies it;
// the standalone binary passes a logging implementation. This keeps the sim
// package free of a hard dependency on the testing package.
type T interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Helper()
}

// Clock is a deterministic fake clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Unix(1_000_000, 0)} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the fake clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// HarnessOptions configures a Harness.
type HarnessOptions struct {
	Files               map[string][]byte // media-block files (RPL + timed text)
	PreferredLanguages  []string
	ExpectedFingerprint string
	// ExpectedScreenName is the other half of the §8.5 pinned identity.
	ExpectedScreenName string
	FetchAheadReels    int
	// EagerLanguages caps the up-front language fetch (§5.3); 0 fetches all.
	EagerLanguages int
	Log            *logging.Logger
}

// Harness wires a fake clock, hub, controller, memory-backed resolver, loader, and
// the acsp.Machine into one deterministically-drivable unit.
type Harness struct {
	Clock   *Clock
	Hub     *fanout.Hub
	Ctrl    *caption.Controller
	Machine *acsp.Machine
	Fetcher *resolver.MemoryFetcher
	// Cache is the transient timed-text cache, so a scenario can assert that
	// content really is gone at a show boundary (guardrail #5).
	Cache *resolver.Cache

	mu       sync.Mutex
	subs     map[string]*fanout.Subscriber
	warnings []model.Warning
	fetches  *recordingFetcher
}

// recordingFetcher counts media-block fetches per locator. Scenarios need it
// because §5.3's "skip fetching reels already past" is a statement about what is
// NOT fetched, which no caption assertion can demonstrate.
type recordingFetcher struct {
	inner  *resolver.MemoryFetcher
	mu     sync.Mutex
	counts map[string]int
}

func (r *recordingFetcher) Fetch(ctx context.Context, ref string) ([]byte, error) {
	r.mu.Lock()
	r.counts[ref]++
	r.mu.Unlock()
	return r.inner.Fetch(ctx, ref)
}

func (r *recordingFetcher) count(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[ref]
}

// NewHarness builds a Harness over the given media-block files.
func NewHarness(opts HarnessOptions) *Harness {
	clk := NewClock()
	log := opts.Log
	if log == nil {
		log = logging.Nop()
	}
	hub := fanout.NewHub(fanout.Options{Auditorium: "Sim Auditorium"})
	ctrl := caption.NewController(caption.Options{Hub: hub, Clock: clk.Now})
	fetcher := &resolver.MemoryFetcher{Files: opts.Files}
	if fetcher.Files == nil {
		fetcher.Files = map[string][]byte{}
	}
	rec := &recordingFetcher{inner: fetcher, counts: map[string]int{}}
	res := resolver.NewPluggable(rec, nil, log)
	cache := resolver.NewCache(8 << 20)
	loader := resolver.NewLoader(resolver.NewPipeline(res, cache, log), opts.FetchAheadReels)
	h := &Harness{Clock: clk, Hub: hub, Ctrl: ctrl, Fetcher: fetcher, Cache: cache,
		subs: map[string]*fanout.Subscriber{}, fetches: rec}
	h.Machine = acsp.New(acsp.Options{
		Controller:          ctrl,
		Resolver:            res,
		Loader:              loader,
		Clock:               clk.Now,
		Log:                 log,
		ExpectedFingerprint: opts.ExpectedFingerprint,
		ExpectedScreenName:  opts.ExpectedScreenName,
		PreferredLanguages:  opts.PreferredLanguages,
		FetchAheadReels:     opts.FetchAheadReels,
		EagerLanguages:      opts.EagerLanguages,
		OnWarning:           h.addWarning,
	})
	return h
}

// FetchCount returns how many times a media-block locator has been fetched.
func (h *Harness) FetchCount(ref string) int { return h.fetches.count(ref) }

func (h *Harness) addWarning(w model.Warning) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warnings = append(h.warnings, w)
}

// Warnings returns the loud warnings raised so far (e.g. wrong-room identity).
func (h *Harness) Warnings() []model.Warning {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.Warning, len(h.warnings))
	copy(out, h.warnings)
	return out
}

// HasWarning reports whether a warning with the given code was raised.
func (h *Harness) HasWarning(code string) bool {
	for _, w := range h.Warnings() {
		if w.Code == code {
			return true
		}
	}
	return false
}

// Send delivers a server→ACS request and returns the response.
func (h *Harness) Send(req acsp.Request) acsp.Response { return h.Machine.Handle(req) }

// Advance moves the fake clock forward without ticking.
func (h *Harness) Advance(d time.Duration) { h.Clock.Advance(d) }

// Tick runs the machine's time-based checks (lease/comms).
func (h *Harness) Tick() { h.Machine.Tick() }

// AdvanceTick advances the clock then ticks (the common pairing).
func (h *Harness) AdvanceTick(d time.Duration) {
	h.Clock.Advance(d)
	h.Machine.Tick()
}

// Evaluate forces the timing engines to re-resolve the active cue at the current
// clock (used to cross cue boundaries deterministically).
func (h *Harness) Evaluate() { h.Ctrl.Evaluate() }

// AdvanceEval advances the clock then re-resolves cues.
func (h *Harness) AdvanceEval(d time.Duration) {
	h.Clock.Advance(d)
	h.Ctrl.Evaluate()
}

// RunTimeline drives Update Timeline at a fixed cadence up to untilMS, advancing
// the fake clock in step and ticking as production would, with a status keepalive
// at the half-lease cadence so the lease does not lapse mid-run (§11.1 "timeline
// updates at a configurable cadence", §5.2 item 1). It leaves the engines
// evaluated at the final position.
func (h *Harness) RunTimeline(cadence time.Duration, untilMS int64, playoutID string) {
	step := cadence.Milliseconds()
	if step <= 0 {
		return
	}
	pos := h.State().TimelinePositionMS
	interval := h.State().RequestIntervalMS // == half the lease
	var sinceKeepalive int64
	for pos < untilMS {
		pos += step
		h.Clock.Advance(cadence)
		sinceKeepalive += step
		if interval > 0 && sinceKeepalive >= interval {
			h.Send(Status(interval * 2))
			sinceKeepalive = 0
		}
		h.Send(UpdateTimeline(pos, playoutID, acsp.PlayPlaying))
		h.Machine.Tick()
	}
	h.Ctrl.Evaluate()
}

func (h *Harness) sub(lang string) *fanout.Subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.subs[lang]; ok {
		return s
	}
	s, _ := h.Hub.Subscribe(lang)
	h.subs[lang] = s
	return s
}

// Caption returns the current caption text for a language (empty when cleared).
func (h *Harness) Caption(lang string) string {
	m := h.sub(lang).Peek()
	if m.Visible {
		return m.Text
	}
	return ""
}

// PatronStatus returns the current patron-facing status for a language.
func (h *Harness) PatronStatus(lang string) string { return h.sub(lang).Peek().Status }

// State returns the machine's caption-link state.
func (h *Harness) State() model.CaptionLinkState { return h.Machine.State() }

// --- Assertions ---

// AssertCaption asserts the current caption text for a language.
func (h *Harness) AssertCaption(t T, lang, want string) {
	t.Helper()
	if got := h.Caption(lang); got != want {
		t.Errorf("caption[%s] = %q, want %q", lang, got, want)
	}
}

// AssertNoCaption asserts no caption is currently shown for a language.
func (h *Harness) AssertNoCaption(t T, lang string) {
	t.Helper()
	if got := h.Caption(lang); got != "" {
		t.Errorf("caption[%s] = %q, want cleared", lang, got)
	}
}

// AssertState asserts the machine's ACSP state.
func (h *Harness) AssertState(t T, want model.ACSPState) {
	t.Helper()
	if got := h.State().State; got != want {
		t.Errorf("state = %s, want %s", got, want)
	}
}

// AssertNoCachedContent asserts the transient timed-text cache is empty — what
// guardrail #5 ("purged at show end") actually means in memory.
func (h *Harness) AssertNoCachedContent(t T) {
	t.Helper()
	if got := h.Cache.Used(); got != 0 {
		t.Errorf("transient cache holds %d bytes of caption content, want 0", got)
	}
}

// AssertFetchCount asserts how many times a media-block locator was fetched.
func (h *Harness) AssertFetchCount(t T, ref string, want int) {
	t.Helper()
	if got := h.FetchCount(ref); got != want {
		t.Errorf("fetches of %s = %d, want %d", ref, got, want)
	}
}

// AssertCommsLost asserts the comms-loss flag (continue-until-lease behavior).
func (h *Harness) AssertCommsLost(t T, want bool) {
	t.Helper()
	if got := h.Machine.CommsLost(); got != want {
		t.Errorf("commsLost = %v, want %v", got, want)
	}
}
