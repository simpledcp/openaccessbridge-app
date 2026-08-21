package model

import "time"

// The §9.4 support session: "Off by default. When enabled by an admin it is
// outbound-only, time-limited, and audited, and visibly indicated. It is the
// only re-entry path once SSH is disabled."
//
// WHAT A SESSION IS, AND WHAT IT IS NOT. Opening one opens a WINDOW: a single
// outbound destination in the firewall's output chain, for a bounded time,
// recorded. It does not dial anything. This appliance has no tunnel client and
// no support endpoint of its own, so "a support session is open" must never be
// allowed to read as "a support engineer is connected" — the first is a posture
// this box controls, the second is an event it cannot observe. Every surface
// that shows a session says which one it means.
//
// The window is the dangerous half, which is why it is the half that is
// modelled: an appliance that has disabled SSH and then leaves an egress hole
// open indefinitely has traded one permanent way in for another.

// SupportSessionState is the lifecycle of one session.
type SupportSessionState string

const (
	// SupportOpen: the window is open and the appliance is inside it.
	SupportOpen SupportSessionState = "open"
	// SupportClosed: an operator closed it deliberately.
	SupportClosed SupportSessionState = "closed"
	// SupportExpired: it reached its deadline and the appliance closed it. This
	// is the expected ending — a session that has to be closed by hand is one
	// that could be forgotten.
	SupportExpired SupportSessionState = "expired"
	// SupportEndedByRestart: the appliance restarted while it was open, and it
	// was NOT resumed. Support mode fails closed across a boot: a window that
	// survived a restart would outlive the person who opened it and the reason
	// they gave.
	SupportEndedByRestart SupportSessionState = "ended_by_restart"
)

// MaxSupportSession is the longest window that can be opened, whatever is
// requested. §9.4 says "time-limited"; a limit an operator can raise to a week
// is not one. Four hours covers a support call comfortably and expires before
// the next day's staff arrive.
const MaxSupportSession = 4 * time.Hour

// DefaultSupportSession is the window granted when no duration is asked for.
const DefaultSupportSession = time.Hour

// SupportSession is one audited window.
type SupportSession struct {
	ID    string              `json:"id"`
	State SupportSessionState `json:"state"`
	// OpenedAt / ExpiresAt bound the window. ExpiresAt is ABSOLUTE, so a clock
	// read later cannot be talked into extending it.
	OpenedAt  time.Time  `json:"opened_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	// OpenedBy is a HASH of the admin username that opened it (§9.4 keeps
	// identifiers out of logs; the audit trail keeps a stable reference without
	// storing the name).
	OpenedBy string `json:"opened_by"`
	// Reason is the operator's own note — a ticket number, a caller. It is kept
	// in the venue's local audit trail, and it is NEVER logged: it is free text
	// and could contain anything (§9.4).
	Reason string `json:"reason"`
	// Endpoint is the single destination the window permits, as
	// "iface:host:port", so the audit record says exactly what was allowed rather
	// than that "support was on".
	Endpoint string `json:"endpoint"`
}

// Active reports whether this session's window is open at t. It is computed from
// the absolute deadline rather than from State alone: a session whose timer has
// not yet been serviced is already over, and every reader must agree about that.
func (s SupportSession) Active(t time.Time) bool {
	return s.State == SupportOpen && t.Before(s.ExpiresAt)
}

// RemainingAt is how much of the window is left, never negative.
func (s SupportSession) RemainingAt(t time.Time) time.Duration {
	if !s.Active(t) {
		return 0
	}
	return s.ExpiresAt.Sub(t)
}
