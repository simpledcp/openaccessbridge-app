// Package debugcapture implements the §9.4 debug-capture escape hatch: the ONE
// sanctioned way caption text or audio samples may be written down, and only
// under every guard the buildplan attaches to it.
//
//	"Debug caption logging and debug audio capture are each explicit, admin-only,
//	time-limited, size-limited (audio), and visibly indicated on the dashboard
//	while active, write only to a bounded tmpfs or an encrypted support bundle,
//	and auto-delete on a timer and on reboot." (§9.4)
//
// Each of those seven properties is enforced here:
//
//	explicit     — nothing captures until Start is called with a Grant; the zero
//	               state of a Manager captures nothing.
//	admin-only   — a Grant without a GrantedBy identity is refused, so no
//	               unauthenticated path can open a capture.
//	time-limited — every grant carries a TTL, clamped to Options.MaxTTL.
//	size-limited — every grant carries a byte budget, clamped to Options.MaxBytes;
//	               writes past it are refused loudly, never silently truncated.
//	indicated    — Warnings() raises model.WarnDebugCaptureActive for the whole
//	               window, which is what puts the banner on the dashboard (§8.3).
//	bounded sink — the capture directory must live on tmpfs (Options.RequireTmpfs),
//	               which is verified by introspecting the mount table.
//	auto-delete  — Sweep/Run delete an expired capture's files, Stop deletes on
//	               demand, and because the sink is tmpfs a reboot deletes them
//	               unconditionally.
//
// DELIBERATE DESIGN CHOICE: this package does NOT bypass the logging redaction
// guard, and no such bypass exists. The structured log stream stays content-free
// under every condition (guardrail #6); captured content goes only to this
// bounded, TTL'd, self-deleting sink. A redaction bypass token would have made
// the single chokepoint in logging/redact.go conditional, which is precisely the
// property that makes it trustworthy.
package debugcapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Kind is what a capture records. They are separate grants: an admin enabling
// caption logging does not thereby enable audio capture.
type Kind string

const (
	// KindCaption captures caption text for a "captions look wrong" report.
	KindCaption Kind = "caption"
	// KindAudio captures audio samples for an input/qualification report.
	KindAudio Kind = "audio"
)

// Defaults. They are deliberately small: a debug capture is a few minutes of
// evidence, not a recording facility.
const (
	DefaultMaxTTL         = 15 * time.Minute
	DefaultCaptionMaxByte = 1 << 20 // 1 MiB of text is a long show's worth of cues
	DefaultAudioMaxByte   = 8 << 20 // 8 MiB caps a short PCM/Opus excerpt
	// DefaultSweepInterval is how often Run checks for an expired capture.
	DefaultSweepInterval = 5 * time.Second
)

// Errors callers distinguish.
var (
	ErrNoGrant     = errors.New("debugcapture: no active capture for this kind")
	ErrNotAdmin    = errors.New("debugcapture: a capture requires an identified admin grant")
	ErrExpired     = errors.New("debugcapture: the capture window has expired")
	ErrBudgetSpent = errors.New("debugcapture: the capture byte budget is exhausted")
	ErrActive      = errors.New("debugcapture: a capture of this kind is already active")
	ErrNotTmpfs    = errors.New("debugcapture: the capture directory is not on tmpfs")
)

// Options configures a Manager.
type Options struct {
	// Dir is the capture sink. On the appliance this is the unit's tmpfs
	// RuntimeDirectory, so a reboot deletes anything left behind.
	Dir string
	// RequireTmpfs refuses to start a capture unless Dir really is on tmpfs
	// (introspected from ProcMounts). Production must leave this true.
	RequireTmpfs bool
	// ProcMounts is the mount table, injectable for tests. Default "/proc/mounts".
	ProcMounts string
	// MaxTTL and MaxBytes are the hard ceilings a grant is clamped to.
	MaxTTL   time.Duration
	MaxBytes int64
	// Now is the clock, injectable for tests. Default time.Now.
	Now func() time.Time
	// Log receives the audit trail. Content NEVER passes through it.
	Log *logging.Logger
}

// Manager owns the active captures. The zero state captures nothing.
type Manager struct {
	opts Options

	mu       sync.Mutex
	sessions map[Kind]*session
}

// session is one live grant plus its open sink.
type session struct {
	kind      Kind
	grantee   string // hashed before it reaches a log
	reason    string
	started   time.Time
	expires   time.Time
	budget    int64
	written   int64
	path      string
	file      *os.File
	capReport bool // the budget-exhausted event has already been emitted
}

// Status is a content-free snapshot of one capture, for the dashboard.
type Status struct {
	Kind         Kind      `json:"kind"`
	StartedAt    time.Time `json:"started_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	BytesWritten int64     `json:"bytes_written"`
	ByteBudget   int64     `json:"byte_budget"`
	Reason       string    `json:"reason"`
}

// Grant is an admin's explicit, bounded authorization to capture.
type Grant struct {
	Kind Kind
	// TTL is how long the capture may run; clamped to Options.MaxTTL. Zero takes
	// the ceiling.
	TTL time.Duration
	// MaxBytes is the byte budget; clamped to Options.MaxBytes. Zero takes the
	// per-kind default.
	MaxBytes int64
	// GrantedBy identifies the admin who authorized it. Required — it is the
	// "admin-only" half of §9.4 — and hashed before it is ever logged.
	GrantedBy string
	// Reason is a short free-text note ("patron reports captions one line late").
	// It is surfaced on the admin dashboard through Status, and it is NEVER
	// written to the log stream: free text typed about a caption fault can quote
	// the caption, and the redaction guard keys off field names, not values.
	Reason string
}

// New builds a Manager. It does not create the directory or start anything.
func New(opts Options) *Manager {
	if opts.MaxTTL <= 0 {
		opts.MaxTTL = DefaultMaxTTL
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultAudioMaxByte
	}
	if opts.ProcMounts == "" {
		opts.ProcMounts = DefaultProcMounts
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = logging.Nop()
	}
	return &Manager{opts: opts, sessions: map[Kind]*session{}}
}

// Start opens a capture under an explicit admin grant. It refuses an
// unidentified grant, a second capture of the same kind, and a sink that is not
// on tmpfs.
func (m *Manager) Start(g Grant) (Status, error) {
	if strings.TrimSpace(g.GrantedBy) == "" {
		return Status{}, ErrNotAdmin
	}
	if g.Kind != KindCaption && g.Kind != KindAudio {
		return Status{}, fmt.Errorf("debugcapture: unknown capture kind %q", g.Kind)
	}
	if m.opts.Dir == "" {
		return Status{}, errors.New("debugcapture: no capture directory configured")
	}
	if m.opts.RequireTmpfs {
		ok, err := IsTmpfs(m.opts.Dir, m.opts.ProcMounts)
		if err != nil {
			return Status{}, fmt.Errorf("%w: %v", ErrNotTmpfs, err)
		}
		if !ok {
			return Status{}, ErrNotTmpfs
		}
	}

	now := m.opts.Now()
	ttl := g.TTL
	if ttl <= 0 || ttl > m.opts.MaxTTL {
		ttl = m.opts.MaxTTL
	}
	budget := g.MaxBytes
	if budget <= 0 {
		budget = defaultBudget(g.Kind)
	}
	if budget > m.opts.MaxBytes {
		budget = m.opts.MaxBytes
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// An already-expired session of this kind is swept first, so a stale grant
	// does not block a fresh one.
	m.sweepLocked(now)
	if _, ok := m.sessions[g.Kind]; ok {
		return Status{}, ErrActive
	}
	if err := os.MkdirAll(m.opts.Dir, 0o700); err != nil {
		return Status{}, fmt.Errorf("debugcapture: capture directory: %w", err)
	}
	path := filepath.Join(m.opts.Dir, fmt.Sprintf("%s-%d.capture", g.Kind, now.UnixNano()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Status{}, fmt.Errorf("debugcapture: capture sink: %w", err)
	}
	s := &session{
		kind: g.Kind, grantee: logging.HashID("admin", g.GrantedBy), reason: g.Reason,
		started: now, expires: now.Add(ttl), budget: budget, path: path, file: f,
	}
	m.sessions[g.Kind] = s

	// Audit trail: who (hashed), what kind, and the bounds — never the content.
	//
	// The Reason is deliberately NOT logged. It is unbounded operator free text
	// typed while looking at a caption problem ("the cue read '...' and was one
	// line late"), so it is a content vector the redaction chokepoint cannot
	// catch: redact.go keys off field NAMES, and "reason" names nothing denied.
	// Only its presence and length go into the trail; the text itself stays in
	// the in-memory Status the admin dashboard renders (guardrail #6).
	m.opts.Log.Warn(logging.EvDebugCaptureStarted,
		logging.Field("kind", string(g.Kind)),
		logging.Field("granted_by_id", s.grantee),
		logging.Field("ttl_ms", ttl.Milliseconds()),
		logging.Field("byte_budget", budget),
		logging.Field("reason_given", strings.TrimSpace(g.Reason) != ""),
		logging.Field("reason_len", len(g.Reason)))
	return s.status(), nil
}

// Write appends to the active capture of that kind. It enforces the window and
// the budget on every call: a capture that has expired or run out of budget
// refuses loudly rather than silently dropping bytes (guardrail #7).
func (m *Manager) Write(kind Kind, p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.opts.Now()
	s, ok := m.sessions[kind]
	if !ok {
		m.sweepLocked(now)
		return 0, ErrNoGrant
	}
	// An expired window is reported as ErrExpired, not as "no grant": the caller
	// asked for a capture that WAS authorized and has run out of time, and a
	// truthful error is what stops it re-opening one silently.
	if !now.Before(s.expires) {
		m.stopLocked(kind, "expired")
		return 0, ErrExpired
	}
	if s.written+int64(len(p)) > s.budget {
		if !s.capReport {
			s.capReport = true
			m.opts.Log.Warn(logging.EvDebugCaptureCapped,
				logging.Field("kind", string(kind)),
				logging.Field("byte_budget", s.budget),
				logging.Field("bytes_written", s.written))
		}
		return 0, ErrBudgetSpent
	}
	n, err := s.file.Write(p)
	s.written += int64(n)
	if err != nil {
		return n, fmt.Errorf("debugcapture: write: %w", err)
	}
	return n, nil
}

// Stop ends a capture and DELETES its file immediately.
func (m *Manager) Stop(kind Kind) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[kind]; !ok {
		return ErrNoGrant
	}
	m.stopLocked(kind, "stopped")
	return nil
}

// Sweep deletes any capture whose window has closed. It is idempotent and cheap;
// Run calls it on a ticker, and Start/Write call it inline so an expired capture
// cannot outlive its grant even if nothing is ticking.
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.opts.Now())
}

// Run sweeps on a ticker until ctx is done, then deletes everything still open.
// Losing the process is also safe: the sink is tmpfs, so a reboot deletes it.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			m.Sweep()
		}
	}
}

// Close ends and deletes every capture.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for kind := range m.sessions {
		m.stopLocked(kind, "shutdown")
	}
}

// Active reports whether any capture is running (after sweeping expiries).
func (m *Manager) Active() bool { return len(m.Sessions()) > 0 }

// Sessions returns a content-free snapshot of the live captures.
func (m *Manager) Sessions() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.opts.Now())
	out := make([]Status, 0, len(m.sessions))
	for _, kind := range []Kind{KindCaption, KindAudio} {
		if s, ok := m.sessions[kind]; ok {
			out = append(out, s.status())
		}
	}
	return out
}

// Warnings raises the §8.3 dashboard indicator for the whole capture window —
// the "visibly indicated on the dashboard while active" half of §9.4. It is a
// warning, not a critical: the capture is a deliberate, authorized action, but it
// must never be invisible.
func (m *Manager) Warnings() []model.Warning {
	var out []model.Warning
	for _, s := range m.Sessions() {
		out = append(out, model.Warning{
			Code:     model.WarnDebugCaptureActive,
			Severity: model.SeverityWarning,
			Message: fmt.Sprintf("Debug %s capture is ACTIVE until %s (bounded to %d bytes, auto-deletes on expiry and on reboot)",
				s.Kind, s.ExpiresAt.UTC().Format(time.RFC3339), s.ByteBudget),
		})
	}
	return out
}

// sweepLocked closes and deletes expired sessions. Caller holds m.mu.
func (m *Manager) sweepLocked(now time.Time) {
	for kind, s := range m.sessions {
		if !now.Before(s.expires) {
			m.stopLocked(kind, "expired")
		}
	}
}

// stopLocked closes the sink, deletes the file, and audits the end of the
// window. Caller holds m.mu.
func (m *Manager) stopLocked(kind Kind, why string) {
	s, ok := m.sessions[kind]
	if !ok {
		return
	}
	delete(m.sessions, kind)
	if s.file != nil {
		_ = s.file.Close()
	}
	// Auto-delete: the whole point of the bounded sink. A failure to remove is
	// loud, because a leftover capture file is exactly what §9.4 forbids.
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		m.opts.Log.Error(logging.EvDebugCaptureStopped,
			logging.Field("kind", string(kind)),
			logging.Field("reason", why),
			logging.Field("delete_failed", true),
			logging.Field("error", err.Error()))
		return
	}
	m.opts.Log.Warn(logging.EvDebugCaptureStopped,
		logging.Field("kind", string(kind)),
		logging.Field("reason", why),
		logging.Field("granted_by_id", s.grantee),
		logging.Field("bytes_written", s.written),
		logging.Field("deleted", true))
}

func (s *session) status() Status {
	return Status{
		Kind: s.kind, StartedAt: s.started, ExpiresAt: s.expires,
		BytesWritten: s.written, ByteBudget: s.budget, Reason: s.reason,
	}
}

func defaultBudget(k Kind) int64 {
	if k == KindCaption {
		return DefaultCaptionMaxByte
	}
	return DefaultAudioMaxByte
}
