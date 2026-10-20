package db_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStoreWithTenant creates an in-memory Store with state migrations applied
// and a single active tenant.
func newStoreWithTenant(t *testing.T, tenantID string) *db.Store {
	t.Helper()
	// A uniquely named database per test: the previous shared
	// "file::memory:" DSN let every caller in the process share one schema.
	stateDB := testutil.NewStateDB(t)

	_, err := stateDB.Exec(
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

func TestRevokeOldest_ScopedToTenant(t *testing.T) {
	store := newStoreWithTenant(t, "ta")
	_, err := store.StateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES ('tb', 'B', 'tb@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	now := time.Now().Unix()
	// Tenant B owns the globally oldest key, which is also expired.
	_, err = store.StateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 VALUES ('key-b', 'tb', 'b', 'prefixb1', 'hash', ?, ?)`, now-1000, now-500)
	require.NoError(t, err)
	_, err = store.StateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 VALUES ('key-a', 'ta', 'a', 'prefixa1', 'hash', ?, ?)`, now-10, now-5)
	require.NoError(t, err)

	id, err := store.RevokeOldestExpired("ta")
	require.NoError(t, err)
	assert.Equal(t, "key-a", id)

	id, err = store.RevokeOldest("ta")
	require.NoError(t, err)
	assert.Empty(t, id, "tenant A has no live keys left; tenant B's key must not be touched")

	var revokedAt sql.NullInt64
	require.NoError(t, store.StateDB.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = 'key-b'`).Scan(&revokedAt))
	assert.False(t, revokedAt.Valid, "other tenant's key must stay active")
}

// Concurrent callers must each revoke a distinct key: the select and update
// happen in one statement so two recoveries cannot both pick the same row.
func TestRevokeOldest_ConcurrentCallersRevokeDistinctKeys(t *testing.T) {
	stateDB := testutil.NewFileStateDB(t)
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES ('tc', 'C', 'tc@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	store := &db.Store{StateDB: stateDB}

	const n = 8
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		_, err := stateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
			 VALUES (?, 'tc', ?, ?, 'hash', ?)`,
			fmt.Sprintf("k%d", i), fmt.Sprintf("k%d", i), fmt.Sprintf("pfx%05d", i), now-int64(n-i))
		require.NoError(t, err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.RevokeOldest("tc")
			assert.NoError(t, err)
			mu.Lock()
			seen[id]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Len(t, seen, n, "every caller must revoke a different key")
	for id, c := range seen {
		assert.NotEmpty(t, id)
		assert.Equal(t, 1, c, "key %s revoked by more than one caller", id)
	}
}
