package main

import (
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/require"
)

// A DB at the pre-020 schema (server has not started since the upgrade) must
// be rotatable: openRotateDB creates server_secrets.
func TestOpenRotateDB_PreServerSecretsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ah.db")
	d, err := openRotateDB(path, false)
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE server_secrets`)
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM schema_migrations WHERE name = 'state_020_server_secrets.sql'`)
	require.NoError(t, err)
	d.Close()

	d, err = openRotateDB(path, false)
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, db.ApplyStateMigrations(d)) // idempotent

	old, nk := make([]byte, 32), make([]byte, 32)
	nk[0] = 1
	res, err := crypto.RotateKeys(d, old, nk, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.HashKeys)
}

// Dry-run must not apply migrations: on a pre-020 DB it reports that the hash
// key row would be created and leaves the schema untouched.
func TestOpenRotateDB_DryRunDoesNotMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ah.db")
	d, err := openRotateDB(path, false)
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE server_secrets`)
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM schema_migrations WHERE name = 'state_020_server_secrets.sql'`)
	require.NoError(t, err)
	d.Close()

	d, err = openRotateDB(path, true)
	require.NoError(t, err)
	defer d.Close()

	old, nk := make([]byte, 32), make([]byte, 32)
	nk[0] = 1
	res, err := crypto.RotateKeys(d, old, nk, true)
	require.NoError(t, err)
	require.Equal(t, 1, res.HashKeys)

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'server_secrets'`).Scan(&n))
	require.Equal(t, 0, n, "dry-run must not create server_secrets")
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = 'state_020_server_secrets.sql'`).Scan(&n))
	require.Equal(t, 0, n, "dry-run must not record migrations")
}

// Dry-run on a current DB still computes the result from existing rows.
func TestOpenRotateDB_DryRunCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ah.db")
	d, err := openRotateDB(path, false)
	require.NoError(t, err)
	d.Close()

	d, err = openRotateDB(path, true)
	require.NoError(t, err)
	defer d.Close()
	old, nk := make([]byte, 32), make([]byte, 32)
	nk[0] = 1
	res, err := crypto.RotateKeys(d, old, nk, true)
	require.NoError(t, err)
	require.Equal(t, 1, res.HashKeys)
}
