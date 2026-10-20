package crypto_test

import (
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecrypt_MalformedCiphertext(t *testing.T) {
	key := make([]byte, 32)

	_, err := crypto.Decrypt("not-hex", key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hex decode")

	_, err = crypto.Decrypt("0011", key) // shorter than the GCM nonce
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")

	enc, err := crypto.Encrypt([]byte("secret"), key)
	require.NoError(t, err)
	tampered := enc[:len(enc)-2] + "00"
	if tampered == enc {
		tampered = enc[:len(enc)-2] + "11"
	}
	_, err = crypto.Decrypt(tampered, key)
	require.Error(t, err, "GCM must reject a modified tag")
}

// Only the first 32 bytes of a longer key are used, in both directions.
func TestEncryptDecrypt_LongKeyUsesFirst32Bytes(t *testing.T) {
	long := []byte(strings.Repeat("k", 40))
	enc, err := crypto.Encrypt([]byte("v"), long)
	require.NoError(t, err)
	plain, err := crypto.Decrypt(enc, long[:32])
	require.NoError(t, err)
	assert.Equal(t, "v", string(plain))
}

func TestLoadOrCreateAPIKeyHashKey_StableAndWrongMasterKey(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	k1, err := crypto.LoadOrCreateAPIKeyHashKey(db, oldKey)
	require.NoError(t, err)
	assert.Equal(t, oldKey, k1, "first use seeds the hash key with the master key")

	k2, err := crypto.LoadOrCreateAPIKeyHashKey(db, oldKey)
	require.NoError(t, err)
	assert.Equal(t, k1, k2, "second load must return the stored key")

	_, err = crypto.LoadOrCreateAPIKeyHashKey(db, newKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong master key")
}

// A corrupt kanban token (encrypted under another key) aborts the whole
// rotation and reports the failure against the right column.
func TestRotateKeys_CorruptKanbanTokenAborts(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()
	ensureTenantAndService(t, db, "svc-k")
	wrong := make([]byte, 32)
	wrong[0] = 0xEE
	seedKanban(t, db, wrong, "kb-bad", "tenant-svc-k", "token")
	seedDatabase(t, db, oldKey, "db-good", "tenant-svc-k", "pw", "postgres://x")

	res, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.Error(t, err)
	assert.Equal(t, 1, res.Errors)
	assert.Equal(t, 1, res.DatabasePasswords)
	assert.Equal(t, 0, res.KanbanTokens)

	// Rolled back: the good database password still decrypts with the old key.
	var enc string
	require.NoError(t, db.QueryRow(`SELECT password_encrypted FROM databases WHERE id = 'db-good'`).Scan(&enc))
	plain, err := crypto.Decrypt(enc, oldKey)
	require.NoError(t, err)
	assert.Equal(t, "pw", string(plain))
}

// A snapshot env blob that is not valid JSON is counted as an error, not
// silently skipped.
func TestRotateKeys_MalformedSnapshotJSON(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()
	ensureTenantAndService(t, db, "svc-s")
	_, err := db.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, env_encrypted, created_at)
		 VALUES ('snap-bad', 'tenant-svc-s', 'svc-s', 'snap', 'img:latest', 'not-json', 1)`)
	require.NoError(t, err)

	res, err := crypto.RotateKeys(db, oldKey, newKey, true)
	require.Error(t, err)
	assert.Equal(t, 1, res.Errors)
	assert.Equal(t, 0, res.SnapshotEnvs)
}
