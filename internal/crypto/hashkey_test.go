package crypto_test

import (
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DB whose api_keys were hashed before server_secrets existed (no row yet)
// must keep authenticating: the first load seeds the row from the master key,
// and a rotation with no row yet preserves the old key as the hash key.
func TestAPIKeyHashKey_LegacyDBAndRotationWithoutRow(t *testing.T) {
	oldKey, newKey := testKeys()
	db := testutil.NewStateDB(t)
	legacyHash := crypto.HashAPIKey("secret", oldKey) // computed by the old scheme

	// Rotation before the server ever created the row.
	_, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.NoError(t, err)

	hk, err := crypto.LoadOrCreateAPIKeyHashKey(db, newKey)
	require.NoError(t, err)
	assert.True(t, crypto.VerifyAPIKey(legacyHash, "secret", hk))
	assert.False(t, crypto.VerifyAPIKey(legacyHash, "other", hk))

	// Fresh startup on another legacy DB: row seeded from master key.
	db2 := testutil.NewStateDB(t)
	hk2, err := crypto.LoadOrCreateAPIKeyHashKey(db2, oldKey)
	require.NoError(t, err)
	assert.True(t, crypto.VerifyAPIKey(legacyHash, "secret", hk2))
	hk3, err := crypto.LoadOrCreateAPIKeyHashKey(db2, oldKey) // idempotent
	require.NoError(t, err)
	assert.Equal(t, hk2, hk3)
}

func TestRotateKeys_WrongOldKey_LeavesHashKeyUntouched(t *testing.T) {
	oldKey, newKey := testKeys()
	wrong := make([]byte, 32)
	db := testutil.NewStateDB(t)
	_, err := crypto.LoadOrCreateAPIKeyHashKey(db, oldKey)
	require.NoError(t, err)
	var before string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM server_secrets`).Scan(&before))

	_, err = crypto.RotateKeys(db, wrong, newKey, false)
	require.Error(t, err)

	var after string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM server_secrets`).Scan(&after))
	assert.Equal(t, before, after)
	hk, err := crypto.LoadOrCreateAPIKeyHashKey(db, oldKey)
	require.NoError(t, err)
	assert.Equal(t, oldKey, hk)
}
