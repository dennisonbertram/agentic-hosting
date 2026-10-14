package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxIdempotencyEntries   = 500
	maxIdempotencyPerTenant = 50
	maxIdempotencyKeyLen    = 128
	maxIdempotencyBodyLen   = 8 * 1024 // 8KB max stored response
	idempotencyTTL          = 10 * time.Minute
)

type idempotencyEntry struct {
	statusCode  int
	contentType string // preserved from original response
	body        []byte
	bodyHash    string // SHA-256 hex of request body for mismatch detection
	createdAt   time.Time
	expiresAt   time.Time
	done        chan struct{} // closed when the owning request finishes
	pending     bool          // true while the first request is still running
}

type IdempotencyStore struct {
	mu      sync.RWMutex
	entries map[string]*idempotencyEntry
}

func NewIdempotencyStore() *IdempotencyStore {
	s := &IdempotencyStore{
		entries: make(map[string]*idempotencyEntry),
	}
	go s.cleanup()
	return s
}

func (s *IdempotencyStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.entries {
			if now.After(v.expiresAt) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        []byte
	overflow    bool // true if body exceeded maxIdempotencyBodyLen
}

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.wroteHeader {
		return
	}
	rr.statusCode = code
	rr.wroteHeader = true
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	// Implicit 200 if WriteHeader was never called (per net/http spec)
	if !rr.wroteHeader {
		rr.statusCode = http.StatusOK
		rr.wroteHeader = true
		rr.ResponseWriter.WriteHeader(http.StatusOK)
	}
	// Cap in-memory buffering to prevent memory exhaustion from large responses.
	// Once overflow, stop buffering but continue writing to the client.
	if !rr.overflow {
		if len(rr.body)+len(b) <= maxIdempotencyBodyLen {
			rr.body = append(rr.body, b...)
		} else {
			rr.overflow = true
			rr.body = nil // release buffered bytes
		}
	}
	return rr.ResponseWriter.Write(b)
}

// Flush proxies to the underlying ResponseWriter if it supports http.Flusher.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter for middleware that uses
// http.ResponseController or similar unwrapping patterns.
func (rr *responseRecorder) Unwrap() http.ResponseWriter {
	return rr.ResponseWriter
}

const maxHashBodySize = 1 << 20 // 1MB — matches server max body size

// errBodyTooLarge is returned when the request body exceeds maxHashBodySize.
var errBodyTooLarge = fmt.Errorf("request body too large")

// hashRequestBody reads the request body (up to maxHashBodySize), computes
// SHA-256, and replaces r.Body with a new reader so downstream handlers can
// still read it. Returns errBodyTooLarge if the body exceeds the limit.
func hashRequestBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "empty", nil
	}
	lr := io.LimitReader(r.Body, maxHashBodySize+1)
	bodyBytes, err := io.ReadAll(lr)
	if err != nil {
		return "", err
	}
	if int64(len(bodyBytes)) > maxHashBodySize {
		return "", errBodyTooLarge
	}
	r.Body.Close()
	// Replace body so downstream handlers can read it
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	h := sha256.Sum256(bodyBytes)
	return hex.EncodeToString(h[:]), nil
}

func (s *IdempotencyStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only apply idempotency to POST and PUT
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Never cache auth key creation responses because they return a secret
		// that should only be shown on the original creation response.
		if strings.HasPrefix(r.URL.Path, "/v1/auth/keys") {
			next.ServeHTTP(w, r)
			return
		}

		// Validate key length
		if len(key) > maxIdempotencyKeyLen {
			writeJSONError(w, http.StatusBadRequest, "idempotency key too long")
			return
		}

		// Scope by tenant + method + path + query string.
		// Fail closed: reject if tenant ID is missing when idempotency key is
		// present, to prevent silent bypass of idempotency guarantees.
		tenantID := GetTenantID(r.Context())
		if tenantID == "" {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		fullKey := tenantID + ":" + r.Method + ":" + r.URL.RequestURI() + ":" + key

		// Hash request body for payload mismatch detection.
		// Body is already capped by maxBodySize middleware (1MB).
		reqBodyHash, err := hashRequestBody(r)
		if err != nil {
			if err == errBodyTooLarge {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			} else {
				writeJSONError(w, http.StatusBadRequest, "failed to read request body")
			}
			return
		}

		var entry *idempotencyEntry
		for {
			s.mu.Lock()
			existing, exists := s.entries[fullKey]
			if exists && !time.Now().Before(existing.expiresAt) {
				delete(s.entries, fullKey)
				exists = false
			}
			if !exists {
				entry = s.reserve(tenantID, fullKey, reqBodyHash)
				s.mu.Unlock()
				break
			}
			if existing.bodyHash != reqBodyHash {
				s.mu.Unlock()
				writeJSONError(w, http.StatusConflict, "idempotency key reused with different request body")
				return
			}
			if !existing.pending {
				s.mu.Unlock()
				replay(w, existing)
				return
			}
			done := existing.done
			s.mu.Unlock()
			// Another request with this key is in flight: wait for it, then replay
			// (or run ourselves if it produced nothing cacheable).
			select {
			case <-done:
			case <-r.Context().Done():
				writeJSONError(w, http.StatusConflict, "a request with this idempotency key is still in progress")
				return
			}
		}

		// Always release waiters and drop the pending entry unless a response was stored
		// (also covers handler panics).
		stored := false
		defer func() {
			s.mu.Lock()
			if !stored && s.entries[fullKey] == entry {
				delete(s.entries, fullKey)
			}
			s.mu.Unlock()
			close(entry.done)
		}()

		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		// Default to 200 if handler never called WriteHeader (per net/http spec)
		if !rec.wroteHeader {
			rec.statusCode = http.StatusOK
		}

		// Only cache successful (2xx) responses within size bounds.
		// Error responses are not cached to prevent "sticky failure" attacks.
		if rec.statusCode >= 200 && rec.statusCode < 300 && !rec.overflow && len(rec.body) <= maxIdempotencyBodyLen {
			s.mu.Lock()
			if s.entries[fullKey] == entry {
				entry.statusCode = rec.statusCode
				entry.contentType = rec.Header().Get("Content-Type")
				entry.body = rec.body
				entry.pending = false
				stored = true
			}
			s.mu.Unlock()
		}
	})
}

func replay(w http.ResponseWriter, entry *idempotencyEntry) {
	w.Header().Set("Idempotency-Replayed", "true")
	// Always set security header for consistency with httpx helpers
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(entry.body) > 0 {
		if entry.contentType != "" {
			w.Header().Set("Content-Type", entry.contentType)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.WriteHeader(entry.statusCode)
	if len(entry.body) > 0 {
		w.Write(entry.body)
	}
}

// reserve inserts a pending entry for fullKey, evicting the tenant's oldest
// completed entry at the per-tenant cap and the globally oldest at the global cap.
// Caller must hold s.mu.
func (s *IdempotencyStore) reserve(tenantID, fullKey, bodyHash string) *idempotencyEntry {
	tenantPrefix := tenantID + ":"
	count := 0
	var oldestTenantKey string
	var oldestTenantTime time.Time
	for k, v := range s.entries {
		if strings.HasPrefix(k, tenantPrefix) {
			count++
			if !v.pending && (oldestTenantKey == "" || v.createdAt.Before(oldestTenantTime)) {
				oldestTenantKey, oldestTenantTime = k, v.createdAt
			}
		}
	}
	if count >= maxIdempotencyPerTenant && oldestTenantKey != "" {
		delete(s.entries, oldestTenantKey)
	}
	if len(s.entries) >= maxIdempotencyEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.entries {
			if !v.pending && (oldestKey == "" || v.createdAt.Before(oldestTime)) {
				oldestKey, oldestTime = k, v.createdAt
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	now := time.Now()
	e := &idempotencyEntry{bodyHash: bodyHash, createdAt: now, expiresAt: now.Add(idempotencyTTL), done: make(chan struct{}), pending: true}
	s.entries[fullKey] = e
	return e
}
