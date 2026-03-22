package db

import (
	"database/sql"
	"fmt"
	"time"
)

// RevokeOldestExpired finds the oldest expired (but not yet revoked) API key
// for the given tenant and revokes it. It returns the revoked key's ID, or
// ("", nil) if there are no expired keys to revoke.
func (s *Store) RevokeOldestExpired(tenantID string) (string, error) {
	now := time.Now().Unix()
	var keyID string
	err := s.StateDB.QueryRow(
		`SELECT id FROM api_keys
		 WHERE tenant_id = ? AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at < ?
		 ORDER BY created_at ASC
		 LIMIT 1`,
		tenantID, now,
	).Scan(&keyID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find oldest expired key: %w", err)
	}

	_, err = s.StateDB.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND tenant_id = ?`,
		now, keyID, tenantID,
	)
	if err != nil {
		return "", fmt.Errorf("revoke expired key %s: %w", keyID, err)
	}
	return keyID, nil
}

// RevokeOldest finds the oldest (by created_at) non-revoked API key for the
// given tenant and revokes it. It returns the revoked key's ID, or ("", nil)
// if no keys exist to revoke.
func (s *Store) RevokeOldest(tenantID string) (string, error) {
	now := time.Now().Unix()
	var keyID string
	err := s.StateDB.QueryRow(
		`SELECT id FROM api_keys
		 WHERE tenant_id = ? AND revoked_at IS NULL
		 ORDER BY created_at ASC
		 LIMIT 1`,
		tenantID,
	).Scan(&keyID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find oldest key: %w", err)
	}

	_, err = s.StateDB.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND tenant_id = ?`,
		now, keyID, tenantID,
	)
	if err != nil {
		return "", fmt.Errorf("revoke key %s: %w", keyID, err)
	}
	return keyID, nil
}
