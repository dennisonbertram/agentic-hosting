package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RevokeOldestExpired finds the oldest expired (but not yet revoked) API key
// for the given tenant and revokes it. It returns the revoked key's ID, or
// ("", nil) if there are no expired keys to revoke.
func (s *Store) RevokeOldestExpired(tenantID string) (string, error) {
	now := time.Now().Unix()
	id, err := s.revokeOldestWhere(tenantID, now,
		`AND expires_at IS NOT NULL AND expires_at < ?`, now)
	if err != nil {
		return "", fmt.Errorf("revoke oldest expired key: %w", err)
	}
	return id, nil
}

// RevokeOldest finds the oldest (by created_at) non-revoked API key for the
// given tenant and revokes it. It returns the revoked key's ID, or ("", nil)
// if no keys exist to revoke.
func (s *Store) RevokeOldest(tenantID string) (string, error) {
	id, err := s.revokeOldestWhere(tenantID, time.Now().Unix(), "")
	if err != nil {
		return "", fmt.Errorf("revoke oldest key: %w", err)
	}
	return id, nil
}

// revokeOldestWhere selects and revokes the tenant's oldest non-revoked key
// matching the extra predicate in a single UPDATE statement. Doing the select
// and the update atomically means two concurrent callers (e.g. parallel key
// recoveries on a full keyring) each revoke a distinct key instead of both
// "revoking" the same one and leaving the keyring over its limit.
func (s *Store) revokeOldestWhere(tenantID string, now int64, extra string, extraArgs ...any) (string, error) {
	args := append([]any{now, tenantID}, extraArgs...)
	var keyID string
	err := s.StateDB.QueryRow(
		`UPDATE api_keys SET revoked_at = ?
		 WHERE id = (
		     SELECT id FROM api_keys
		     WHERE tenant_id = ? AND revoked_at IS NULL `+extra+`
		     ORDER BY created_at ASC
		     LIMIT 1
		 ) AND revoked_at IS NULL
		 RETURNING id`,
		args...,
	).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return keyID, nil
}
