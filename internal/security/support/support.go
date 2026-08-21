// Package support implements the §9.4 support session: the only way back into
// an appliance whose SSH is disabled, and therefore the one feature in this
// product whose failure mode is a permanent way in.
//
// WHAT OPENING A SESSION DOES, EXACTLY. It opens a WINDOW — one outbound
// destination in the firewall's output chain, for a bounded time, recorded in an
// audit trail. It does not dial anything, because this appliance has no tunnel
// client and no support endpoint of its own. "A support session is open" must
// therefore never be allowed to read as "a support engineer is connected": the
// first is a posture this box controls and can report honestly, the second is an
// event it cannot observe at all. Every surface that shows a session says which
// of the two it means, and Report carries the distinction as a field rather than
// as prose that could be dropped.
//
// THE FOUR PROPERTIES §9.4 ASKS FOR, and where each one lives:
//
//	off by default  — nothing here runs until Open is called, and Open refuses
//	                  without a configured endpoint (an unconfigured appliance
//	                  cannot be talked into opening a window to nowhere).
//	outbound-only   — internal/security/firewall emits the rule in the OUTPUT
//	                  chain only, scoped to one address and one port.
//	time-limited    — an ABSOLUTE deadline, clamped to model.MaxSupportSession,
//	                  enforced by a timer AND re-checked on every read, and NOT
//	                  survivable across a restart.
//	audited         — every transition is written to the trail and logged as an
//	                  event; the operator's reason is stored locally and never
//	                  logged, because it is free text (§9.4).
package support

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
)

// Errors an operator surface turns into a message.
var (
	// ErrNoEndpoint: no support endpoint is configured, so there is nothing to
	// permit. Refusing is the whole point — the alternative is a window with no
	// destination, which can only be rendered as an unrestricted egress hole or
	// as nothing at all.
	ErrNoEndpoint = errors.New("support: no support endpoint is configured (host, port and egress interface are all required)")
	// ErrAlreadyOpen: one session at a time. Extending is deliberately close +
	// open, so the trail shows two windows with two reasons rather than one that
	// silently grew.
	ErrAlreadyOpen = errors.New("support: a session is already open; close it before opening another")
	// ErrNotOpen: nothing to close.
	ErrNotOpen = errors.New("support: no session is open")
	// ErrNoReason: §9.4 audits sessions, and an audit entry with no reason is a
	// timestamp. The reason is required.
	ErrNoReason = errors.New("support: a reason is required so the trail records why the appliance was opened")
)

// maxReasonLen bounds the operator's note. It is stored, so it is bounded.
const maxReasonLen = 200

// Store is the audit trail this package writes to (internal/config).
type Store interface {
	RecordSupportOpen(model.SupportSession) error
	RecordSupportClose(id string, state model.SupportSessionState, at time.Time) error
	OpenSupportSessions() ([]model.SupportSession, error)
}

// Endpoint is the configured destination a session may reach.
type Endpoint struct {
	Iface string
	Host  string
	Port  int
}

// Configured reports whether all three parts are present. A partial endpoint is
// treated as none: two thirds of an address is not an address.
func (e Endpoint) Configured() bool {
	return strings.TrimSpace(e.Iface) != "" && strings.TrimSpace(e.Host) != "" && e.Port > 0
}

func (e Endpoint) String() string { return fmt.Sprintf("%s:%s:%d", e.Iface, e.Host, e.Port) }

// Options configures a Manager.
type Options struct {
	Store Store
	Log   *logging.Logger
	// Endpoint resolves the configured destination on each call, so an operator
	// correcting it in the setup wizard does not need a restart.
	Endpoint func() Endpoint
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// OnChange is signalled when the window opens or closes, so the supervisor
	// re-applies the firewall AT THAT MOMENT rather than at its next scheduled
	// reconciliation. Without it an expired window would keep permitting egress
	// until the next posture pass — a small number of seconds, but the wrong
	// number: the deadline is the promise.
	OnChange func()
}

// Manager owns the single support session.
type Manager struct {
	store    Store
	log      *logging.Logger
	endpoint func() Endpoint
	now      func() time.Time
	onChange func()

	mu      sync.Mutex
	current *model.SupportSession
	timer   *time.Timer
}

// New builds a Manager and CLOSES anything the trail still shows as open.
//
// Support mode fails closed across a restart. A window that survived a boot
// would outlive the person who opened it, the reason they gave, and quite
// possibly the call — and it would come back without anyone deciding to reopen
// it. The trail records those as ended_by_restart rather than losing them.
func New(opts Options) *Manager {
	m := &Manager{
		store: opts.Store, log: opts.Log,
		endpoint: opts.Endpoint, now: opts.Now, onChange: opts.OnChange,
	}
	if m.log == nil {
		m.log = logging.Nop()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.endpoint == nil {
		m.endpoint = func() Endpoint { return Endpoint{} }
	}
	if m.store != nil {
		if open, err := m.store.OpenSupportSessions(); err == nil {
			for _, sess := range open {
				_ = m.store.RecordSupportClose(sess.ID, model.SupportEndedByRestart, m.now())
				m.log.Warn(logging.EvSupportSessionClosed,
					logging.Field("phase", "ended_by_restart"),
					logging.Field("session_id", sess.ID))
			}
		}
	}
	return m
}

// OpenRequest is what an operator asks for.
type OpenRequest struct {
	// Reason is required and is stored, never logged.
	Reason string
	// AdminUser is the authenticated operator; only its HASH is kept.
	AdminUser string
	// Duration is clamped to model.MaxSupportSession; zero uses the default.
	Duration time.Duration
}

// Open starts a session.
func (m *Manager) Open(req OpenRequest) (model.SupportSession, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return model.SupportSession{}, ErrNoReason
	}
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	ep := m.endpoint()
	if !ep.Configured() {
		return model.SupportSession{}, ErrNoEndpoint
	}

	dur := req.Duration
	if dur <= 0 {
		dur = model.DefaultSupportSession
	}
	if dur > model.MaxSupportSession {
		dur = model.MaxSupportSession
	}

	m.mu.Lock()
	now := m.now()
	if m.current != nil && m.current.Active(now) {
		m.mu.Unlock()
		return model.SupportSession{}, ErrAlreadyOpen
	}
	// A session past its deadline that the timer has not serviced yet is FINISHED
	// here rather than dropped. Overwriting m.current would leave the trail with a
	// row that says "open" for a window that closed — and OpenSupportSessions,
	// which startup uses to fail closed, would find it on every subsequent boot.
	m.expireIfDueLocked()
	sess := model.SupportSession{
		ID:        newID(now),
		State:     model.SupportOpen,
		OpenedAt:  now.UTC(),
		ExpiresAt: now.Add(dur).UTC(),
		OpenedBy:  logging.HashID("admin", req.AdminUser),
		Reason:    reason,
		Endpoint:  ep.String(),
	}
	if m.store != nil {
		if err := m.store.RecordSupportOpen(sess); err != nil {
			m.mu.Unlock()
			// The window is NOT opened if the trail cannot record it. An audited
			// feature whose audit failed is an unaudited feature, and §9.4 asks
			// for an audited one.
			return model.SupportSession{}, fmt.Errorf("support: refusing to open an unrecorded session: %w", err)
		}
	}
	m.current = &sess
	m.armLocked(dur)
	m.mu.Unlock()

	// §9.4 audit event. The reason is NOT logged — it is operator free text and
	// could contain anything — but its presence and length are, so a trail with a
	// suspiciously empty reason is visible without the text itself travelling.
	m.log.Warn(logging.EvSupportSessionOpened,
		logging.Field("session_id", sess.ID),
		logging.Field("opened_by", sess.OpenedBy),
		logging.Field("endpoint", sess.Endpoint),
		logging.Field("duration_ms", dur.Milliseconds()),
		logging.Field("reason_len", len(reason)))
	m.notify()
	return sess, nil
}

// Close ends the current session.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.current == nil {
		m.mu.Unlock()
		return ErrNotOpen
	}
	sess := *m.current
	now := m.now()
	state := model.SupportClosed
	if !sess.Active(now) {
		state = model.SupportExpired
	}
	m.finishLocked(state, now)
	m.mu.Unlock()
	m.notify()
	return nil
}

// Current returns the live session, expiring it first if its deadline has passed.
//
// Expiry is enforced here as well as by the timer because a reader must never
// see a window the deadline has already closed. The timer makes the firewall
// change happen promptly; this makes every ANSWER correct even if the timer has
// not run yet.
func (m *Manager) Current() (model.SupportSession, bool) {
	m.mu.Lock()
	expired := m.expireIfDueLocked()
	// A session whose endpoint has been reconfigured under it is ENDED, not
	// quietly retargeted. The window an operator authorised was to one address;
	// moving it is a new decision, and it should be made deliberately and get its
	// own audit row.
	if !expired && m.current != nil {
		if ep := m.endpoint(); ep.String() != m.current.Endpoint {
			m.log.Warn(logging.EvSupportSessionClosed,
				logging.Field("phase", "endpoint_changed_under_session"))
			m.finishLocked(model.SupportClosed, m.now())
			expired = true
		}
	}
	var sess model.SupportSession
	ok := false
	if m.current != nil {
		sess, ok = *m.current, true
	}
	m.mu.Unlock()
	if expired {
		m.notify()
	}
	return sess, ok
}

// Egress is the firewall window this session justifies, or nil when none is
// open. It is what the supervisor puts into the ruleset.
func (m *Manager) Egress() *firewall.SupportEgress {
	sess, ok := m.Current()
	if !ok || !sess.Active(m.now()) {
		return nil
	}
	ep := m.endpoint()
	// The window permits the endpoint the SESSION was opened for, not whatever is
	// configured now. Reading the live setting here would silently move the hole
	// to a new address while the audit trail, the log line and the dashboard all
	// went on naming the old one — an operator reviewing the trail afterwards
	// would be reading about a destination the appliance never used.
	//
	// So a changed endpoint CLOSES the session instead (see Current), and this
	// only ever emits the recorded one.
	if !ep.Configured() || ep.String() != sess.Endpoint {
		return nil
	}
	return &firewall.SupportEgress{Iface: ep.Iface, Host: ep.Host, Port: ep.Port}
}

// Report is the operator-facing state.
type Report struct {
	// Active: a window is open right now.
	Active bool `json:"active"`
	// EndpointConfigured: a session COULD be opened. An appliance with no
	// endpoint cannot be re-entered at all once SSH is off, which is a fact worth
	// knowing before it is needed rather than during an outage.
	EndpointConfigured bool `json:"endpoint_configured"`
	// Session is the live one, when there is one.
	Session *model.SupportSession `json:"session,omitempty"`
	// RemainingSeconds is what is left of the window.
	RemainingSeconds int `json:"remaining_seconds"`
	// TunnelEstablished is ALWAYS false in this build, and it is a field rather
	// than an omission so that nothing can quietly start reading "a window is
	// open" as "someone is connected". This appliance opens the window; it has no
	// tunnel client and dials nothing.
	TunnelEstablished bool `json:"tunnel_established"`
	// Note states that distinction in words for a surface that renders it.
	Note string `json:"note"`
}

// Status reports the current state.
func (m *Manager) Status() Report {
	ep := m.endpoint()
	rep := Report{EndpointConfigured: ep.Configured()}
	sess, ok := m.Current()
	if ok && sess.Active(m.now()) {
		s := sess
		rep.Active = true
		rep.Session = &s
		rep.RemainingSeconds = int(sess.RemainingAt(m.now()).Seconds())
		rep.Note = "An outbound window to " + sess.Endpoint + " is open until " +
			sess.ExpiresAt.Format(time.RFC3339) + ". This appliance has not connected to anything: " +
			"it permits the connection, it does not make one."
	} else if !rep.EndpointConfigured {
		rep.Note = "No support endpoint is configured, so no support session can be opened. " +
			"If SSH is disabled, there is currently no way back into this appliance."
	} else {
		rep.Note = "Closed. Support mode is off by default and opens only when an operator opens it."
	}
	return rep
}

// expireIfDueLocked closes an over-deadline session. Caller holds mu.
func (m *Manager) expireIfDueLocked() bool {
	if m.current == nil {
		return false
	}
	now := m.now()
	if m.current.Active(now) {
		return false
	}
	m.finishLocked(model.SupportExpired, now)
	return true
}

// finishLocked records the ending and clears the live session. Caller holds mu.
func (m *Manager) finishLocked(state model.SupportSessionState, at time.Time) {
	sess := *m.current
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.store != nil {
		_ = m.store.RecordSupportClose(sess.ID, state, at)
	}
	m.current = nil
	m.log.Warn(logging.EvSupportSessionClosed,
		logging.Field("session_id", sess.ID),
		logging.Field("outcome", string(state)),
		logging.Field("open_ms", at.Sub(sess.OpenedAt).Milliseconds()))
}

// armLocked schedules the expiry. Caller holds mu.
func (m *Manager) armLocked(d time.Duration) {
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(d, func() {
		m.mu.Lock()
		expired := m.expireIfDueLocked()
		m.mu.Unlock()
		if expired {
			m.notify()
		}
	})
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

// newID is a sortable, non-secret identifier for the trail.
func newID(now time.Time) string {
	return "sup-" + now.UTC().Format("20060102T150405.000000000")
}
