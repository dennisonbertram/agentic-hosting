package testutil

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

// dbCounter provides unique names for in-memory databases to prevent
// shared-cache collisions between state and metering databases in the
// same test process.
var dbCounter atomic.Int64

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

// NewMeteringDB creates a fresh in-memory SQLite database with all metering
// migrations applied. The returned *sql.DB is closed automatically when the
// test ends. Uses a unique database name to avoid shared-cache collisions
// with NewStateDB when both are used in the same test.
func NewMeteringDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("metering_%d", dbCounter.Add(1))
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_foreign_keys=on&_busy_timeout=5000", name)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("testutil.NewMeteringDB: open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.ApplyMeteringMigrations(sqlDB); err != nil {
		t.Fatalf("testutil.NewMeteringDB: migrations: %v", err)
	}
	return sqlDB
}
