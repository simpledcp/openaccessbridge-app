package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// TestSchemaVersionStampedOnFreshDatabase: a new database records the schema
// version this build writes (§10.8 config_schema_version).
func TestSchemaVersionStampedOnFreshDatabase(t *testing.T) {
	s := openTemp(t)
	got, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != model.CurrentConfigSchemaVersion {
		t.Errorf("schema version = %d, want %d", got, model.CurrentConfigSchemaVersion)
	}
}

// TestRefusesADatabaseFromANewerBuild: a config database stamped by a later
// schema must not be opened and read as if it were this one. Silently accepting
// it is how an appliance comes up in a posture nobody configured (§10.8,
// guardrail #7).
func TestRefusesADatabaseFromANewerBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := s.stampSchemaVersion(model.CurrentConfigSchemaVersion + 7); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	s.Close()

	reopened, err := OpenSQLite(path)
	if err == nil {
		reopened.Close()
		t.Fatalf("opening a newer-schema database must fail loudly")
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("the error must name the cause, got %v", err)
	}
}

// TestAnUnreadableSchemaVersionIsNotAssumedToBeOurs.
func TestAnUnreadableSchemaVersionIsNotAssumedToBeOurs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE schema_meta SET value='banana' WHERE key=?`, schemaVersionKey); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	s.Close()

	if reopened, err := OpenSQLite(path); err == nil {
		reopened.Close()
		t.Fatalf("an unparsable schema version must fail loudly, not default to ours")
	}
}

// TestOlderDatabaseIsMigratedAndRestamped: the stamp moves forward once the
// (currently empty) migration path has run, so the next open is a no-op rather
// than a repeated migration.
func TestOlderDatabaseIsMigratedAndRestamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := s.stampSchemaVersion(0); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	s.Close()

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("an older database must still open: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != model.CurrentConfigSchemaVersion {
		t.Errorf("schema version after migration = %d, want %d", got, model.CurrentConfigSchemaVersion)
	}
}

// TestSystemConfigCarriesTheSchemaVersion: §10.8's record is only useful if the
// revisions in it are the ones actually in play, so the writing build stamps the
// schema version rather than trusting the caller's copy.
func TestSystemConfigCarriesTheSchemaVersion(t *testing.T) {
	s := openTemp(t)
	if err := s.PutSystemConfig(model.SystemConfigVersion{UpdateChannel: "stable"}); err != nil {
		t.Fatalf("PutSystemConfig: %v", err)
	}
	got, err := s.GetSystemConfig()
	if err != nil {
		t.Fatalf("GetSystemConfig: %v", err)
	}
	if got.ConfigSchemaVersion != model.CurrentConfigSchemaVersion {
		t.Errorf("record schema version = %d, want %d", got.ConfigSchemaVersion, model.CurrentConfigSchemaVersion)
	}
}

func openTemp(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
