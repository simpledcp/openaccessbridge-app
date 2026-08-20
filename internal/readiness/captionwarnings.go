package readiness

import (
	"sort"
	"sync"

	"github.com/simpledcp/captionbridge/internal/model"
)

// DefaultLeaseWarnFraction mirrors acsp.Options.WarnFraction's default: the
// caption link warns once the remaining lease drops below this fraction of the
// granted lease (§5.2 item 1). It is the same number on both sides so a warning
// is cleared by exactly the condition that raised it.
const DefaultLeaseWarnFraction = 0.25

// CaptionWarnings is the live set of caption-path loud warnings (§8.3).
//
// The ACSP state machine raises warnings as one-shot EVENTS through
// Options.OnWarning — "the lease is close to lapsing", "the server would not
// serve an asset", "this is not the server this room is pinned to". The
// dashboard needs a SET instead: the conditions that are true NOW. This type
// holds each raised warning and drops it when the observed link state shows the
// condition has passed.
//
// Nothing here expires on a timer. A warning is cleared because something was
// MEASURED — the lease was renewed, the show changed, the link was torn down —
// never because enough seconds went by (guardrail #7: a condition that is still
// true must not quietly stop being reported).
//
// All methods are safe for concurrent use. Raise and Observe are called from the
// ACSP callbacks, which run with the Machine's own lock held, so neither may call
// back into the Machine — hence Observe recording the state rather than the set
// asking for it.
type CaptionWarnings struct {
	mu       sync.Mutex
	fraction float64
	link     model.CaptionLinkState
	raised   map[string]raisedWarning
}

// raisedWarning is one warning plus the link context it was raised against. The
// context is what the clearing rules compare the current state with.
type raisedWarning struct {
	w         model.Warning
	playoutID string
	server    string
}

// NewCaptionWarnings builds an empty set. warnFraction must match the machine's
// Options.WarnFraction; 0 uses DefaultLeaseWarnFraction.
func NewCaptionWarnings(warnFraction float64) *CaptionWarnings {
	if warnFraction <= 0 {
		warnFraction = DefaultLeaseWarnFraction
	}
	return &CaptionWarnings{fraction: warnFraction, raised: map[string]raisedWarning{}}
}

// Observe records the latest caption-link state. It is wired to
// acsp.Options.OnState and is also called by the dashboard poll before Snapshot,
// so the clearing rules always run against the freshest measurement available.
func (c *CaptionWarnings) Observe(link model.CaptionLinkState) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.link = link
	c.mu.Unlock()
}

// Raise records a warning raised by the caption link. A repeat of the same code
// refreshes the message and the context rather than accumulating duplicates —
// the dashboard shows conditions, not an event log.
func (c *CaptionWarnings) Raise(w model.Warning) {
	if c == nil || w.Code == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raised[w.Code] = raisedWarning{w: w, playoutID: c.link.CurrentPlayoutID, server: c.link.Server}
}

// Clear drops a code unconditionally (a caller that knows the condition ended).
func (c *CaptionWarnings) Clear(code string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.raised, code)
	c.mu.Unlock()
}

// Snapshot returns the warnings that still apply, ordered by code so the
// dashboard does not reshuffle between polls. Warnings whose condition the
// observed state shows has passed are dropped from the set as well as from the
// result: they are gone, not merely hidden.
func (c *CaptionWarnings) Snapshot() []model.Warning {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.Warning, 0, len(c.raised))
	for code, r := range c.raised {
		if c.staleLocked(r) {
			delete(c.raised, code)
			continue
		}
		out = append(out, r.w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	if len(out) == 0 {
		return nil
	}
	return out
}

// staleLocked applies the per-code clearing rule against the observed state.
func (c *CaptionWarnings) staleLocked(r raisedWarning) bool {
	link := c.link
	switch r.w.Code {
	case model.WarnCaptionLeaseNearExpiry:
		// Cleared by the same event that resets the machine's own leaseWarned
		// flag: a status request that pushes the deadline back out past the warn
		// fraction. RequestIntervalMS is half the granted lease (§5.2 item 1), so
		// it is what the remaining lease is measured against. A lease that is no
		// longer active is not "near expiry" — it has either expired (the link
		// state says so on its own) or never been granted.
		if !link.LeaseActive {
			return true
		}
		leaseMS := link.RequestIntervalMS * 2
		return leaseMS > 0 && float64(link.LeaseExpiresInMS) > c.fraction*float64(leaseMS)
	case model.WarnCaptionAssetUnavailable, model.WarnCaptionParseDegraded:
		// Both describe the content currently loaded. A different show, or no
		// show at all, is a different question — the next load re-raises it if it
		// is still true.
		return !link.RPLLoaded || link.CurrentPlayoutID != r.playoutID
	case model.WarnServerIdentityMismatch:
		// The mismatch belongs to the announced connection. A torn-down link, or a
		// DIFFERENT server on the other end, means the announce that raised it is
		// gone; the next Announce re-evaluates the pinned identity (§8.5).
		//
		// An empty recorded server means the warning was raised DURING the announce
		// that established the connection — the machine evaluates the pinned
		// identity before it publishes the new state — so there is nothing to
		// compare against and the mismatch stands until the link goes down. Getting
		// this backwards would clear the wrong-room warning the instant it was
		// raised, which is the one moment it has to survive.
		return link.State == model.StateDisconnected ||
			(r.server != "" && link.Server != r.server)
	default:
		// Unknown caption codes survive until the link is torn down. A code this
		// package does not recognize gets the conservative treatment: keep
		// shouting rather than guess that the condition passed.
		return link.State == model.StateDisconnected
	}
}
