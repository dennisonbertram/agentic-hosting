package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func Encrypt(plaintext []byte, key []byte) (string, error) {
	if len(key) < 32 {
		return "", fmt.Errorf("key must be at least 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", fmt.Errorf("new cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("new gcm: %w", err)
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}

	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

func Decrypt(ciphertextHex string, key []byte) ([]byte, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("key must be at least 32 bytes, got %d", len(key))
	}
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}

	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt hash: %w", err)
	}
	return string(hash), nil
}

func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// HashAPIKey produces a fast, constant-time-comparable hash for API key verification.
// Uses HMAC-SHA256 with a server secret to prevent offline brute-force if DB is leaked.
func HashAPIKey(key string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyAPIKey compares an API key against a stored HMAC hash in constant time.
func VerifyAPIKey(storedHash, key string, secret []byte) bool {
	computed := HashAPIKey(key, secret)
	return hmac.Equal([]byte(storedHash), []byte(computed))
}

// APIKeyHashSecretName is the server_secrets row holding the API-key HMAC key.
const APIKeyHashSecretName = "api_key_hmac"

// LoadOrCreateAPIKeyHashKey returns the HMAC key used to hash API keys. It is
// stored encrypted under masterKey in server_secrets so master key rotation
// can re-encrypt it without changing existing key hashes. On first use the
// key is the master key itself, because that is what pre-existing hashes were
// computed with.
func LoadOrCreateAPIKeyHashKey(db *sql.DB, masterKey []byte) ([]byte, error) {
	enc, err := Encrypt(masterKey, masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt api key hash key: %w", err)
	}
	// INSERT OR IGNORE keeps concurrent first starts from overwriting each other.
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO server_secrets (name, value_encrypted, created_at) VALUES (?, ?, ?)`,
		APIKeyHashSecretName, enc, time.Now().Unix(),
	); err != nil {
		return nil, fmt.Errorf("store api key hash key: %w", err)
	}
	var stored string
	if err := db.QueryRow(`SELECT value_encrypted FROM server_secrets WHERE name = ?`, APIKeyHashSecretName).Scan(&stored); err != nil {
		return nil, fmt.Errorf("load api key hash key: %w", err)
	}
	key, err := Decrypt(stored, masterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt api key hash key (wrong master key?): %w", err)
	}
	return key, nil
}

// GenerateAPIKey generates a random 64-char hex API key secret.
func GenerateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateAPIKeyWithID generates an API key secret and a unique key ID.
// Returns (secret, keyID, error). The caller combines them as "keyID.secret"
// in the token given to the user, enabling O(1) lookup by key ID.
func GenerateAPIKeyWithID() (string, string, error) {
	secret, err := GenerateAPIKey()
	if err != nil {
		return "", "", err
	}
	idBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, idBytes); err != nil {
		return "", "", fmt.Errorf("generate key id: %w", err)
	}
	keyID := hex.EncodeToString(idBytes)
	return secret, keyID, nil
}
