package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyRoles_MemberCannotRevokeOrSuspend(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	ownerToken := seedAuthenticatedTenant(t, stateDB, masterKey)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	create := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"agent"}`)))
	create.Header.Set("Authorization", "Bearer "+ownerToken)
	create.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	srv.ServeHTTP(createRR, create)
	require.Equal(t, http.StatusCreated, createRR.Code, createRR.Body.String())

	var created CreateKeyResponse
	require.NoError(t, json.NewDecoder(createRR.Body).Decode(&created))
	assert.Equal(t, "member", created.Role)
	require.NotEmpty(t, created.APIKey)

	revoke := httptest.NewRequest(http.MethodDelete, "/v1/auth/keys/"+created.ID, nil)
	revoke.Header.Set("Authorization", "Bearer "+created.APIKey)
	revokeRR := httptest.NewRecorder()
	srv.ServeHTTP(revokeRR, revoke)
	assert.Equal(t, http.StatusForbidden, revokeRR.Code, revokeRR.Body.String())
	assert.Contains(t, revokeRR.Body.String(), "cannot revoke")

	del := httptest.NewRequest(http.MethodDelete, "/v1/tenant", nil)
	del.Header.Set("Authorization", "Bearer "+created.APIKey)
	delRR := httptest.NewRecorder()
	srv.ServeHTTP(delRR, del)
	assert.Equal(t, http.StatusForbidden, delRR.Code, delRR.Body.String())
	assert.Contains(t, delRR.Body.String(), "cannot suspend")

	// Owner can still revoke the member key.
	okRevoke := httptest.NewRequest(http.MethodDelete, "/v1/auth/keys/"+created.ID, nil)
	okRevoke.Header.Set("Authorization", "Bearer "+ownerToken)
	okRR := httptest.NewRecorder()
	srv.ServeHTTP(okRR, okRevoke)
	assert.Equal(t, http.StatusNoContent, okRR.Code, okRR.Body.String())
}

func TestKeyRoles_MemberCannotMintOwnerKey(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	ownerToken := seedAuthenticatedTenant(t, stateDB, masterKey)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	create := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"agent"}`)))
	create.Header.Set("Authorization", "Bearer "+ownerToken)
	create.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	srv.ServeHTTP(createRR, create)
	require.Equal(t, http.StatusCreated, createRR.Code, createRR.Body.String())
	var member CreateKeyResponse
	require.NoError(t, json.NewDecoder(createRR.Body).Decode(&member))

	escalate := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"boss","role":"owner"}`)))
	escalate.Header.Set("Authorization", "Bearer "+member.APIKey)
	escalate.Header.Set("Content-Type", "application/json")
	escRR := httptest.NewRecorder()
	srv.ServeHTTP(escRR, escalate)
	assert.Equal(t, http.StatusForbidden, escRR.Code, escRR.Body.String())
}

func TestKeyRoles_OwnerCanMintOwnerKey(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	ownerToken := seedAuthenticatedTenant(t, stateDB, masterKey)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	create := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"ops","role":"owner"}`)))
	create.Header.Set("Authorization", "Bearer "+ownerToken)
	create.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, create)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created CreateKeyResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&created))
	assert.Equal(t, "owner", created.Role)
}

func TestKeyRoles_InvalidRoleRejected(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader([]byte(`{"name":"x","role":"admin"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
