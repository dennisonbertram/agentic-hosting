package db_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpen_MigrationsApplied verifies that db.Open applies all migrations and
// the expected tables exist in the state database.
func TestOpen_MigrationsApplied(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := db.Open(dbPath)
	require.NoError(t, err, "db.Open should succeed")
	t.Cleanup(func() { store.Close() })

	// Verify key tables exist in state DB
	expectedTables := []string{
		"tenants",
		"api_keys",
		"tenant_quotas",
		"services",
		"service_env",
		"builds",
		"databases",
		"schema_migrations",
	}
	for _, table := range expectedTables {
		tableExists(t, store.StateDB, table)
	}
}

// TestOpen_MigrationIdempotent verifies that running migrations twice
// (reopening the DB) does not fail or duplicate schema.
func TestOpen_MigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// First open
	store1, err := db.Open(dbPath)
	require.NoError(t, err, "first db.Open should succeed")
	store1.Close()

	// Second open — migrations should be skipped, not reapplied
	store2, err := db.Open(dbPath)
	require.NoError(t, err, "second db.Open should succeed (idempotent migrations)")
	store2.Close()
}

// TestOpen_BothDatabases verifies that both state and metering databases are opened.
func TestOpen_BothDatabases(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	require.NotNil(t, store.StateDB, "StateDB should be non-nil")
	require.NotNil(t, store.MeteringDB, "MeteringDB should be non-nil")

	// Verify metering DB was created on disk
	meteringPath := filepath.Join(dir, "test-metering.db")
	_, err = os.Stat(meteringPath)
	assert.NoError(t, err, "metering DB file should exist on disk")
}

// TestServicesTable_CircuitBreakerColumns verifies columns added in migration 007-009.
func TestServicesTable_CircuitBreakerColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	// Insert a tenant so we can insert a service (FK constraint)
	_, err = store.StateDB.Exec(
		`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ('t1', 'Test Tenant', 'test@example.com', 1, 1)`,
	)
	require.NoError(t, err)

	_, err = store.StateDB.Exec(
		`INSERT INTO tenant_quotas (tenant_id) VALUES ('t1')`,
	)
	require.NoError(t, err)

	_, err = store.StateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, created_at, updated_at) VALUES ('s1', 't1', 'my-service', 1, 1)`,
	)
	require.NoError(t, err)

	// Verify circuit breaker columns are readable with expected defaults
	var crashCount, circuitOpen, circuitOpenCount int
	var circuitRetryAt, crashWindowStart, lastCrashedAt sql.NullInt64
	err = store.StateDB.QueryRow(
		`SELECT crash_count, circuit_open, circuit_open_count, circuit_retry_at, crash_window_start, last_crashed_at FROM services WHERE id = 's1'`,
	).Scan(&crashCount, &circuitOpen, &circuitOpenCount, &circuitRetryAt, &crashWindowStart, &lastCrashedAt)
	require.NoError(t, err, "circuit breaker columns should exist after migrations")

	assert.Equal(t, 0, crashCount, "default crash_count should be 0")
	assert.Equal(t, 0, circuitOpen, "default circuit_open should be 0")
	assert.Equal(t, 0, circuitOpenCount, "default circuit_open_count should be 0")
	assert.False(t, circuitRetryAt.Valid, "default circuit_retry_at should be NULL")
	assert.False(t, crashWindowStart.Valid, "default crash_window_start should be NULL")
	assert.False(t, lastCrashedAt.Valid, "default last_crashed_at should be NULL")
}

// tableExists is a helper that asserts a table exists in the given database.
func tableExists(t *testing.T, db *sql.DB, tableName string) {
	t.Helper()
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tableName,
	).Scan(&count)
	require.NoError(t, err, "querying sqlite_master for table %q failed", tableName)
	assert.Equal(t, 1, count, "table %q should exist", tableName)
}
