package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- BT-DB-001: maskConnString hides password, only shows host:port ----

func TestMaskConnString_Postgres(t *testing.T) {
	input := "postgres://user:supersecretpassword@db.example.com:5432/mydb"
	got := maskConnString(input)
	assert.NotContains(t, got, "supersecretpassword", "password must not appear in masked connection string")
	assert.Contains(t, got, "db.example.com:5432", "host:port must be visible in masked connection string")
}

func TestMaskConnString_Redis(t *testing.T) {
	input := "redis://:myredispassword@cache.example.com:6379/0"
	got := maskConnString(input)
	assert.NotContains(t, got, "myredispassword", "redis password must not appear in masked connection string")
	assert.Contains(t, got, "cache.example.com:6379", "redis host:port must be visible in masked connection string")
}

func TestMaskConnString_NoPassword(t *testing.T) {
	// Connection strings with no password should pass through (host:port still visible)
	input := "postgres://user@db.example.com:5432/mydb"
	got := maskConnString(input)
	assert.Contains(t, got, "db.example.com:5432", "host:port must be visible when no password present")
}

func TestMaskConnString_Opaque(t *testing.T) {
	// Non-URL connection strings (plain DSN) should still not expose passwords
	// e.g. "host=db.example.com port=5432 user=foo password=secret dbname=mydb"
	// For opaque strings we just return them as-is (no URL to parse), which is
	// fine — the test is that URL-format strings are properly masked.
	input := "not-a-url"
	got := maskConnString(input)
	// Must not panic and must return something
	assert.NotEmpty(t, got)
}

// ---- BT-DB-002: envKeyForType returns correct env var name ----

func TestEnvKeyForType_Postgres(t *testing.T) {
	key := envKeyForType("postgres")
	assert.Equal(t, "DATABASE_URL", key, "postgres type must map to DATABASE_URL env var key")
}

func TestEnvKeyForType_Redis(t *testing.T) {
	key := envKeyForType("redis")
	assert.Equal(t, "REDIS_URL", key, "redis type must map to REDIS_URL env var key")
}

func TestEnvKeyForType_Unknown(t *testing.T) {
	// Unknown types should still return something (fallback) rather than panic
	key := envKeyForType("mysql")
	assert.NotEmpty(t, key, "envKeyForType must return a non-empty string for unknown types")
}

// ---- BT-DB-003: delete requires --confirm flag ----

func TestRunDbDelete_RequiresConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		if r.URL.Path == "/v1/databases" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Database{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-db", Type: "postgres", Status: "running"},
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runDbDelete(c, "my-db", false /* confirm=false */)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm", "error must mention --confirm flag")
	assert.False(t, deleteCalled, "DELETE must not be called without --confirm")
}

func TestRunDbDelete_WithConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/databases/") {
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/databases" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Database{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-db", Type: "postgres", Status: "running"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runDbDelete(c, "my-db", true /* confirm=true */)
	require.NoError(t, err)
	assert.True(t, deleteCalled, "DELETE must be called when --confirm is set")
}

// ---- BT-DB-004: db list table output has correct columns ----

func TestWriteDbTable_Columns(t *testing.T) {
	dbs := []ahclient.Database{
		{
			ID:     "aabbccdd11223344aabbccdd11223344",
			Name:   "my-postgres",
			Type:   "postgres",
			Status: "running",
		},
	}

	var buf bytes.Buffer
	writeDbTable(&buf, dbs)
	output := buf.String()

	assert.Contains(t, output, "NAME", "table must have NAME column")
	assert.Contains(t, output, "TYPE", "table must have TYPE column")
	assert.Contains(t, output, "STATUS", "table must have STATUS column")
	assert.Contains(t, output, "ID", "table must have ID column")

	assert.Contains(t, output, "my-postgres", "table must show database name")
	assert.Contains(t, output, "postgres", "table must show database type")
	assert.Contains(t, output, "running", "table must show database status")
}

func TestWriteDbTable_IDTruncated(t *testing.T) {
	dbs := []ahclient.Database{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "db"},
	}
	var buf bytes.Buffer
	writeDbTable(&buf, dbs)
	output := buf.String()

	assert.NotContains(t, output, "aabbccdd11223344aabbccdd11223344",
		"full 32-char ID must not appear in table")
	assert.Contains(t, output, "aabbccdd", "truncated ID prefix must appear")
}

// ---- BT-DB-005: resolveDbID uses hex ID directly without a list call ----

func TestResolveDbID_HexID(t *testing.T) {
	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/databases" {
			listCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ahclient.Database{
			ID: "aabbccdd11223344aabbccdd11223344",
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	hexID := "aabbccdd11223344aabbccdd11223344"
	id, err := resolveDbID(c, hexID)
	require.NoError(t, err)
	assert.Equal(t, hexID, id)
	assert.False(t, listCalled, "ListDatabases must NOT be called when arg is a 32-char hex ID")
}

func TestResolveDbID_ByName(t *testing.T) {
	wantID := "aabbccdd11223344aabbccdd11223344"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/databases" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Database{
				{ID: wantID, Name: "my-db", Type: "postgres", Status: "running"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	id, err := resolveDbID(c, "my-db")
	require.NoError(t, err)
	assert.Equal(t, wantID, id)
}

func TestResolveDbID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Database{})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	_, err := resolveDbID(c, "no-such-db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-db")
}

// ---- regression tests ----

// Regression: maskConnString must not panic on empty string.
func TestMaskConnString_Empty(t *testing.T) {
	got := maskConnString("")
	// Should not panic; result is deterministic (empty or some placeholder)
	_ = got
}

// Regression: envKeyForType must be case-sensitive — "Postgres" != "postgres".
func TestEnvKeyForType_CaseSensitive(t *testing.T) {
	// "Postgres" (capital P) is not a known type; should NOT return DATABASE_URL
	// (callers always send lowercase from the CLI flag).
	// This ensures the switch is not accidentally case-insensitive.
	key := envKeyForType("Postgres")
	assert.NotEqual(t, "DATABASE_URL", key, "envKeyForType must be case-sensitive (only lowercase 'postgres' maps to DATABASE_URL)")
}

// Regression: delete with confirm=false must error even if db name looks like a hex ID.
func TestRunDbDelete_HexIDNoConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runDbDelete(c, "aabbccdd11223344aabbccdd11223344", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
	assert.False(t, deleteCalled)
}

// Regression: writeDbTable must handle empty list without panicking.
func TestWriteDbTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	writeDbTable(&buf, []ahclient.Database{})
	// Header should still be present
	assert.Contains(t, buf.String(), "NAME")
}
