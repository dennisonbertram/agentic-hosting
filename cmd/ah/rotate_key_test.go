package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStateDBFile creates a migrated state DB file, as a server start would.
func newStateDBFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ah.db")
	d, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, db.ApplyStateMigrations(d))
	require.NoError(t, d.Close())
	return path
}

// A mistyped or unset --db-path must not make SQLite create an empty DB that
// then "rotates" successfully while the real database keeps the old key.
func TestOpenRotateDB_MissingFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	for _, dryRun := range []bool{false, true} {
		_, err := openRotateDB(path, dryRun)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing.db")
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "openRotateDB must not create the file")
	}
}

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoadKeyFile(t *testing.T) {
	const hexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

	t.Run("valid with trailing newline", func(t *testing.T) {
		key, err := loadKeyFile(writeKeyFile(t, hexKey+"\n"))
		require.NoError(t, err)
		require.Len(t, key, 32)
		assert.Equal(t, byte(0x1f), key[31])
	})
	t.Run("wrong length", func(t *testing.T) {
		_, err := loadKeyFile(writeKeyFile(t, hexKey[:62]))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "62 characters")
	})
	t.Run("surrounding whitespace is rejected", func(t *testing.T) {
		_, err := loadKeyFile(writeKeyFile(t, " "+hexKey[1:]))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid hex")
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := loadKeyFile(filepath.Join(t.TempDir(), "nope.key"))
		require.Error(t, err)
	})
}

// A DB at the pre-020 schema (server has not started since the upgrade) must
// be rotatable: openRotateDB creates server_secrets.
func TestOpenRotateDB_PreServerSecretsSchema(t *testing.T) {
	path := newStateDBFile(t)
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
	path := newStateDBFile(t)
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
	path := newStateDBFile(t)
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
