package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyCreate_ExpiredKeysDoNotOccupyQuota(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	now := time.Now().Unix()

	// Fill remaining slots with expired keys (seed key is the only live one).
	for i := 1; i < maxKeysPerTenant; i++ {
		_, err := stateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"expired-"+strconv.Itoa(i), "tenant-1", "expired", "exp"+strconv.Itoa(i)+"xxx", "hash",
			1, now-60,
		)
		require.NoError(t, err)
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"replacement"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "expired keys must not block a new key: %s", rr.Body.String())
}

func TestKeyList_OmitsExpiredKeys(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	now := time.Now().Unix()
	_, err := stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 VALUES ('expired-key', 'tenant-1', 'stale', 'stale001', 'hash', 1, ?)`,
		now-1,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var keys []KeyInfo
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&keys))
	for _, k := range keys {
		assert.NotEqual(t, "expired-key", k.ID, "expired keys must not appear in the list")
	}
	assert.Len(t, keys, 1, "only the live seed key should be listed")
}

func TestTenantUsage_ExpiredKeysAreNotCounted(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	now := time.Now().Unix()
	_, err := stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 VALUES ('expired-key', 'tenant-1', 'stale', 'stale001', 'hash', 1, ?)`,
		now-1,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp TenantUsageResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, 1, resp.APIKeys.Used, "expired keys must not count toward usage")
}
