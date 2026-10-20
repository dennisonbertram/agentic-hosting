package testutil

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

// NewFileStateDB creates a file-backed state DB opened like production
// (WAL, busy timeout, multiple connections) with migrations applied.
// Use it for tests that need real write-lock contention.
func NewFileStateDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	sqlDB, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		t.Fatalf("testutil.NewFileStateDB: open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.ApplyStateMigrations(sqlDB); err != nil {
		t.Fatalf("testutil.NewFileStateDB: migrations: %v", err)
	}
	return sqlDB
}
