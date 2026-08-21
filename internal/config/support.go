package config

// Persistence for the §9.4 support-session audit trail.
//
// This is the one place the appliance stores something that is neither
// configuration nor a profile, and it is worth saying why that is consistent
// with guardrail #5 rather than an exception to it. The guardrail forbids
// persisting CONTENT — caption text, audio. An audit trail of who opened a way
// into the appliance, when, for how long, and to what address is the opposite:
// it is the record that makes the feature accountable, and a support mode with
// no durable trail is one whose use cannot be reviewed after the fact.
//
// It holds no content, no patron identifiers and no credentials.

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/simpledcp/captionbridge/internal/model"
)

// Settings that configure where a support session may reach. All three are
// required before one can be opened; there is no default, because a default
// endpoint is an address the venue never chose.
const (
	SettingSupportEndpointHost = "support_endpoint_host"
	SettingSupportEndpointPort = "support_endpoint_port"
	SettingSupportEgressIface  = "support_egress_iface"
)

// RecordSupportOpen writes a newly opened session.
func (s *SQLiteStore) RecordSupportOpen(sess model.SupportSession) error {
	var closed sql.NullString
	_, err := s.db.Exec(`
INSERT INTO support_sessions (id, state, opened_at, expires_at, closed_at, opened_by, reason, endpoint)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, string(sess.State), sess.OpenedAt.UTC().Format(time.RFC3339Nano),
		sess.ExpiresAt.UTC().Format(time.RFC3339Nano), closed, sess.OpenedBy, sess.Reason, sess.Endpoint)
	if err != nil {
		return fmt.Errorf("config: record support session: %w", err)
	}
	return nil
}

// RecordSupportClose closes a session in the trail. The state says HOW it ended
// — closed by an operator, expired on its own, or ended by a restart — because
// "the window was open and then it was not" is the least useful thing an audit
// trail can say.
func (s *SQLiteStore) RecordSupportClose(id string, state model.SupportSessionState, at time.Time) error {
	res, err := s.db.Exec(`
UPDATE support_sessions SET state = ?, closed_at = ? WHERE id = ? AND closed_at IS NULL`,
		string(state), at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("config: close support session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("config: support session %q is not open", id)
	}
	return nil
}

// OpenSupportSessions returns sessions the trail still shows as open. It exists
// for one caller: startup, which must close them. Support mode fails closed
// across a restart (model.SupportEndedByRestart).
func (s *SQLiteStore) OpenSupportSessions() ([]model.SupportSession, error) {
	rows, err := s.db.Query(`
SELECT id, state, opened_at, expires_at, opened_by, reason, endpoint
FROM support_sessions WHERE closed_at IS NULL ORDER BY opened_at`)
	if err != nil {
		return nil, fmt.Errorf("config: read open support sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSupport(rows)
}

// RecentSupportSessions returns the newest sessions for the operator surface.
func (s *SQLiteStore) RecentSupportSessions(limit int) ([]model.SupportSession, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT id, state, opened_at, expires_at, opened_by, reason, endpoint
FROM support_sessions ORDER BY opened_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("config: read support sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSupport(rows)
}

func scanSupport(rows *sql.Rows) ([]model.SupportSession, error) {
	var out []model.SupportSession
	for rows.Next() {
		var sess model.SupportSession
		var state, opened, expires string
		if err := rows.Scan(&sess.ID, &state, &opened, &expires, &sess.OpenedBy, &sess.Reason, &sess.Endpoint); err != nil {
			return nil, err
		}
		sess.State = model.SupportSessionState(state)
		sess.OpenedAt, _ = time.Parse(time.RFC3339Nano, opened)
		sess.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		out = append(out, sess)
	}
	return out, rows.Err()
}
