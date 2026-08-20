package readiness

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// leased builds a link state with a granted lease of leaseMS and remainingMS
// left on it. RequestIntervalMS is half the lease, exactly as the machine
// reports it (§5.2 item 1).
func leased(leaseMS, remainingMS int64) model.CaptionLinkState {
	return model.CaptionLinkState{
		State: model.StatePlaying, Server: "server-a",
		LeaseActive: true, LeaseExpiresInMS: remainingMS, RequestIntervalMS: leaseMS / 2,
		RPLLoaded: true, CurrentPlayoutID: "playout-1",
	}
}

// TestLeaseNearExpiryClearsOnRenewal: the near-expiry warning is a CONDITION,
// not an event log. A status request that pushes the deadline back out clears it
// — the same event that resets the machine's own leaseWarned flag.
func TestLeaseNearExpiryClearsOnRenewal(t *testing.T) {
	c := NewCaptionWarnings(0.25)
	c.Observe(leased(30000, 4000)) // 13% of the lease left → the machine warns
	c.Raise(model.Warning{Code: model.WarnCaptionLeaseNearExpiry, Severity: model.SeverityWarning, Message: "close"})

	if got := c.Snapshot(); !hasCode(got, model.WarnCaptionLeaseNearExpiry) {
		t.Fatalf("a near-expiry lease must still be reported, got %v", codes(got))
	}
	c.Observe(leased(30000, 29000)) // renewed
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("a renewed lease must clear the warning, got %v", codes(got))
	}
}

// TestLeaseWarningClearedWhenLeaseGone: "near expiry" is meaningless once there
// is no lease; the link state reports LEASE_EXPIRED on its own.
func TestLeaseWarningClearedWhenLeaseGone(t *testing.T) {
	c := NewCaptionWarnings(0.25)
	c.Observe(leased(30000, 2000))
	c.Raise(model.Warning{Code: model.WarnCaptionLeaseNearExpiry, Severity: model.SeverityWarning})
	c.Observe(model.CaptionLinkState{State: model.StateLeaseExpired, Server: "server-a"})
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("expected no warning once the lease is gone, got %v", codes(got))
	}
}

// TestAssetWarningsFollowTheShow: an "assets did not all serve" condition belongs
// to the content loaded when it was raised. A new show re-asks the question.
func TestAssetWarningsFollowTheShow(t *testing.T) {
	for _, code := range []string{model.WarnCaptionAssetUnavailable, model.WarnCaptionParseDegraded} {
		c := NewCaptionWarnings(0)
		c.Observe(leased(30000, 20000))
		c.Raise(model.Warning{Code: code, Severity: model.SeverityWarning})
		if got := c.Snapshot(); !hasCode(got, code) {
			t.Fatalf("%s: expected the warning to stand for the loaded show, got %v", code, codes(got))
		}
		next := leased(30000, 20000)
		next.CurrentPlayoutID = "playout-2"
		c.Observe(next)
		if got := c.Snapshot(); len(got) != 0 {
			t.Fatalf("%s: a new show must clear it, got %v", code, codes(got))
		}
	}
}

// TestIdentityMismatchHoldsUntilTheLinkIsTornDown: the wrong-room warning (§8.5)
// must not evaporate while the appliance is still talking to the wrong server.
func TestIdentityMismatchHoldsUntilTheLinkIsTornDown(t *testing.T) {
	c := NewCaptionWarnings(0)
	c.Observe(leased(30000, 20000))
	c.Raise(model.Warning{Code: model.WarnServerIdentityMismatch, Severity: model.SeverityCritical})

	// Time passes, the lease renews, the show changes — none of that makes it the
	// right room.
	next := leased(30000, 29000)
	next.CurrentPlayoutID = "playout-9"
	c.Observe(next)
	if got := c.Snapshot(); !hasCode(got, model.WarnServerIdentityMismatch) {
		t.Fatalf("the wrong-room warning must persist while connected, got %v", codes(got))
	}
	c.Observe(model.CaptionLinkState{State: model.StateDisconnected})
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("a torn-down link clears it (the next Announce re-evaluates), got %v", codes(got))
	}
}

// TestUnknownCaptionCodeIsKept: a code this package does not recognize is kept
// while the link is up rather than guessed to be over (guardrail #7).
func TestUnknownCaptionCodeIsKept(t *testing.T) {
	c := NewCaptionWarnings(0)
	c.Observe(leased(30000, 20000))
	c.Raise(model.Warning{Code: "some_future_caption_condition", Severity: model.SeverityWarning})
	if got := c.Snapshot(); len(got) != 1 {
		t.Fatalf("expected the unknown code to be kept, got %v", codes(got))
	}
}

// TestRaiseIsIdempotentPerCode: the dashboard shows conditions, not an event
// stream, so re-raising a code must not stack duplicates.
func TestRaiseIsIdempotentPerCode(t *testing.T) {
	c := NewCaptionWarnings(0)
	c.Observe(leased(30000, 2000))
	for i := 0; i < 5; i++ {
		c.Raise(model.Warning{Code: model.WarnCaptionLeaseNearExpiry, Severity: model.SeverityWarning, Message: "close"})
	}
	if got := c.Snapshot(); len(got) != 1 {
		t.Fatalf("expected one warning per code, got %v", codes(got))
	}
}

// TestNilCaptionWarningsIsUsable: the set is optional wiring; a nil one must not
// panic on the dashboard path.
func TestNilCaptionWarningsIsUsable(t *testing.T) {
	var c *CaptionWarnings
	c.Observe(leased(30000, 1000))
	c.Raise(model.Warning{Code: model.WarnCaptionLeaseNearExpiry})
	if got := c.Snapshot(); got != nil {
		t.Fatalf("expected nil snapshot, got %v", codes(got))
	}
}
