package testutil

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// NewStateDB creates a fresh in-memory SQLite database with all state migrations applied.
// The returned *sql.DB is closed automatically when the test ends.
func NewStateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("testutil.NewStateDB: open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := applyMigrations(t, db); err != nil {
		t.Fatalf("testutil.NewStateDB: migrations: %v", err)
	}
	return db
}

// applyMigrations applies all state_*.sql migrations from the embedded FS.
// Import the actual migration runner from internal/db if available, or apply inline.
func applyMigrations(t *testing.T, db *sql.DB) error {
	t.Helper()
	// TODO: wire up to the actual migration runner from internal/db
	// For now, apply core schema inline so tests can start running
	schema := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            email TEXT UNIQUE NOT NULL,
            status TEXT DEFAULT 'active',
            created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS api_keys (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id),
            name TEXT NOT NULL,
            key_prefix TEXT NOT NULL,
            key_hash TEXT NOT NULL,
            created_at INTEGER NOT NULL,
            last_used_at INTEGER,
            expires_at INTEGER,
            revoked_at INTEGER
        )`,
		`CREATE TABLE IF NOT EXISTS tenant_quotas (
            tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
            max_services INTEGER DEFAULT 5,
            max_databases INTEGER DEFAULT 3,
            max_memory_mb INTEGER DEFAULT 2048,
            max_cpu_cores REAL DEFAULT 2.0,
            max_disk_gb INTEGER DEFAULT 20,
            api_rate_limit INTEGER DEFAULT 100
        )`,
		`CREATE TABLE IF NOT EXISTS services (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id),
            name TEXT NOT NULL,
            status TEXT DEFAULT 'stopped',
            image TEXT,
            source_type TEXT,
            source_ref TEXT,
            container_id TEXT,
            created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL,
            port INTEGER DEFAULT 8000,
            last_error TEXT DEFAULT '',
            crash_count INTEGER DEFAULT 0,
            circuit_open INTEGER DEFAULT 0,
            last_crashed_at INTEGER,
            crash_window_start INTEGER,
            circuit_retry_at INTEGER,
            circuit_open_count INTEGER DEFAULT 0
        )`,
		`CREATE TABLE IF NOT EXISTS service_env (
            service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
            key TEXT NOT NULL,
            value_encrypted TEXT NOT NULL,
            created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL,
            PRIMARY KEY (service_id, key)
        )`,
		`CREATE TABLE IF NOT EXISTS builds (
            id TEXT PRIMARY KEY,
            service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
            tenant_id TEXT NOT NULL,
            status TEXT DEFAULT 'pending',
            source_type TEXT NOT NULL,
            source_url TEXT,
            source_ref TEXT DEFAULT 'main',
            image TEXT,
            nixpacks_plan TEXT,
            log TEXT DEFAULT '',
            started_at INTEGER,
            finished_at INTEGER,
            created_at INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS databases (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
            name TEXT NOT NULL,
            type TEXT NOT NULL,
            status TEXT DEFAULT 'provisioning',
            container_id TEXT,
            host TEXT DEFAULT '127.0.0.1',
            port INTEGER,
            db_name TEXT,
            username TEXT,
            password_encrypted TEXT NOT NULL,
            connection_string_encrypted TEXT,
            volume_name TEXT,
            created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL
        )`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
