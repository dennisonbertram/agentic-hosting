package db_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStoreWithTenant creates an in-memory Store with state migrations applied
// and a single active tenant.
func newStoreWithTenant(t *testing.T, tenantID string) *db.Store {
	t.Helper()
	stateDB, err := sql.Open("sqlite3", "file::memory:?mode=memory&cache=shared&_foreign_keys=on&_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { stateDB.Close() })
	require.NoError(t, db.ApplyStateMigrations(stateDB))

	_, err = stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, 'Test', ?, 'active', 1, 1)`,
		tenantID, tenantID+"@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, tenantID)
	require.NoError(t, err)

	return &db.Store{StateDB: stateDB}
}

func TestRevokeOldestExpired(t *testing.T) {
	t.Run("revokes the oldest expired key", func(t *testing.T) {
		store := newStoreWithTenant(t, "t1")
		now := time.Now().Unix()

		// Insert 3 keys: one not expired, two expired (different ages).
		_, err := store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
			 VALUES (?, 't1', 'active', 'prefix01', 'hash', ?, NULL)`,
			"key-active", now-100,
		)
		require.NoError(t, err)
		_, err = store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
			 VALUES (?, 't1', 'old-expired', 'prefix02', 'hash', ?, ?)`,
			"key-old-exp", now-200, now-50,
		)
		require.NoError(t, err)
		_, err = store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
			 VALUES (?, 't1', 'new-expired', 'prefix03', 'hash', ?, ?)`,
			"key-new-exp", now-100, now-10,
		)
		require.NoError(t, err)

		revokedID, err := store.RevokeOldestExpired("t1")
		require.NoError(t, err)
		assert.Equal(t, "key-old-exp", revokedID, "should revoke the oldest expired key")

		// Verify it is actually revoked in the DB.
		var revokedAt sql.NullInt64
		err = store.StateDB.QueryRow(
			`SELECT revoked_at FROM api_keys WHERE id = ?`, "key-old-exp",
		).Scan(&revokedAt)
		require.NoError(t, err)
		assert.True(t, revokedAt.Valid, "revoked_at should be set")
	})

	t.Run("returns empty when no expired keys exist", func(t *testing.T) {
		store := newStoreWithTenant(t, "t2")
		now := time.Now().Unix()

		_, err := store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
			 VALUES (?, 't2', 'active', 'prefix01', 'hash', ?)`,
			"key-no-exp", now-100,
		)
		require.NoError(t, err)

		revokedID, err := store.RevokeOldestExpired("t2")
		require.NoError(t, err)
		assert.Empty(t, revokedID, "should return empty when no expired keys")
	})

	t.Run("skips already-revoked expired keys", func(t *testing.T) {
		store := newStoreWithTenant(t, "t3")
		now := time.Now().Unix()

		// An expired key that is already revoked.
		_, err := store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at, revoked_at)
			 VALUES (?, 't3', 'already-revoked', 'prefix01', 'hash', ?, ?, ?)`,
			"key-already-rev", now-200, now-50, now-10,
		)
		require.NoError(t, err)

		revokedID, err := store.RevokeOldestExpired("t3")
		require.NoError(t, err)
		assert.Empty(t, revokedID, "should skip already-revoked keys")
	})
}

func TestRevokeOldest(t *testing.T) {
	t.Run("revokes the oldest key by created_at", func(t *testing.T) {
		store := newStoreWithTenant(t, "t4")
		now := time.Now().Unix()

		for i := 0; i < 5; i++ {
			_, err := store.StateDB.Exec(
				`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
				 VALUES (?, 't4', ?, ?, 'hash', ?)`,
				fmt.Sprintf("key-%d", i), fmt.Sprintf("key-%d", i), fmt.Sprintf("pre%04d", i), now-int64(5-i)*100,
			)
			require.NoError(t, err)
		}

		revokedID, err := store.RevokeOldest("t4")
		require.NoError(t, err)
		assert.Equal(t, "key-0", revokedID, "should revoke the oldest key")

		// Verify it is revoked.
		var revokedAt sql.NullInt64
		err = store.StateDB.QueryRow(
			`SELECT revoked_at FROM api_keys WHERE id = ?`, "key-0",
		).Scan(&revokedAt)
		require.NoError(t, err)
		assert.True(t, revokedAt.Valid)
	})

	t.Run("returns empty when tenant has no keys", func(t *testing.T) {
		store := newStoreWithTenant(t, "t5")

		revokedID, err := store.RevokeOldest("t5")
		require.NoError(t, err)
		assert.Empty(t, revokedID)
	})

	t.Run("skips already-revoked keys", func(t *testing.T) {
		store := newStoreWithTenant(t, "t6")
		now := time.Now().Unix()

		// One revoked key, one active key.
		_, err := store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, revoked_at)
			 VALUES (?, 't6', 'old-revoked', 'prefix01', 'hash', ?, ?)`,
			"key-revoked", now-200, now-100,
		)
		require.NoError(t, err)
		_, err = store.StateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
			 VALUES (?, 't6', 'active', 'prefix02', 'hash', ?)`,
			"key-active", now-50,
		)
		require.NoError(t, err)

		revokedID, err := store.RevokeOldest("t6")
		require.NoError(t, err)
		assert.Equal(t, "key-active", revokedID, "should skip revoked keys and revoke the active one")
	})
}
