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
	d, err := openRotateDB(path)
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE server_secrets`)
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM schema_migrations WHERE name = 'state_020_server_secrets.sql'`)
	require.NoError(t, err)
	d.Close()

	d, err = openRotateDB(path)
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, db.ApplyStateMigrations(d)) // idempotent

	old, nk := make([]byte, 32), make([]byte, 32)
	nk[0] = 1
	res, err := crypto.RotateKeys(d, old, nk, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.HashKeys)
}
