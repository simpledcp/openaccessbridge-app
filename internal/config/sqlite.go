package config

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	if err := s.ensureSchemaVersion(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) ensureSchemaVersion() error {
	_, err := s.db.Exec(
		`INSERT INTO schema_meta(key,value) VALUES('config_schema_version',?)
         ON CONFLICT(key) DO NOTHING`,
		fmt.Sprint(model.CurrentConfigSchemaVersion))
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
	return v, nil
}

func (s *SQLiteStore) PutSystemConfig(v model.SystemConfigVersion) error {
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
