package crypto

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
)

// RotateResult summarizes what a key rotation did (or would do in dry-run mode).
type RotateResult struct {
	ServiceEnvVars    int // rows re-encrypted in service_env
	DatabasePasswords int // rows re-encrypted in databases.password_encrypted
	DatabaseConnStrs  int // rows re-encrypted in databases.connection_string_encrypted
	KanbanTokens      int // rows re-encrypted in kanbans.admin_token_encrypted
	SnapshotEnvs      int // rows re-encrypted in snapshots.env_encrypted
	Errors            int // individual field decryption/encryption failures (should be 0)
}

// Total returns the total number of fields that were (or would be) re-encrypted.
func (r RotateResult) Total() int {
	return r.ServiceEnvVars + r.DatabasePasswords + r.DatabaseConnStrs + r.KanbanTokens + r.SnapshotEnvs
}

// RotateKeys re-encrypts every encrypted field in the database from oldKey to
// newKey. The entire operation runs inside a single SQLite transaction for
// atomicity: either every field is re-encrypted or none are.
//
// This is an OFFLINE operation — the server must be stopped before running it.
// Key material is never logged or included in error messages.
func RotateKeys(db *sql.DB, oldKey, newKey []byte, dryRun bool) (RotateResult, error) {
	var result RotateResult

	if len(oldKey) < 32 {
		return result, fmt.Errorf("old key must be at least 32 bytes, got %d", len(oldKey))
	}
	if len(newKey) < 32 {
		return result, fmt.Errorf("new key must be at least 32 bytes, got %d", len(newKey))
	}

	tx, err := db.Begin()
	if err != nil {
		return result, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// 1. Re-encrypt service_env.value_encrypted
	count, errs, err := rotateServiceEnv(tx, oldKey, newKey, dryRun)
	if err != nil {
		return result, fmt.Errorf("rotate service env vars: %w", err)
	}
	result.ServiceEnvVars = count
	result.Errors += errs

	// 2. Re-encrypt databases.password_encrypted
	count, errs, err = rotateDatabasePasswords(tx, oldKey, newKey, dryRun)
	if err != nil {
		return result, fmt.Errorf("rotate database passwords: %w", err)
	}
	result.DatabasePasswords = count
	result.Errors += errs

	// 3. Re-encrypt databases.connection_string_encrypted
	count, errs, err = rotateDatabaseConnStrs(tx, oldKey, newKey, dryRun)
	if err != nil {
		return result, fmt.Errorf("rotate database connection strings: %w", err)
	}
	result.DatabaseConnStrs = count
	result.Errors += errs

	// 4. Re-encrypt kanbans.admin_token_encrypted
	count, errs, err = rotateKanbanTokens(tx, oldKey, newKey, dryRun)
	if err != nil {
		return result, fmt.Errorf("rotate kanban tokens: %w", err)
	}
	result.KanbanTokens = count
	result.Errors += errs

	// 5. Re-encrypt snapshots.env_encrypted
	count, errs, err = rotateSnapshotEnvs(tx, oldKey, newKey, dryRun)
	if err != nil {
		return result, fmt.Errorf("rotate snapshot envs: %w", err)
	}
	result.SnapshotEnvs = count
	result.Errors += errs

	// Abort if any errors occurred — we want all-or-nothing.
	if result.Errors > 0 {
		return result, fmt.Errorf("rotation aborted: %d field(s) failed to re-encrypt", result.Errors)
	}

	if dryRun {
		// Do not commit in dry-run mode.
		log.Printf("dry-run: would re-encrypt %d field(s) total", result.Total())
		return result, nil
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit transaction: %w", err)
	}

	return result, nil
}

// reencrypt decrypts ciphertext with oldKey and re-encrypts with newKey.
// Returns the new ciphertext hex string.
func reencrypt(ciphertextHex string, oldKey, newKey []byte) (string, error) {
	plaintext, err := Decrypt(ciphertextHex, oldKey)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	newCiphertext, err := Encrypt(plaintext, newKey)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	return newCiphertext, nil
}

func rotateServiceEnv(tx *sql.Tx, oldKey, newKey []byte, dryRun bool) (int, int, error) {
	rows, err := tx.Query(`SELECT service_id, key, value_encrypted FROM service_env`)
	if err != nil {
		return 0, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type row struct {
		serviceID, key, valueEnc string
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.serviceID, &r.key, &r.valueEnc); err != nil {
			return 0, 0, fmt.Errorf("scan: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate: %w", err)
	}

	var count, errs int
	for _, r := range toUpdate {
		newEnc, err := reencrypt(r.valueEnc, oldKey, newKey)
		if err != nil {
			log.Printf("ERROR: failed to re-encrypt service_env (service_id=%s, key=%s): %v", r.serviceID, r.key, err)
			errs++
			continue
		}
		if !dryRun {
			if _, err := tx.Exec(
				`UPDATE service_env SET value_encrypted = ? WHERE service_id = ? AND key = ?`,
				newEnc, r.serviceID, r.key,
			); err != nil {
				return count, errs, fmt.Errorf("update service_env (service_id=%s, key=%s): %w", r.serviceID, r.key, err)
			}
		}
		count++
	}
	return count, errs, nil
}

func rotateDatabasePasswords(tx *sql.Tx, oldKey, newKey []byte, dryRun bool) (int, int, error) {
	rows, err := tx.Query(`SELECT id, password_encrypted FROM databases WHERE password_encrypted IS NOT NULL AND password_encrypted != ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, passwordEnc string
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.passwordEnc); err != nil {
			return 0, 0, fmt.Errorf("scan: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate: %w", err)
	}

	var count, errs int
	for _, r := range toUpdate {
		newEnc, err := reencrypt(r.passwordEnc, oldKey, newKey)
		if err != nil {
			log.Printf("ERROR: failed to re-encrypt databases.password_encrypted (id=%s): %v", r.id, err)
			errs++
			continue
		}
		if !dryRun {
			if _, err := tx.Exec(
				`UPDATE databases SET password_encrypted = ? WHERE id = ?`,
				newEnc, r.id,
			); err != nil {
				return count, errs, fmt.Errorf("update databases.password_encrypted (id=%s): %w", r.id, err)
			}
		}
		count++
	}
	return count, errs, nil
}

func rotateDatabaseConnStrs(tx *sql.Tx, oldKey, newKey []byte, dryRun bool) (int, int, error) {
	rows, err := tx.Query(`SELECT id, connection_string_encrypted FROM databases WHERE connection_string_encrypted IS NOT NULL AND connection_string_encrypted != ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, connStrEnc string
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.connStrEnc); err != nil {
			return 0, 0, fmt.Errorf("scan: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate: %w", err)
	}

	var count, errs int
	for _, r := range toUpdate {
		newEnc, err := reencrypt(r.connStrEnc, oldKey, newKey)
		if err != nil {
			log.Printf("ERROR: failed to re-encrypt databases.connection_string_encrypted (id=%s): %v", r.id, err)
			errs++
			continue
		}
		if !dryRun {
			if _, err := tx.Exec(
				`UPDATE databases SET connection_string_encrypted = ? WHERE id = ?`,
				newEnc, r.id,
			); err != nil {
				return count, errs, fmt.Errorf("update databases.connection_string_encrypted (id=%s): %w", r.id, err)
			}
		}
		count++
	}
	return count, errs, nil
}

func rotateKanbanTokens(tx *sql.Tx, oldKey, newKey []byte, dryRun bool) (int, int, error) {
	rows, err := tx.Query(`SELECT id, admin_token_encrypted FROM kanbans WHERE admin_token_encrypted IS NOT NULL AND admin_token_encrypted != ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, tokenEnc string
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tokenEnc); err != nil {
			return 0, 0, fmt.Errorf("scan: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate: %w", err)
	}

	var count, errs int
	for _, r := range toUpdate {
		newEnc, err := reencrypt(r.tokenEnc, oldKey, newKey)
		if err != nil {
			log.Printf("ERROR: failed to re-encrypt kanbans.admin_token_encrypted (id=%s): %v", r.id, err)
			errs++
			continue
		}
		if !dryRun {
			if _, err := tx.Exec(
				`UPDATE kanbans SET admin_token_encrypted = ? WHERE id = ?`,
				newEnc, r.id,
			); err != nil {
				return count, errs, fmt.Errorf("update kanbans.admin_token_encrypted (id=%s): %w", r.id, err)
			}
		}
		count++
	}
	return count, errs, nil
}

// rotateSnapshotEnvs handles the snapshots.env_encrypted column. This column
// contains a JSON object where each value is individually AES-256-GCM encrypted.
// We must parse the JSON, re-encrypt each value, then write the updated JSON back.
func rotateSnapshotEnvs(tx *sql.Tx, oldKey, newKey []byte, dryRun bool) (int, int, error) {
	rows, err := tx.Query(`SELECT id, env_encrypted FROM snapshots WHERE env_encrypted IS NOT NULL AND env_encrypted != '' AND env_encrypted != '{}'`)
	if err != nil {
		return 0, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, envEnc string
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.envEnc); err != nil {
			return 0, 0, fmt.Errorf("scan: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate: %w", err)
	}

	var count, errs int
	for _, r := range toUpdate {
		// Parse the JSON blob: {"KEY": "encrypted-hex", ...}
		var envMap map[string]string
		if err := json.Unmarshal([]byte(r.envEnc), &envMap); err != nil {
			log.Printf("ERROR: failed to parse snapshot env JSON (id=%s): %v", r.id, err)
			errs++
			continue
		}

		if len(envMap) == 0 {
			continue // nothing to re-encrypt
		}

		newMap := make(map[string]string, len(envMap))
		fieldErr := false
		for k, cipherHex := range envMap {
			newCipher, err := reencrypt(cipherHex, oldKey, newKey)
			if err != nil {
				log.Printf("ERROR: failed to re-encrypt snapshot env var (id=%s, key=%s): %v", r.id, k, err)
				errs++
				fieldErr = true
				break // abort this snapshot — partial re-encryption is worse than none
			}
			newMap[k] = newCipher
		}
		if fieldErr {
			continue
		}

		if !dryRun {
			newJSON, err := json.Marshal(newMap)
			if err != nil {
				return count, errs, fmt.Errorf("marshal snapshot env JSON (id=%s): %w", r.id, err)
			}
			if _, err := tx.Exec(
				`UPDATE snapshots SET env_encrypted = ? WHERE id = ?`,
				string(newJSON), r.id,
			); err != nil {
				return count, errs, fmt.Errorf("update snapshots.env_encrypted (id=%s): %w", r.id, err)
			}
		}
		count++
	}
	return count, errs, nil
}
