package crypto_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKeys returns two distinct 32-byte keys for testing.
func testKeys() (oldKey, newKey []byte) {
	oldKey = make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	newKey = make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(i + 100)
	}
	return
}

// seedServiceEnv inserts encrypted env vars for testing.
func seedServiceEnv(t *testing.T, db *sql.DB, key []byte, serviceID string, vars map[string]string) {
	t.Helper()
	// Ensure the tenant and service exist first.
	ensureTenantAndService(t, db, serviceID)
	for k, v := range vars {
		enc, err := crypto.Encrypt([]byte(v), key)
		require.NoError(t, err)
		_, err = db.Exec(
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`,
			serviceID, k, enc,
		)
		require.NoError(t, err)
	}
}

// seedDatabase inserts a database record with encrypted password and connection string.
func seedDatabase(t *testing.T, db *sql.DB, key []byte, id, tenantID, password, connStr string) {
	t.Helper()
	passEnc, err := crypto.Encrypt([]byte(password), key)
	require.NoError(t, err)
	connEnc, err := crypto.Encrypt([]byte(connStr), key)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO databases (id, tenant_id, name, type, status, password_encrypted, connection_string_encrypted, created_at, updated_at)
		 VALUES (?, ?, 'testdb', 'postgres', 'ready', ?, ?, 1, 1)`,
		id, tenantID, passEnc, connEnc,
	)
	require.NoError(t, err)
}

// seedKanban inserts a kanban record with an encrypted admin token.
func seedKanban(t *testing.T, db *sql.DB, key []byte, id, tenantID, adminToken string) {
	t.Helper()
	tokenEnc, err := crypto.Encrypt([]byte(adminToken), key)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, admin_token_encrypted, created_at, updated_at)
		 VALUES (?, ?, 'ready', ?, 1, 1)`,
		id, tenantID, tokenEnc,
	)
	require.NoError(t, err)
}

// seedSnapshot inserts a snapshot with an encrypted env JSON blob.
func seedSnapshot(t *testing.T, db *sql.DB, key []byte, id, tenantID, serviceID string, envVars map[string]string) {
	t.Helper()
	envBlob := make(map[string]string, len(envVars))
	for k, v := range envVars {
		enc, err := crypto.Encrypt([]byte(v), key)
		require.NoError(t, err)
		envBlob[k] = enc
	}
	envJSON, err := json.Marshal(envBlob)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, env_encrypted, created_at)
		 VALUES (?, ?, ?, 'snap1', 'img:latest', ?, 1)`,
		id, tenantID, serviceID, string(envJSON),
	)
	require.NoError(t, err)
}

// ensureTenantAndService creates the required tenant, tenant_quotas, and services rows
// if they don't already exist. Column names match the real migration schema.
func ensureTenantAndService(t *testing.T, db *sql.DB, serviceID string) {
	t.Helper()
	tenantID := "tenant-" + serviceID
	// Insert tenant if not exists (tenants table has: id, name, email, status, created_at, updated_at).
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, 'test', ?, 'active', 1, 1)`, tenantID, tenantID+"@test.com",
	)
	// Insert tenant_quotas if not exists.
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO tenant_quotas (tenant_id, max_services, max_databases)
		 VALUES (?, 10, 10)`, tenantID,
	)
	// Insert service if not exists (port and dns_label are added by later migrations).
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO services (id, tenant_id, name, dns_label, status, image, port, created_at, updated_at)
		 VALUES (?, ?, 'svc', ?, 'running', 'img:latest', 8080, 1, 1)`,
		serviceID, tenantID, "dns-"+serviceID,
	)
}

func TestRotateKeys_RoundTrip(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	// Seed all four encrypted data types.
	ensureTenantAndService(t, db, "svc1")

	seedServiceEnv(t, db, oldKey, "svc1", map[string]string{
		"SECRET_KEY": "my-secret-value",
		"DB_URL":     "postgres://user:pass@host/db",
	})

	seedDatabase(t, db, oldKey, "db1", "tenant-svc1", "dbpassword123", "postgres://u:p@h:5432/d")

	seedKanban(t, db, oldKey, "kb1", "tenant-svc1", "admin-token-xyz")

	seedSnapshot(t, db, oldKey, "snap1", "tenant-svc1", "svc1", map[string]string{
		"API_KEY":     "key123",
		"DB_PASSWORD": "pass456",
	})

	// Rotate keys.
	result, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.NoError(t, err)

	assert.Equal(t, 2, result.ServiceEnvVars, "expected 2 service env vars")
	assert.Equal(t, 1, result.DatabasePasswords, "expected 1 database password")
	assert.Equal(t, 1, result.DatabaseConnStrs, "expected 1 database connection string")
	assert.Equal(t, 1, result.KanbanTokens, "expected 1 kanban token")
	assert.Equal(t, 1, result.SnapshotEnvs, "expected 1 snapshot env blob")
	assert.Equal(t, 0, result.Errors, "expected no errors")
	assert.Equal(t, 6, result.Total(), "expected 6 total fields")

	// Verify data can be decrypted with the NEW key.
	// 1. Service env vars
	var enc string
	err = db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc1", "SECRET_KEY").Scan(&enc)
	require.NoError(t, err)
	plain, err := crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "my-secret-value", string(plain))

	// Verify OLD key no longer works.
	_, err = crypto.Decrypt(enc, oldKey)
	assert.Error(t, err, "old key should not decrypt data after rotation")

	// 2. Database password
	err = db.QueryRow(`SELECT password_encrypted FROM databases WHERE id = ?`, "db1").Scan(&enc)
	require.NoError(t, err)
	plain, err = crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "dbpassword123", string(plain))

	// 3. Database connection string
	err = db.QueryRow(`SELECT connection_string_encrypted FROM databases WHERE id = ?`, "db1").Scan(&enc)
	require.NoError(t, err)
	plain, err = crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:p@h:5432/d", string(plain))

	// 4. Kanban admin token
	err = db.QueryRow(`SELECT admin_token_encrypted FROM kanbans WHERE id = ?`, "kb1").Scan(&enc)
	require.NoError(t, err)
	plain, err = crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "admin-token-xyz", string(plain))

	// 5. Snapshot env vars
	err = db.QueryRow(`SELECT env_encrypted FROM snapshots WHERE id = ?`, "snap1").Scan(&enc)
	require.NoError(t, err)
	var envMap map[string]string
	require.NoError(t, json.Unmarshal([]byte(enc), &envMap))
	for k, cipherHex := range envMap {
		plain, err := crypto.Decrypt(cipherHex, newKey)
		require.NoError(t, err)
		switch k {
		case "API_KEY":
			assert.Equal(t, "key123", string(plain))
		case "DB_PASSWORD":
			assert.Equal(t, "pass456", string(plain))
		default:
			t.Errorf("unexpected env key: %s", k)
		}
	}
}

func TestRotateKeys_DryRun(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	ensureTenantAndService(t, db, "svc1")
	seedServiceEnv(t, db, oldKey, "svc1", map[string]string{
		"KEY1": "val1",
	})
	seedDatabase(t, db, oldKey, "db1", "tenant-svc1", "pass", "conn://str")

	// Get the original ciphertext so we can compare after dry run.
	var origEnvEnc, origPassEnc string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc1", "KEY1").Scan(&origEnvEnc))
	require.NoError(t, db.QueryRow(`SELECT password_encrypted FROM databases WHERE id = ?`, "db1").Scan(&origPassEnc))

	// Dry run.
	result, err := crypto.RotateKeys(db, oldKey, newKey, true)
	require.NoError(t, err)

	assert.Equal(t, 1, result.ServiceEnvVars)
	assert.Equal(t, 1, result.DatabasePasswords)
	assert.Equal(t, 1, result.DatabaseConnStrs)
	assert.Equal(t, 0, result.Errors)

	// Verify data was NOT changed.
	var afterEnvEnc, afterPassEnc string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc1", "KEY1").Scan(&afterEnvEnc))
	require.NoError(t, db.QueryRow(`SELECT password_encrypted FROM databases WHERE id = ?`, "db1").Scan(&afterPassEnc))

	assert.Equal(t, origEnvEnc, afterEnvEnc, "dry-run should not modify service_env")
	assert.Equal(t, origPassEnc, afterPassEnc, "dry-run should not modify database password")

	// Verify the old key still works (nothing changed).
	plain, err := crypto.Decrypt(afterEnvEnc, oldKey)
	require.NoError(t, err)
	assert.Equal(t, "val1", string(plain))
}

func TestRotateKeys_EmptyDatabase(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	result, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Total(), "no data to rotate should succeed with 0 total")
	assert.Equal(t, 0, result.Errors)
}

func TestRotateKeys_WrongOldKey(t *testing.T) {
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = 0xFF
	}

	ensureTenantAndService(t, db, "svc1")
	seedServiceEnv(t, db, oldKey, "svc1", map[string]string{
		"KEY1": "val1",
	})

	// Try to rotate with the wrong old key.
	result, err := crypto.RotateKeys(db, wrongKey, newKey, false)
	assert.Error(t, err, "rotation should fail when old key is wrong")
	assert.Contains(t, err.Error(), "failed to re-encrypt")
	assert.Equal(t, 1, result.Errors)
}

func TestRotateKeys_KeyTooShort(t *testing.T) {
	db := testutil.NewStateDB(t)
	shortKey := make([]byte, 16)
	goodKey := make([]byte, 32)

	_, err := crypto.RotateKeys(db, shortKey, goodKey, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "old key must be at least 32 bytes")

	_, err = crypto.RotateKeys(db, goodKey, shortKey, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "new key must be at least 32 bytes")
}

func TestRotateKeys_Atomicity(t *testing.T) {
	// Verify that if one field fails to decrypt, the entire transaction is rolled back.
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = 0xDD
	}

	ensureTenantAndService(t, db, "svc1")

	// Insert one env var encrypted with the correct old key.
	seedServiceEnv(t, db, oldKey, "svc1", map[string]string{
		"GOOD": "good-value",
	})

	// Insert another env var encrypted with a DIFFERENT key (simulating corruption).
	badEnc, err := crypto.Encrypt([]byte("bad-value"), wrongKey)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`,
		"svc1", "BAD", badEnc,
	)
	require.NoError(t, err)

	// Get original ciphertext for the good value.
	var origGoodEnc string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc1", "GOOD").Scan(&origGoodEnc))

	// Rotation should fail because BAD key can't be decrypted with oldKey.
	result, err := crypto.RotateKeys(db, oldKey, newKey, false)
	assert.Error(t, err)
	assert.Equal(t, 1, result.Errors)

	// Verify the GOOD value was NOT changed (transaction rolled back).
	var afterGoodEnc string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc1", "GOOD").Scan(&afterGoodEnc))
	assert.Equal(t, origGoodEnc, afterGoodEnc, "good value should be unchanged after failed rotation (atomic rollback)")

	// Verify old key still works for the good value.
	plain, err := crypto.Decrypt(afterGoodEnc, oldKey)
	require.NoError(t, err)
	assert.Equal(t, "good-value", string(plain))
}

func TestRotateKeys_NullAndEmptyFields(t *testing.T) {
	// Ensure rotation handles NULL and empty encrypted fields gracefully.
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	// Create a tenant for the database.
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES ('t1', 'test', 't1@test.com', 'active', 1, 1)`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services, max_databases)
		 VALUES ('t1', 10, 10)`)
	require.NoError(t, err)

	// Insert a database with password but NULL connection string.
	passEnc, err := crypto.Encrypt([]byte("mypass"), oldKey)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO databases (id, tenant_id, name, type, status, password_encrypted, connection_string_encrypted, created_at, updated_at)
		 VALUES ('db-null', 't1', 'testdb', 'postgres', 'ready', ?, NULL, 1, 1)`, passEnc,
	)
	require.NoError(t, err)

	// Insert a kanban with empty admin_token_encrypted.
	_, err = db.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, admin_token_encrypted, created_at, updated_at)
		 VALUES ('kb-empty', 't1', 'ready', '', 1, 1)`,
	)
	require.NoError(t, err)

	// Insert a snapshot with empty env_encrypted.
	_, err = db.Exec(
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, created_at, updated_at)
		 VALUES ('svc-null', 't1', 'svc', 'svc-null', 'running', 'img:latest', 8080, 1, 1)`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, env_encrypted, created_at)
		 VALUES ('snap-empty', 't1', 'svc-null', 'snap', 'img:latest', '', 1)`,
	)
	require.NoError(t, err)

	// Rotation should succeed, only touching the non-null/non-empty fields.
	result, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.NoError(t, err)

	assert.Equal(t, 0, result.ServiceEnvVars)
	assert.Equal(t, 1, result.DatabasePasswords, "should re-encrypt the non-null password")
	assert.Equal(t, 0, result.DatabaseConnStrs, "NULL conn str should be skipped")
	assert.Equal(t, 0, result.KanbanTokens, "empty token should be skipped")
	assert.Equal(t, 0, result.SnapshotEnvs, "empty env should be skipped")
	assert.Equal(t, 0, result.Errors)

	// Verify the password was re-encrypted with the new key.
	var enc string
	require.NoError(t, db.QueryRow(`SELECT password_encrypted FROM databases WHERE id = ?`, "db-null").Scan(&enc))
	plain, err := crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "mypass", string(plain))
}

func TestRotateKeys_MultipleRows(t *testing.T) {
	// Ensure rotation handles multiple rows across all tables.
	db := testutil.NewStateDB(t)
	oldKey, newKey := testKeys()

	// Set up two services with env vars.
	ensureTenantAndService(t, db, "svc-a")
	ensureTenantAndService(t, db, "svc-b")
	seedServiceEnv(t, db, oldKey, "svc-a", map[string]string{"K1": "v1", "K2": "v2"})
	seedServiceEnv(t, db, oldKey, "svc-b", map[string]string{"K3": "v3"})

	// Two databases.
	seedDatabase(t, db, oldKey, "db-a", "tenant-svc-a", "pass-a", "conn-a")
	seedDatabase(t, db, oldKey, "db-b", "tenant-svc-b", "pass-b", "conn-b")

	result, err := crypto.RotateKeys(db, oldKey, newKey, false)
	require.NoError(t, err)

	assert.Equal(t, 3, result.ServiceEnvVars)
	assert.Equal(t, 2, result.DatabasePasswords)
	assert.Equal(t, 2, result.DatabaseConnStrs)
	assert.Equal(t, 0, result.Errors)
	assert.Equal(t, 7, result.Total())

	// Spot check one value from each service.
	var enc string
	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc-a", "K1").Scan(&enc))
	plain, err := crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(plain))

	require.NoError(t, db.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = ? AND key = ?`, "svc-b", "K3").Scan(&enc))
	plain, err = crypto.Decrypt(enc, newKey)
	require.NoError(t, err)
	assert.Equal(t, "v3", string(plain))
}
