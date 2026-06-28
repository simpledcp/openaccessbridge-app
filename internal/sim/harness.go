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
	FetchAheadReels     int
	Log                 *logging.Logger
}

// Harness wires a fake clock, hub, controller, memory-backed resolver, loader, and
// the acsp.Machine into one deterministically-drivable unit.
type Harness struct {
	Clock   *Clock
	Hub     *fanout.Hub
	Ctrl    *caption.Controller
	Machine *acsp.Machine
	Fetcher *resolver.MemoryFetcher

	mu       sync.Mutex
	subs     map[string]*fanout.Subscriber
	warnings []model.Warning
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
	res := resolver.NewPluggable(fetcher, nil, log)
	loader := resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(8<<20), log), opts.FetchAheadReels)
	h := &Harness{Clock: clk, Hub: hub, Ctrl: ctrl, Fetcher: fetcher, subs: map[string]*fanout.Subscriber{}}
	h.Machine = acsp.New(acsp.Options{
		Controller:          ctrl,
		Resolver:            res,
		Loader:              loader,
		Clock:               clk.Now,
		Log:                 log,
		ExpectedFingerprint: opts.ExpectedFingerprint,
		PreferredLanguages:  opts.PreferredLanguages,
		FetchAheadReels:     opts.FetchAheadReels,
		OnWarning:           h.addWarning,
	})
	return h
}

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

// AssertCommsLost asserts the comms-loss flag (continue-until-lease behavior).
func (h *Harness) AssertCommsLost(t T, want bool) {
	t.Helper()
	if got := h.Machine.CommsLost(); got != want {
		t.Errorf("commsLost = %v, want %v", got, want)
	}
}
