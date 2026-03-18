package testutil

import (
	"database/sql"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

// NewStateDB creates a fresh in-memory SQLite database with all state migrations applied.
// The returned *sql.DB is closed automatically when the test ends.
func NewStateDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use shared cache so all connections see the same in-memory database.
	// Without this, each connection from the pool gets its own empty database.
	sqlDB, err := sql.Open("sqlite3", "file::memory:?mode=memory&cache=shared&_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("testutil.NewStateDB: open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.ApplyStateMigrations(sqlDB); err != nil {
		t.Fatalf("testutil.NewStateDB: migrations: %v", err)
	}
	return sqlDB
}
