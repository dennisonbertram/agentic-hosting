// Package environments manages per-agent sandboxed dev environment lifecycle.
package environments

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Environment represents a sandboxed dev environment.
type Environment struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	BaseImage      string `json:"base_image"`
	ContainerID    string `json:"-"`
	VolumeName     string `json:"-"`
	IdleTimeoutSec int    `json:"idle_timeout_sec"`
	LastActivityAt *int64 `json:"last_activity_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a dev environment.
type CreateRequest struct {
	Name           string `json:"name"`
	BaseImage      string `json:"base_image"`
	IdleTimeoutSec int    `json:"idle_timeout_sec,omitempty"`
}

// allowedBaseImages maps user-facing names to actual Docker image tags.
var allowedBaseImages = map[string]string{
	"node:20":      "node:20-bookworm",
	"python:3.12":  "python:3.12-bookworm",
	"golang:1.25":  "golang:1.25-bookworm",
	"ubuntu:24.04": "ubuntu:24.04",
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Manager manages environment lifecycle.
type Manager struct {
	db     *sql.DB
	docker docker.Client
}

// NewManager creates an environment manager.
func NewManager(db *sql.DB, dockerClient docker.Client) *Manager {
	if dockerClient == nil {
		panic("environments: NewManager requires non-nil docker client")
	}
	mgr := &Manager{
		db:     db,
		docker: dockerClient,
	}
	mgr.reconcileStale()
	return mgr
}

// reconcileStale marks environments stuck in "creating" as "failed" on startup.
func (m *Manager) reconcileStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM environments WHERE status = 'creating'`)
	if err != nil {
		log.Printf("environments: reconcile query failed: %v", err)
		return
	}
	defer rows.Close()

	var stale []struct{ id, containerID, volumeName string }
	for rows.Next() {
		var s struct{ id, containerID, volumeName string }
		if err := rows.Scan(&s.id, &s.containerID, &s.volumeName); err != nil {
			continue
		}
		stale = append(stale, s)
	}

	for _, s := range stale {
		log.Printf("environments: reconciling stale environment %s", s.id)
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
		log.Printf("environments: reconciled %d stale environments", len(stale))
	}
}

// Create provisions a new dev environment.
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Environment, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, apierr.Validation("name is required (alphanumeric, max 128 chars)")
	}

	dockerImage, ok := allowedBaseImages[req.BaseImage]
	if !ok {
		allowed := make([]string, 0, len(allowedBaseImages))
		for k := range allowedBaseImages {
			allowed = append(allowed, k)
		}
		return nil, apierr.Validation(fmt.Sprintf("invalid base_image %q; allowed: %s", req.BaseImage, strings.Join(allowed, ", ")))
	}

	if req.IdleTimeoutSec <= 0 {
		req.IdleTimeoutSec = 1800 // 30 minutes default
	}
	if req.IdleTimeoutSec > 86400 {
		return nil, apierr.Validation("idle_timeout_sec must be <= 86400 (24 hours)")
	}

	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, fmt.Errorf("disk check: %w", err)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// Atomic quota check + insert in a single transaction.
	// The INSERT acquires SQLite's write lock, serializing concurrent creates.
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	var maxEnvironments int
	if err := tx.QueryRowContext(ctx,
		`SELECT max_environments FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxEnvironments); err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM environments WHERE tenant_id = ? AND status NOT IN ('failed', 'deleting')`, tenantID,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}
	if count >= maxEnvironments {
		return nil, apierr.QuotaExceeded(fmt.Sprintf("environment quota exceeded (max %d)", maxEnvironments))
	}

	volumeName := fmt.Sprintf("ah-env-%s", id)
	now := time.Now().Unix()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO environments (id, tenant_id, name, status, base_image, container_id, volume_name,
		 idle_timeout_sec, last_activity_at, created_at, updated_at)
		 VALUES (?, ?, ?, 'creating', ?, '', ?, ?, ?, ?, ?)`,
		id, tenantID, req.Name, req.BaseImage, volumeName, req.IdleTimeoutSec, now, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert environment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Create volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Pull image
	if err := m.docker.PullImage(ctx, dockerImage); err != nil {
		log.Printf("environments: pull image %s failed (may use cached): %v", dockerImage, err)
	}

	// Ensure tenant network exists
	if _, err := m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID)); err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	// Run environment container
	containerID, err := m.docker.RunDevEnvironment(ctx, docker.RunDevEnvConfig{
		TenantID:   tenantID,
		EnvID:      id,
		Image:      dockerImage,
		VolumeName: volumeName,
	})
	if err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("run environment: %w", err)
	}

	// Update record
	result, updateErr := m.db.ExecContext(ctx,
		`UPDATE environments SET container_id = ?, status = 'running', updated_at = ? WHERE id = ?`,
		containerID, time.Now().Unix(), id,
	)
	if updateErr != nil || result == nil {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("environment record deleted during creation")
	}
	if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("environment record deleted during creation")
	}

	return &Environment{
		ID:             id,
		TenantID:       tenantID,
		Name:           req.Name,
		Status:         "running",
		BaseImage:      req.BaseImage,
		ContainerID:    containerID,
		VolumeName:     volumeName,
		IdleTimeoutSec: req.IdleTimeoutSec,
		LastActivityAt: &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// Get returns an environment by ID, scoped to tenant.
func (m *Manager) Get(ctx context.Context, tenantID, envID string) (*Environment, error) {
	e := &Environment{}
	var lastActivity sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, base_image, container_id, volume_name,
		 idle_timeout_sec, last_activity_at, last_error, created_at, updated_at
		 FROM environments WHERE id = ? AND tenant_id = ?`,
		envID, tenantID,
	).Scan(&e.ID, &e.TenantID, &e.Name, &e.Status, &e.BaseImage, &e.ContainerID,
		&e.VolumeName, &e.IdleTimeoutSec, &lastActivity, &e.LastError, &e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("environment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	if lastActivity.Valid {
		e.LastActivityAt = &lastActivity.Int64
	}
	return e, nil
}

// List returns all environments for a tenant.
func (m *Manager) List(ctx context.Context, tenantID string) ([]*Environment, error) {
	return m.ListPaginated(ctx, tenantID, 100, 0)
}

// ListPaginated returns environments with limit and offset.
func (m *Manager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*Environment, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, status, base_image, idle_timeout_sec, last_activity_at,
		 last_error, created_at, updated_at
		 FROM environments WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	var result []*Environment
	for rows.Next() {
		e := &Environment{}
		var lastActivity sql.NullInt64
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Name, &e.Status, &e.BaseImage,
			&e.IdleTimeoutSec, &lastActivity, &e.LastError, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		if lastActivity.Valid {
			e.LastActivityAt = &lastActivity.Int64
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// Delete destroys an environment: stops container, removes volume, deletes record.
func (m *Manager) Delete(ctx context.Context, tenantID, envID string) error {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}

	if e.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, e.ContainerID); err != nil {
			log.Printf("environments: stop container %s: %v", e.ContainerID, err)
		}
		if err := m.docker.RemoveContainer(ctx, e.ContainerID); err != nil {
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("failed to remove environment container")
			}
		}
	}

	if e.VolumeName != "" {
		if err := m.docker.RemoveVolume(ctx, e.VolumeName); err != nil {
			if !strings.Contains(err.Error(), "no such volume") &&
				!strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("failed to remove environment volume")
			}
		}
	}

	_, err = m.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ? AND tenant_id = ?`, envID, tenantID)
	if err != nil {
		return fmt.Errorf("delete environment record: %w", err)
	}
	return nil
}

// Stop stops a running environment.
func (m *Manager) Stop(ctx context.Context, tenantID, envID string) error {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "running" {
		return apierr.Conflict("environment is not running")
	}
	if e.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, e.ContainerID); err != nil {
			return fmt.Errorf("stop container: %w", err)
		}
	}
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		time.Now().Unix(), envID, tenantID)
	if err != nil {
		return fmt.Errorf("update stopped: %w", err)
	}
	return nil
}

// Start starts a stopped environment.
func (m *Manager) Start(ctx context.Context, tenantID, envID string) error {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "stopped" {
		return apierr.Conflict("environment is not stopped")
	}
	if e.ContainerID != "" {
		if err := m.docker.StartContainer(ctx, e.ContainerID); err != nil {
			return fmt.Errorf("start container: %w", err)
		}
	}
	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'running', last_activity_at = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		now, now, envID, tenantID)
	if err != nil {
		return fmt.Errorf("update running: %w", err)
	}
	return nil
}

// TouchActivity updates the last_activity_at timestamp.
func (m *Manager) TouchActivity(ctx context.Context, tenantID, envID string) {
	now := time.Now().Unix()
	_, err := m.db.ExecContext(ctx,
		`UPDATE environments SET last_activity_at = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		now, now, envID, tenantID)
	if err != nil {
		log.Printf("environments: touch activity for %s: %v", envID, err)
	}
}

// GetContainerID returns the container ID for a running environment.
func (m *Manager) GetContainerID(ctx context.Context, tenantID, envID string) (string, error) {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return "", err
	}
	if e.Status != "running" {
		return "", apierr.Conflict("environment is not running")
	}
	if e.ContainerID == "" {
		return "", apierr.Conflict("environment has no container")
	}
	return e.ContainerID, nil
}

// GetIdleEnvironments returns environments that have exceeded their idle timeout.
func (m *Manager) GetIdleEnvironments(ctx context.Context) ([]struct{ ID, ContainerID string }, error) {
	now := time.Now().Unix()
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id FROM environments
		 WHERE status = 'running' AND last_activity_at IS NOT NULL
		 AND (? - last_activity_at) > idle_timeout_sec`,
		now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []struct{ ID, ContainerID string }
	for rows.Next() {
		var r struct{ ID, ContainerID string }
		if err := rows.Scan(&r.ID, &r.ContainerID); err != nil {
			continue
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) bool {
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := m.db.ExecContext(freshCtx,
		`UPDATE environments SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("environments: failed to update status for %s: %v", id, err)
		return false
	}
	rowsAffected, _ := result.RowsAffected()
	return rowsAffected > 0
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
