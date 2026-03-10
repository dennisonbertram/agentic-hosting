package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/crypto"
	"github.com/paasd/paasd/internal/middleware"
)

type RegisterRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type RegisterResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
}

type TenantResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type UpdateTenantRequest struct {
	Name *string `json:"name,omitempty"`
}

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// IP-based rate limiter for registration
type registrationLimiter struct {
	mu      sync.Mutex
	entries map[string]*regEntry
}

type regEntry struct {
	count    int
	windowAt time.Time
}

var regLimiter = &registrationLimiter{
	entries: make(map[string]*regEntry),
}

const (
	regMaxPerHour = 10
	regWindow     = 1 * time.Hour
)

func (rl *registrationLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, exists := rl.entries[ip]
	if !exists || now.After(entry.windowAt) {
		rl.entries[ip] = &regEntry{count: 1, windowAt: now.Add(regWindow)}
		return true
	}
	if entry.count >= regMaxPerHour {
		return false
	}
	entry.count++
	return true
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleTenantRegister(w http.ResponseWriter, r *http.Request) {
	// IP-based rate limiting
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	if !regLimiter.allow(ip) {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, `{"error":"rate limit exceeded, try again later"}`, http.StatusTooManyRequests)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Validate name
	if len(req.Name) < 2 {
		http.Error(w, `{"error":"name must be at least 2 characters"}`, http.StatusBadRequest)
		return
	}
	if len(req.Name) > 128 {
		http.Error(w, `{"error":"name must be at most 128 characters"}`, http.StatusBadRequest)
		return
	}

	// Validate email format
	if !emailRegex.MatchString(req.Email) {
		http.Error(w, `{"error":"invalid email format"}`, http.StatusBadRequest)
		return
	}
	if len(req.Email) > 256 {
		http.Error(w, `{"error":"email too long"}`, http.StatusBadRequest)
		return
	}

	tenantID, err := generateID()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`,
		tenantID, req.Name, req.Email, now, now,
	)
	if err != nil {
		// Check for unique constraint on email
		http.Error(w, `{"error":"email already registered"}`, http.StatusConflict)
		return
	}

	// Create default quotas
	_, err = tx.Exec(
		`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`,
		tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// Generate API key
	apiKey, err := crypto.GenerateAPIKey()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	keyHash, err := crypto.HashPassword(apiKey)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	keyID, err := generateID()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	prefix := apiKey[:8]

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'default', ?, ?, ?)`,
		keyID, tenantID, prefix, keyHash, now,
	)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(RegisterResponse{
		TenantID: tenantID,
		APIKey:   apiKey,
	})
}

func (s *Server) handleTenantGet(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var t TenantResponse
	err := s.store.StateDB.QueryRow(
		`SELECT id, name, email, status, created_at, updated_at FROM tenants WHERE id = ?`,
		tenantID,
	).Scan(&t.ID, &t.Name, &t.Email, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(t)
}

func (s *Server) handleTenantUpdate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req UpdateTenantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name != nil {
		if len(*req.Name) < 2 || len(*req.Name) > 128 {
			http.Error(w, `{"error":"name must be 2-128 characters"}`, http.StatusBadRequest)
			return
		}
		_, err := s.store.StateDB.Exec(
			`UPDATE tenants SET name = ?, updated_at = ? WHERE id = ?`,
			*req.Name, time.Now().Unix(), tenantID,
		)
		if err != nil {
			http.Error(w, `{"error":"failed to update tenant"}`, http.StatusInternalServerError)
			return
		}
	}

	s.handleTenantGet(w, r)
}

func (s *Server) handleTenantDelete(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	_, err := s.store.StateDB.Exec(
		`UPDATE tenants SET status = 'suspended', updated_at = ? WHERE id = ?`,
		time.Now().Unix(), tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to delete tenant"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
