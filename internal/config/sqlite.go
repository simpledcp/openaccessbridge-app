package config

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/simpledcp/captionbridge/internal/model"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS auditorium_profiles (
    auditorium_id TEXT PRIMARY KEY,
    data          TEXT NOT NULL   -- JSON of model.AuditoriumProfile (config only)
);
CREATE TABLE IF NOT EXISTS system_config (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    data TEXT NOT NULL           -- JSON of model.SystemConfigVersion
);
CREATE TABLE IF NOT EXISTS admin_credentials (
    username    TEXT PRIMARY KEY,
    pwhash      TEXT NOT NULL,
    must_change INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// SQLiteStore is the crash-safe single-file configuration store. It is the only
// component permitted to persist state, and it persists configuration only.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the config database at path. Pass
// ":memory:" for tests. WAL mode and busy-timeout are set for crash-safety and
// concurrent readers.
func OpenSQLite(path string) (*SQLiteStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // single writer; avoids SQLITE_BUSY on an appliance
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &SQLiteStore{db: db}
	if err := s.migrateSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// schemaVersionKey is the schema_meta row §10.8's config_schema_version lives in.
const schemaVersionKey = "config_schema_version"

// SchemaVersion returns the config schema version recorded in this database.
func (s *SQLiteStore) SchemaVersion() (int, error) {
	v, _, err := s.storedSchemaVersion()
	return v, err
}

// migrateSchema reconciles the database's recorded schema version with the one
// this build understands (§10.8 config_schema_version).
//
// Stamping the current version unconditionally — what an INSERT … DO NOTHING
// does — means bumping model.CurrentConfigSchemaVersion silently opens an older
// database and reads it as if it were new. Three cases, all explicit:
//
//   - no row: a fresh database; stamp it.
//   - older: run the migration steps for the versions in between, then stamp.
//     There are none yet (v1 is the first schema), so this is a stamp with a
//     home for the steps to live when a v2 lands.
//   - newer: a later build wrote this database. Refuse to open it rather than
//     misinterpret configuration written by a schema we do not know (D17 /
//     guardrail #7 — an appliance must not degrade silently, least of all into
//     the wrong firewall posture).
func (s *SQLiteStore) migrateSchema() error {
	stored, ok, err := s.storedSchemaVersion()
	if err != nil {
		return err
	}
	switch {
	case !ok:
		return s.stampSchemaVersion(model.CurrentConfigSchemaVersion)
	case stored > model.CurrentConfigSchemaVersion:
		return fmt.Errorf(
			"config database schema version %d is newer than this build understands (%d): refusing to open it (§10.8)",
			stored, model.CurrentConfigSchemaVersion)
	case stored < model.CurrentConfigSchemaVersion:
		// Migration steps for stored+1 … CurrentConfigSchemaVersion go here, each
		// in its own transaction, before the stamp.
		return s.stampSchemaVersion(model.CurrentConfigSchemaVersion)
	default:
		return nil
	}
}

// storedSchemaVersion reads the recorded version. The second result is false when
// the database has never been stamped (a brand-new file).
func (s *SQLiteStore) storedSchemaVersion() (int, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM schema_meta WHERE key=?`, schemaVersionKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, convErr := strconv.Atoi(strings.TrimSpace(raw))
	if convErr != nil {
		// An unreadable version is not "assume it is ours": that is exactly the
		// silent misinterpretation this function exists to prevent.
		return 0, false, fmt.Errorf("config database schema version %q is not a number (§10.8)", raw)
	}
	return v, true, nil
}

func (s *SQLiteStore) stampSchemaVersion(v int) error {
	_, err := s.db.Exec(
		`INSERT INTO schema_meta(key,value) VALUES(?,?)
         ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		schemaVersionKey, fmt.Sprint(v))
	return err
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// --- Auditorium profiles ---

func (s *SQLiteStore) PutAuditoriumProfile(p model.AuditoriumProfile) error {
	if p.AuditoriumID == "" {
		return errors.New("auditorium_id required")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO auditorium_profiles(auditorium_id,data) VALUES(?,?)
         ON CONFLICT(auditorium_id) DO UPDATE SET data=excluded.data`,
		p.AuditoriumID, string(b))
	return err
}

func (s *SQLiteStore) GetAuditoriumProfile(id string) (model.AuditoriumProfile, bool, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM auditorium_profiles WHERE auditorium_id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AuditoriumProfile{}, false, nil
	}
	if err != nil {
		return model.AuditoriumProfile{}, false, err
	}
	var p model.AuditoriumProfile
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return model.AuditoriumProfile{}, false, err
	}
	return p, true, nil
}

func (s *SQLiteStore) ListAuditoriumProfiles() ([]model.AuditoriumProfile, error) {
	rows, err := s.db.Query(`SELECT data FROM auditorium_profiles ORDER BY auditorium_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditoriumProfile
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var p model.AuditoriumProfile
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteAuditoriumProfile(id string) error {
	_, err := s.db.Exec(`DELETE FROM auditorium_profiles WHERE auditorium_id=?`, id)
	return err
}

// --- System config / versioning ---

func (s *SQLiteStore) GetSystemConfig() (model.SystemConfigVersion, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM system_config WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SystemConfigVersion{
			ConfigSchemaVersion: model.CurrentConfigSchemaVersion,
			UpdateChannel:       "stable",
		}, nil
	}
	if err != nil {
		return model.SystemConfigVersion{}, err
	}
	var v model.SystemConfigVersion
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return model.SystemConfigVersion{}, err
	}
	if v.ConfigSchemaVersion == 0 {
		// Records written before the field was maintained: report the schema the
		// database is actually stamped with rather than a zero (§10.8 exists so a
		// tech can see the revisions in play).
		if stored, ok, err := s.storedSchemaVersion(); err == nil && ok {
			v.ConfigSchemaVersion = stored
		}
	}
	return v, nil
}

func (s *SQLiteStore) PutSystemConfig(v model.SystemConfigVersion) error {
	// The schema version is a property of the database, not of the caller's
	// record: stamp it from the build that is writing (§10.8).
	v.ConfigSchemaVersion = model.CurrentConfigSchemaVersion
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO system_config(id,data) VALUES(1,?)
         ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(b))
	return err
}

// --- Admin credentials ---

func (s *SQLiteStore) PutAdminCredential(c AdminCredential) error {
	if c.Username == "" || c.PWHash == "" {
		return errors.New("username and pwhash required")
	}
	must := 0
	if c.MustChange {
		must = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO admin_credentials(username,pwhash,must_change) VALUES(?,?,?)
         ON CONFLICT(username) DO UPDATE SET pwhash=excluded.pwhash, must_change=excluded.must_change`,
		c.Username, c.PWHash, must)
	return err
}

func (s *SQLiteStore) GetAdminCredential(username string) (AdminCredential, bool, error) {
	var c AdminCredential
	var must int
	err := s.db.QueryRow(
		`SELECT username,pwhash,must_change FROM admin_credentials WHERE username=?`, username).
		Scan(&c.Username, &c.PWHash, &must)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminCredential{}, false, nil
	}
	if err != nil {
		return AdminCredential{}, false, err
	}
	c.MustChange = must != 0
	return c, true, nil
}

func (s *SQLiteStore) HasAnyAdminCredential() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_credentials`).Scan(&n)
	return n > 0, err
}

func (s *SQLiteStore) HasDefaultCredential() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_credentials WHERE must_change=1`).Scan(&n)
	return n > 0, err
}

// --- Settings ---

func (s *SQLiteStore) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *SQLiteStore) PutSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?)
         ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

var _ Store = (*SQLiteStore)(nil)
