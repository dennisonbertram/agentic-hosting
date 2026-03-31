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

// --- helpers ---

func newTestClient(srv *httptest.Server) *ahclient.Client {
	return ahclient.NewClient(srv.URL, "test-key")
}

// --- BT-001: name-or-ID resolution: 32-char hex uses ID directly ---

func TestResolveServiceID_HexID(t *testing.T) {
	// 32-char hex string should be used as-is without any ListServices call.
	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/services" {
			listCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ahclient.Service{ID: "abcdef1234567890abcdef1234567890", Name: "my-svc"})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	hexID := "abcdef1234567890abcdef1234567890" // exactly 32 hex chars
	id, err := resolveServiceID(c, hexID)
	require.NoError(t, err)
	assert.Equal(t, hexID, id, "32-char hex should be used directly as service ID")
	assert.False(t, listCalled, "ListServices must NOT be called when arg is a 32-char hex ID")
}

// --- BT-002: name-or-ID resolution: name triggers list lookup ---

func TestResolveServiceID_ByName(t *testing.T) {
	wantID := "aabbccdd11223344aabbccdd11223344"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: wantID, Name: "my-service"},
				{ID: "00112233445566778899aabbccddeeff", Name: "other-service"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	id, err := resolveServiceID(c, "my-service")
	require.NoError(t, err)
	assert.Equal(t, wantID, id, "name resolution should return matching service ID")
}

// --- BT-003: name resolution: not found returns error ---

func TestResolveServiceID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Service{
			{ID: "aabbccdd11223344aabbccdd11223344", Name: "other-service"},
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	_, err := resolveServiceID(c, "no-such-service")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-service", "error should mention the service name")
}

// --- BT-004: delete requires --confirm flag ---

func TestServiceDelete_RequiresConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runServiceDelete(c, "my-svc", false /* confirm=false */)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm", "error must mention --confirm flag")
	assert.False(t, deleteCalled, "DELETE must not be called without --confirm")
}

// --- BT-005: delete with --confirm proceeds ---

func TestServiceDelete_WithConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/services/") {
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runServiceDelete(c, "my-svc", true /* confirm=true */)
	require.NoError(t, err)
	assert.True(t, deleteCalled, "DELETE must be called when --confirm is set")
}

// --- BT-006: KEY=VALUE parsing for env set ---

func TestParseEnvPairs_Valid(t *testing.T) {
	pairs := []string{"FOO=bar", "BAR=baz=qux", "EMPTY="}
	got, err := parseEnvPairs(pairs)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"FOO":   "bar",
		"BAR":   "baz=qux",
		"EMPTY": "",
	}, got)
}

func TestParseEnvPairs_MissingEquals(t *testing.T) {
	_, err := parseEnvPairs([]string{"NOEQUALS"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOEQUALS", "error must mention the offending argument")
}

func TestParseEnvPairs_EmptyKey(t *testing.T) {
	_, err := parseEnvPairs([]string{"=value"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty key", "error must mention empty key")
}

// --- BT-007: table output for service list ---

func TestServiceListTable_Columns(t *testing.T) {
	services := []ahclient.Service{
		{
			ID:         "abcdef1234567890abcdef1234567890",
			Name:       "web-app",
			Status:     "running",
			URL:        "https://web-app.example.com",
			CrashCount: 3,
		},
	}

	var buf bytes.Buffer
	writeServiceTable(&buf, services)

	output := buf.String()
	// Header row must contain all required columns
	assert.Contains(t, output, "NAME", "table must have NAME column")
	assert.Contains(t, output, "STATUS", "table must have STATUS column")
	assert.Contains(t, output, "ID", "table must have ID column")
	assert.Contains(t, output, "URL", "table must have URL column")
	assert.Contains(t, output, "RESTARTS", "table must have RESTARTS column")

	// Data row must contain service details
	assert.Contains(t, output, "web-app", "table must show service name")
	assert.Contains(t, output, "running", "table must show service status")
	assert.Contains(t, output, "abcdef12", "table must show truncated ID")
	assert.Contains(t, output, "3", "table must show crash count")
}

func TestServiceListTable_TruncatedID(t *testing.T) {
	services := []ahclient.Service{
		{ID: "abcdef1234567890abcdef1234567890", Name: "svc"},
	}
	var buf bytes.Buffer
	writeServiceTable(&buf, services)
	output := buf.String()
	// ID should be truncated — should NOT show full 32-char ID
	assert.NotContains(t, output, "abcdef1234567890abcdef1234567890",
		"full 32-char ID should not appear in table (should be truncated)")
	assert.Contains(t, output, "abcdef12", "truncated ID prefix should appear")
}

// --- BT-008: ambiguous name returns error ---

func TestResolveServiceID_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Two services with the same name (shouldn't happen in practice but must be handled)
		json.NewEncoder(w).Encode([]ahclient.Service{
			{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", Name: "duplicate"},
			{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2", Name: "duplicate"},
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	_, err := resolveServiceID(c, "duplicate")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous", "error must say 'ambiguous'")
}

// --- regression tests ---

// Regression: resolveServiceID should NOT treat a non-hex 32-char string as an ID.
func TestResolveServiceID_Non32HexIsName(t *testing.T) {
	// 32 chars but contains 'g' — not valid hex → treated as name, not ID
	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/services" {
			listCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	// This is 32 chars but has 'g' so not hex
	_, _ = resolveServiceID(c, "abcdefg123456789abcdefg123456789")
	assert.True(t, listCalled, "non-hex 32-char arg must trigger ListServices (treated as name)")
}

// Regression: parseEnvPairs must handle values containing '=' (e.g., base64 encoded strings).
func TestParseEnvPairs_ValueContainsEquals(t *testing.T) {
	got, err := parseEnvPairs([]string{"TOKEN=abc=def=ghi"})
	require.NoError(t, err)
	assert.Equal(t, "abc=def=ghi", got["TOKEN"], "value containing '=' must be preserved entirely")
}

// Regression: confirm guard on delete must check the exact flag value, not just presence.
func TestServiceDelete_ExplicitFalseConfirm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runServiceDelete(c, "my-svc", false)
	require.Error(t, err, "delete without confirm must always return an error")
}
