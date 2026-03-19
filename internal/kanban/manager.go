// Package kanban manages per-tenant Vikunja kanban instances.
package kanban

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Instance represents a provisioned Vikunja kanban instance.
type Instance struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Status      string `json:"status"` // provisioning, ready, stopped, failed
	ContainerID string `json:"-"`
	Port        int    `json:"port"`
	URL         string `json:"url"`
	APIToken    string `json:"api_token,omitempty"` // only on create
	VolumeName  string `json:"-"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a kanban instance.
type CreateRequest struct {
	Name string `json:"name"`
}

// Manager manages kanban instance lifecycle.
type Manager struct {
	db         *sql.DB
	docker     docker.Client
	masterKey  []byte
	mu         sync.Mutex // protects port allocation
	baseDomain string     // e.g. "kanban.agentic.hosting"

	// healthCheck is overridable for testing. Defaults to waitForTCP.
	healthCheck func(port int, timeout time.Duration) bool
}

// NewManager creates a kanban manager.
func NewManager(db *sql.DB, dockerClient docker.Client, masterKey []byte, baseDomain string) *Manager {
	if dockerClient == nil {
		panic("kanban: NewManager requires non-nil docker client")
	}
	mgr := &Manager{
		db:          db,
		docker:      dockerClient,
		masterKey:   masterKey,
		baseDomain:  baseDomain,
		healthCheck: waitForTCP,
	}
	mgr.ReconcileStale()
	return mgr
}

// SetHealthCheck overrides the default TCP health check function.
// Intended for testing — allows tests to skip the real TCP probe.
func (m *Manager) SetHealthCheck(fn func(port int, timeout time.Duration) bool) {
	m.healthCheck = fn
}

// ReconcileStale marks kanban instances stuck in "provisioning" as "failed" and
// attempts to clean up their Docker resources. Called on startup.
func (m *Manager) ReconcileStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM kanban_instances WHERE status = 'provisioning'`)
	if err != nil {
		log.Printf("kanban: reconcile query failed: %v", err)
		return
	}
	defer rows.Close()

	var stale []struct{ id, containerID, volumeName string }
	for rows.Next() {
		var s struct{ id, containerID, volumeName string }
		if err := rows.Scan(&s.id, &s.containerID, &s.volumeName); err != nil {
			log.Printf("kanban: reconcile scan failed: %v", err)
			continue
		}
		stale = append(stale, s)
	}

	for _, s := range stale {
		log.Printf("kanban: reconciling stale instance %s", s.id)
		if s.containerID != "" {
			_ = m.docker.StopContainer(ctx, s.containerID)
			_ = m.docker.RemoveContainer(ctx, s.containerID)
		}
		if s.volumeName != "" {
			_ = m.docker.RemoveVolume(ctx, s.volumeName)
		}
		m.updateStatus(ctx, s.id, "failed")
	}
	if len(stale) > 0 {
		log.Printf("kanban: reconciled %d stale instances", len(stale))
	}
}

// Create provisions a new Vikunja kanban instance.
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Instance, error) {
	if req.Name == "" || len(req.Name) > 128 {
		return nil, apierr.Validation("name is required (max 128 chars)")
	}

	// Check disk space before provisioning
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, fmt.Errorf("disk check: %w", err)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// Limit 1 kanban instance per tenant
	var count int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kanban_instances WHERE tenant_id = ? AND status != 'failed'`, tenantID,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("check kanban count: %w", err)
	}
	if count >= 1 {
		return nil, apierr.Conflict("tenant already has an active kanban instance")
	}

	// Generate API token (64 hex chars = 32 random bytes)
	apiToken, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate api token: %w", err)
	}

	// Generate JWT secret for Vikunja
	jwtSecret, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate jwt secret: %w", err)
	}

	volumeName := fmt.Sprintf("ah-kanban-%s", id)
	ctrName := fmt.Sprintf("ah-kanban-%s-%s", tenantID[:8], id[:16])
	url := fmt.Sprintf("http://%s-%s.%s", req.Name, tenantID[:8], m.baseDomain)

	// Encrypt secrets
	apiTokenEnc, err := crypto.Encrypt([]byte(apiToken), m.masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt api token: %w", err)
	}
	jwtSecretEnc, err := crypto.Encrypt([]byte(jwtSecret), m.masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt jwt secret: %w", err)
	}

	// Find free port and insert atomically — retry on UNIQUE constraint violation
	var port int
	now := time.Now().Unix()
	const maxPortRetries = 5
	for attempt := 0; attempt < maxPortRetries; attempt++ {
		port, err = m.findFreePort()
		if err != nil {
			return nil, fmt.Errorf("find free port: %w", err)
		}

		// Insert DB record — UNIQUE index on port prevents race conditions
		_, err = m.db.ExecContext(ctx,
			`INSERT INTO kanban_instances (id, tenant_id, name, status, port, url,
			 api_token_encrypted, jwt_secret_encrypted, volume_name, created_at, updated_at)
			 VALUES (?, ?, ?, 'provisioning', ?, ?, ?, ?, ?, ?, ?)`,
			id, tenantID, req.Name, port, url,
			apiTokenEnc, jwtSecretEnc, volumeName, now, now,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue // retry with different port
			}
			return nil, fmt.Errorf("insert kanban instance: %w", err)
		}
		break // success
	}
	if err != nil {
		return nil, fmt.Errorf("insert kanban instance after %d retries: %w", maxPortRetries, err)
	}

	// Create Docker volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Run Vikunja container
	containerID, err := m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          ctrName,
		Image:         "vikunja/vikunja:latest",
		HostPort:      port,
		ContainerPort: 3456,
		Env: map[string]string{
			"VIKUNJA_SERVICE_JWTSECRET":          jwtSecret,
			"VIKUNJA_SERVICE_FRONTENDURL":        url,
			"VIKUNJA_DATABASE_TYPE":              "sqlite",
			"VIKUNJA_DATABASE_PATH":              "/app/vikunja/vikunja.db",
			"VIKUNJA_SERVICE_ENABLEREGISTRATION": "false",
			"VIKUNJA_MAILER_ENABLED":             "false",
		},
		VolumeName: volumeName,
		MountPath:  "/app/vikunja",
	})
	if err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Update container ID — check rows affected to detect concurrent Delete
	result, updateErr := m.db.ExecContext(ctx,
		`UPDATE kanban_instances SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, time.Now().Unix(), id,
	)
	if updateErr != nil || result == nil {
		log.Printf("kanban: failed to update container_id for %s, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("kanban record deleted during provisioning")
	}
	if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
		log.Printf("kanban: record %s was deleted during provisioning, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("kanban record deleted during provisioning")
	}

	// Wait for TCP health check (port reachable within 60s)
	healthy := m.healthCheck(port, 60*time.Second)

	if !healthy {
		if m.updateStatus(ctx, id, "failed") {
			_ = m.docker.StopContainer(ctx, containerID)
			_ = m.docker.RemoveContainer(ctx, containerID)
			_ = m.docker.RemoveVolume(ctx, volumeName)
		} else {
			_ = m.docker.StopContainer(ctx, containerID)
			_ = m.docker.RemoveContainer(ctx, containerID)
			_ = m.docker.RemoveVolume(ctx, volumeName)
		}
		return nil, fmt.Errorf("kanban health check failed after 60s")
	}

	if !m.updateStatus(ctx, id, "ready") {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("kanban record deleted during provisioning")
	}

	return &Instance{
		ID:        id,
		TenantID:  tenantID,
		Name:      req.Name,
		Status:    "ready",
		Port:      port,
		URL:       url,
		APIToken:  apiToken,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Get returns a kanban instance by ID, scoped to tenant. API token NOT included.
func (m *Manager) Get(ctx context.Context, tenantID, id string) (*Instance, error) {
	inst := &Instance{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, container_id, port, url,
		 volume_name, created_at, updated_at
		 FROM kanban_instances WHERE id = ? AND tenant_id = ?`,
		id, tenantID,
	).Scan(&inst.ID, &inst.TenantID, &inst.Name, &inst.Status, &inst.ContainerID,
		&inst.Port, &inst.URL, &inst.VolumeName, &inst.CreatedAt, &inst.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("kanban instance not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get kanban instance: %w", err)
	}
	return inst, nil
}

// GetByTenant returns the kanban instance for a tenant (there is at most one).
// API token NOT included.
func (m *Manager) GetByTenant(ctx context.Context, tenantID string) (*Instance, error) {
	inst := &Instance{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, container_id, port, url,
		 volume_name, created_at, updated_at
		 FROM kanban_instances WHERE tenant_id = ? AND status != 'failed'
		 ORDER BY created_at DESC LIMIT 1`,
		tenantID,
	).Scan(&inst.ID, &inst.TenantID, &inst.Name, &inst.Status, &inst.ContainerID,
		&inst.Port, &inst.URL, &inst.VolumeName, &inst.CreatedAt, &inst.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("no kanban instance for this tenant")
	}
	if err != nil {
		return nil, fmt.Errorf("get kanban instance by tenant: %w", err)
	}
	return inst, nil
}

// Delete destroys a kanban instance: stops container, removes volume, deletes record.
func (m *Manager) Delete(ctx context.Context, tenantID, id string) error {
	inst, err := m.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}

	// Stop and remove container
	if inst.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, inst.ContainerID); err != nil {
			log.Printf("kanban: stop container %s: %v", inst.ContainerID, err)
		}
		if err := m.docker.RemoveContainer(ctx, inst.ContainerID); err != nil {
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("kanban: remove container %s failed, keeping record: %v", inst.ContainerID, err)
				return fmt.Errorf("failed to remove kanban container")
			}
		}
	}

	// Remove volume
	if inst.VolumeName != "" {
		if err := m.docker.RemoveVolume(ctx, inst.VolumeName); err != nil {
			if !strings.Contains(err.Error(), "no such volume") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("kanban: remove volume %s failed, keeping record: %v", inst.VolumeName, err)
				return fmt.Errorf("failed to remove kanban volume")
			}
		}
	}

	// Delete record only after Docker cleanup succeeded
	_, err = m.db.ExecContext(ctx, `DELETE FROM kanban_instances WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete kanban record: %w", err)
	}

	return nil
}

// GetAPIToken returns the decrypted API token for a kanban instance.
func (m *Manager) GetAPIToken(ctx context.Context, tenantID, id string) (string, error) {
	var apiTokenEnc string
	err := m.db.QueryRowContext(ctx,
		`SELECT api_token_encrypted FROM kanban_instances WHERE id = ? AND tenant_id = ?`,
		id, tenantID,
	).Scan(&apiTokenEnc)
	if err == sql.ErrNoRows {
		return "", apierr.NotFound("kanban instance not found")
	}
	if err != nil {
		return "", fmt.Errorf("get api token: %w", err)
	}
	plaintext, err := crypto.Decrypt(apiTokenEnc, m.masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt api token: %w", err)
	}
	return string(plaintext), nil
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) bool {
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := m.db.ExecContext(freshCtx,
		`UPDATE kanban_instances SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("kanban: failed to update status for %s: %v", id, err)
		return false
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("kanban: record %s was deleted, status update skipped", id)
		return false
	}
	return true
}

func (m *Manager) findFreePort() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	const minPort, maxPort = 9000, 9500

	rows, err := m.db.Query(`SELECT port FROM kanban_instances WHERE port IS NOT NULL AND status NOT IN ('failed')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	usedPorts := make(map[int]bool)
	for rows.Next() {
		var p int
		rows.Scan(&p)
		usedPorts[p] = true
	}

	for port := minPort; port <= maxPort; port++ {
		if usedPorts[port] {
			continue
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", minPort, maxPort)
}

func waitForTCP(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("random hex: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
