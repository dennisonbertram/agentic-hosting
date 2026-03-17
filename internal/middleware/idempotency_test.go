package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testIdempotencyTenantID = "tenant-idem-test"

// sha256Hex mirrors the production body-hash logic for test setup.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// idempotencyNextHandler is a simple handler that echoes a fixed JSON body.
func idempotencyNextHandler(body string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
}

// idemRequestWithTenant returns a request with the given method, path, idempotency key,
// body, and tenant ID set in context.
func idemRequestWithTenant(method, path, idempotencyKey, tenantID, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	ctx := context.WithValue(req.Context(), TenantIDKey, tenantID)
	return req.WithContext(ctx)
}

func TestIdempotency_PassthroughForGET(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{"ok":true}`, http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	req.Header.Set("Idempotency-Key", "key-abc")
	ctx := context.WithValue(req.Context(), TenantIDKey, testIdempotencyTenantID)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_PassthroughWithoutKey(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{"ok":true}`, http.StatusOK))

	// POST but no Idempotency-Key header
	req := idemRequestWithTenant(http.MethodPost, "/v1/services", "", testIdempotencyTenantID, `{"name":"svc"}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_DuplicateKeyReturnsCachedResponse(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{"id":"svc-1"}`, http.StatusCreated))

	body := `{"name":"my-service"}`

	// First request — handler runs
	req1 := idemRequestWithTenant(http.MethodPost, "/v1/services", "create-svc-001", testIdempotencyTenantID, body)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)
	assert.Empty(t, rec1.Header().Get("Idempotency-Replayed"))

	// Second request with same key and body — should return cached response
	req2 := idemRequestWithTenant(http.MethodPost, "/v1/services", "create-svc-001", testIdempotencyTenantID, body)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusCreated, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, `{"id":"svc-1"}`, rec2.Body.String())
}

func TestIdempotency_BodyHashMismatchReturnsConflict(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{"id":"svc-1"}`, http.StatusCreated))

	// First request with original body
	req1 := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-mismatch-001", testIdempotencyTenantID, `{"name":"original"}`)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	// Second request with same key but DIFFERENT body
	req2 := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-mismatch-001", testIdempotencyTenantID, `{"name":"different"}`)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusConflict, rec2.Code)
}

func TestIdempotency_ExpiredEntryNotReplayed(t *testing.T) {
	store := NewIdempotencyStore()

	body := `{"name":"my-service"}`
	responseBody := `{"id":"svc-expired"}`

	// Pre-populate the store with an expired entry
	reqBodyHash := sha256Hex([]byte(body))
	fullKey := testIdempotencyTenantID + ":POST:/v1/services:key-expired-001"
	store.mu.Lock()
	store.entries[fullKey] = &idempotencyEntry{
		statusCode:  http.StatusCreated,
		contentType: "application/json",
		body:        []byte(responseBody),
		bodyHash:    reqBodyHash,
		createdAt:   time.Now().Add(-20 * time.Minute),
		expiresAt:   time.Now().Add(-10 * time.Minute), // already expired
	}
	store.mu.Unlock()

	callCount := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"svc-new"}`))
	})
	handler := store.Middleware(next)

	req := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-expired-001", testIdempotencyTenantID, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Expired entry should NOT be replayed — handler should be called
	assert.Equal(t, 1, callCount, "handler should be invoked for expired idempotency entry")
	assert.Empty(t, rec.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_ScopedPerTenant(t *testing.T) {
	store := NewIdempotencyStore()

	body := `{"name":"shared-name"}`

	// Tenant A makes a request
	callsA := 0
	nextA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"tenant":"A"}`))
	})
	handlerA := store.Middleware(nextA)

	reqA := idemRequestWithTenant(http.MethodPost, "/v1/services", "shared-key", "tenant-A", body)
	recA := httptest.NewRecorder()
	handlerA.ServeHTTP(recA, reqA)
	require.Equal(t, http.StatusCreated, recA.Code)

	// Tenant B uses the same idempotency key — should NOT get tenant A's cached response
	callsB := 0
	nextB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"tenant":"B"}`))
	})
	handlerB := store.Middleware(nextB)

	reqB := idemRequestWithTenant(http.MethodPost, "/v1/services", "shared-key", "tenant-B", body)
	recB := httptest.NewRecorder()
	handlerB.ServeHTTP(recB, reqB)

	assert.Equal(t, 1, callsB, "tenant B's handler should be called — not cached under tenant A's scope")
	assert.Empty(t, recB.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_KeyTooLongReturns400(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{}`, http.StatusOK))

	longKey := strings.Repeat("x", maxIdempotencyKeyLen+1)
	req := idemRequestWithTenant(http.MethodPost, "/v1/services", longKey, testIdempotencyTenantID, `{}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIdempotency_AuthKeyPathSkipped(t *testing.T) {
	store := NewIdempotencyStore()
	callCount := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"secret":"supersecret"}`))
	})
	handler := store.Middleware(next)

	for i := 0; i < 3; i++ {
		req := idemRequestWithTenant(http.MethodPost, "/v1/auth/keys", "idem-key-001", testIdempotencyTenantID, `{"name":"mykey"}`)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	}
	// Auth key creation should never be cached — handler called every time
	assert.Equal(t, 3, callCount, "auth key creation must not be idempotency-cached")
}

func TestIdempotency_MissingTenantIDFailsClosed(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(idempotencyNextHandler(`{}`, http.StatusOK))

	// POST with idempotency key but no tenant in context
	req := httptest.NewRequest(http.MethodPost, "/v1/services", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "some-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestIdempotency_PUTMethodCached(t *testing.T) {
	store := NewIdempotencyStore()
	callCount := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"updated":true}`))
	})
	handler := store.Middleware(next)

	body := `{"replicas":3}`

	req1 := idemRequestWithTenant(http.MethodPut, "/v1/services/svc-1", "put-key-001", testIdempotencyTenantID, body)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	req2 := idemRequestWithTenant(http.MethodPut, "/v1/services/svc-1", "put-key-001", testIdempotencyTenantID, body)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, 1, callCount, "handler should only run once for PUT with same idempotency key")
	assert.Equal(t, "true", rec2.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_ErrorResponseNotCached(t *testing.T) {
	store := NewIdempotencyStore()
	callCount := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	})
	handler := store.Middleware(next)

	body := `{"name":"svc"}`

	req1 := idemRequestWithTenant(http.MethodPost, "/v1/services", "err-key-001", testIdempotencyTenantID, body)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusInternalServerError, rec1.Code)

	// Second request — error was not cached, handler should be called again
	req2 := idemRequestWithTenant(http.MethodPost, "/v1/services", "err-key-001", testIdempotencyTenantID, body)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, 2, callCount, "error responses must not be cached")
	assert.Empty(t, rec2.Header().Get("Idempotency-Replayed"))
}

func TestIdempotency_LargeResponseNotCached(t *testing.T) {
	store := NewIdempotencyStore()
	callCount := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		// Write a response larger than maxIdempotencyBodyLen to trigger overflow
		largeBody := bytes.Repeat([]byte("x"), maxIdempotencyBodyLen+1)
		w.Write(largeBody)
	})
	handler := store.Middleware(next)

	reqBody := `{"name":"svc"}`

	req1 := idemRequestWithTenant(http.MethodPost, "/v1/services", "large-resp-001", testIdempotencyTenantID, reqBody)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Second request — overflow response was not cached
	req2 := idemRequestWithTenant(http.MethodPost, "/v1/services", "large-resp-001", testIdempotencyTenantID, reqBody)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, 2, callCount, "oversize response must not be cached")
	assert.Empty(t, rec2.Header().Get("Idempotency-Replayed"))
}
