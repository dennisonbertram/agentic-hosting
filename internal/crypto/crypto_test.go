package crypto_test

import (
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	plaintext := []byte("hello, world — secret message 🔐")

	ciphertext, err := crypto.Encrypt(plaintext, key)
	require.NoError(t, err, "Encrypt should not fail")
	assert.NotEmpty(t, ciphertext, "ciphertext should be non-empty")
	assert.NotEqual(t, string(plaintext), ciphertext, "ciphertext should differ from plaintext")

	recovered, err := crypto.Decrypt(ciphertext, key)
	require.NoError(t, err, "Decrypt should not fail")
	assert.Equal(t, plaintext, recovered, "recovered plaintext should match original")
}

func TestEncryptDecrypt_WrongKey(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xAA
	}
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = 0xBB
	}

	plaintext := []byte("sensitive data")
	ciphertext, err := crypto.Encrypt(plaintext, key)
	require.NoError(t, err)

	_, err = crypto.Decrypt(ciphertext, wrongKey)
	assert.Error(t, err, "Decrypt with wrong key should fail")
}

func TestEncryptDecrypt_KeyTooShort(t *testing.T) {
	shortKey := []byte("tooshort") // less than 32 bytes

	_, err := crypto.Encrypt([]byte("data"), shortKey)
	assert.Error(t, err, "Encrypt should fail with short key")
	assert.Contains(t, err.Error(), "32", "error should mention 32 bytes")

	_, err = crypto.Decrypt("aabbcc", shortKey)
	assert.Error(t, err, "Decrypt should fail with short key")
}

func TestEncryptDecrypt_Nondeterministic(t *testing.T) {
	// Each encryption of the same plaintext should produce a different ciphertext
	// because the nonce is random.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	plaintext := []byte("same message")

	ct1, err := crypto.Encrypt(plaintext, key)
	require.NoError(t, err)
	ct2, err := crypto.Encrypt(plaintext, key)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "two encryptions of the same plaintext should differ (random nonce)")
}

func TestHashAPIKey_And_VerifyAPIKey(t *testing.T) {
	secret := []byte("server-hmac-secret-32-bytes-long!!")
	key := "my-api-key-value"

	hash := crypto.HashAPIKey(key, secret)
	assert.NotEmpty(t, hash, "hash should be non-empty")
	assert.Equal(t, 64, len(hash), "HMAC-SHA256 hex digest should be 64 chars")

	// Verify with correct key
	ok := crypto.VerifyAPIKey(hash, key, secret)
	assert.True(t, ok, "VerifyAPIKey should return true for correct key")

	// Verify with wrong key
	ok = crypto.VerifyAPIKey(hash, "wrong-key", secret)
	assert.False(t, ok, "VerifyAPIKey should return false for wrong key")

	// Verify with wrong secret
	ok = crypto.VerifyAPIKey(hash, key, []byte("different-secret-here-0000000000"))
	assert.False(t, ok, "VerifyAPIKey should return false for wrong secret")
}

func TestGenerateAPIKey_LengthAndFormat(t *testing.T) {
	key, err := crypto.GenerateAPIKey()
	require.NoError(t, err, "GenerateAPIKey should not fail")

	// GenerateAPIKey returns hex encoding of 32 random bytes → 64 hex chars
	assert.Equal(t, 64, len(key), "API key should be 64 hex characters")

	// All characters should be valid hex
	validHex := strings.ToLower(key)
	for _, ch := range validHex {
		assert.True(t, (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f'),
			"all characters should be hex digits, got %c", ch)
	}
}

func TestGenerateAPIKey_Unique(t *testing.T) {
	key1, err := crypto.GenerateAPIKey()
	require.NoError(t, err)
	key2, err := crypto.GenerateAPIKey()
	require.NoError(t, err)

	assert.NotEqual(t, key1, key2, "two generated API keys should be different")
}

func TestGenerateAPIKeyWithID(t *testing.T) {
	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)

	assert.Equal(t, 64, len(secret), "secret should be 64 hex chars")
	assert.Equal(t, 32, len(keyID), "key ID should be 32 hex chars (16 bytes)")
	assert.NotEqual(t, secret, keyID, "secret and key ID should differ")
}
