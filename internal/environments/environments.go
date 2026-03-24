// Package environments manages sandboxed dev environment lifecycle.
package environments

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// namePattern matches valid environment names: starts with a letter, contains
// only alphanumeric characters, hyphens, and underscores.
var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// Environment represents a sandboxed dev environment.
type Environment struct {
	ID                   string `json:"id"`
	TenantID             string `json:"tenant_id"`
	Name                 string `json:"name"`
	TemplateID           string `json:"template_id"`
	Status               string `json:"status"`
	ContainerID          string `json:"container_id,omitempty"`
	VolumeName           string `json:"-"`
	LeaseExpiresAt       *int64 `json:"lease_expires_at,omitempty"`
	LeaseDurationSeconds int    `json:"lease_duration_seconds"`
	LastActivityAt       *int64 `json:"last_activity_at,omitempty"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

// Template describes a pre-configured environment type.
type Template struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseImage    string `json:"base_image"`
	Description  string `json:"description"`
	MemoryMB     int    `json:"memory_mb"`
	CPUMillis    int    `json:"cpu_millicores"`
	DiskMB       int    `json:"disk_mb"`
	EgressPolicy string `json:"egress_policy"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating an environment.
type CreateRequest struct {
	Name                 string `json:"name"`
	TemplateID           string `json:"template_id,omitempty"`
	LeaseDurationSeconds *int   `json:"lease_duration_seconds,omitempty"`
}

// ExecRequest holds parameters for executing a command in an environment.
type ExecRequest struct {
	Command []string `json:"command"`
	WorkDir string   `json:"work_dir,omitempty"`
	Timeout int      `json:"timeout,omitempty"` // seconds, max 300, default 60
}

// ExecResponse holds the result of a command execution.
type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	DurationMs int64  `json:"duration_ms"`
}

// SyncRequest holds parameters for syncing code into an environment.
type SyncRequest struct {
	GitURL string `json:"git_url,omitempty"`
	GitRef string `json:"git_ref,omitempty"` // branch/tag/commit, default "HEAD"
}

// Preview represents a preview route exposing an environment port via Traefik.
type Preview struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environment_id"`
	TenantID      string `json:"tenant_id"`
	Name          string `json:"name"`
	Port          int    `json:"port"`
	DNSLabel      string `json:"dns_label"`
	URL           string `json:"url,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// CreatePreviewRequest holds parameters for creating a preview route.
type CreatePreviewRequest struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// previewNamePattern matches valid preview names: 1-63 chars, alphanumeric + hyphens.
var previewNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// Manager manages environment lifecycle.
type Manager struct {
	db               *sql.DB
	docker           docker.Client
	pool             *PoolManager
	baseDomain       string
	traefikConfigDir string
	mu               sync.Mutex // for state transitions
}

// NewManager creates an environment manager.
func NewManager(db *sql.DB, docker docker.Client, baseDomain, traefikConfigDir string) *Manager {
	if docker == nil {
		panic("environments: NewManager requires non-nil docker client")
	}
	return &Manager{
		db:               db,
		docker:           docker,
		baseDomain:       baseDomain,
		traefikConfigDir: traefikConfigDir,
	}
}

// SetPool sets the warm pool manager for faster environment creation.
func (m *Manager) SetPool(p *PoolManager) {
	m.pool = p
}

// Create provisions a new environment.
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Environment, error) {
	// Validate name
	if err := validateName(req.Name); err != nil {
		return nil, err
	}

	// Default template
	if req.TemplateID == "" {
		req.TemplateID = "default"
	}

	// Validate template exists
	tmpl, err := m.GetTemplate(ctx, req.TemplateID)
	if err != nil {
		return nil, err
	}

	// Default and validate lease duration
	leaseDuration := 3600
	if req.LeaseDurationSeconds != nil {
		leaseDuration = *req.LeaseDurationSeconds
	}
	if leaseDuration < 300 || leaseDuration > 86400 {
		return nil, apierr.Validation("lease_duration_seconds must be between 300 and 86400")
	}

	// Generate ID
	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// Check quota inside an IMMEDIATE transaction to prevent concurrent creates
	// from both seeing count below the tenant limit.
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Force write lock with a dummy write (SQLite IMMEDIATE)
	_, _ = tx.ExecContext(ctx, `UPDATE environments SET updated_at = updated_at WHERE id = 'lock'`)
	var maxEnvironments int
	if err := tx.QueryRowContext(ctx,
		`SELECT max_environments FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxEnvironments); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("check quota: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM environments WHERE tenant_id = ? AND status NOT IN ('failed')`, tenantID,
	).Scan(&count); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("check quota: %w", err)
	}
	if count >= maxEnvironments {
		tx.Rollback()
		return nil, apierr.QuotaExceeded(fmt.Sprintf("quota exceeded: maximum %d environments reached", maxEnvironments))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit quota check: %w", err)
	}

	now := time.Now().Unix()
	volumeName := fmt.Sprintf("ah-env-%s", id)

	// Insert DB row with status=creating
	_, err = m.db.ExecContext(ctx,
		`INSERT INTO environments (id, tenant_id, name, template_id, status, volume_name,
		 lease_duration_seconds, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'creating', ?, ?, ?, ?)`,
		id, tenantID, req.Name, req.TemplateID, volumeName, leaseDuration, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, apierr.Conflict(fmt.Sprintf("environment with name %q already exists", req.Name))
		}
		return nil, fmt.Errorf("insert environment: %w", err)
	}

	// Create volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Ensure tenant network
	tenantNet := docker.TenantNetworkName(tenantID)
	if _, err := m.docker.EnsureNetwork(ctx, tenantNet); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	// Determine egress policy from template
	egressAllow := tmpl.EgressPolicy == "allow"

	// Run environment container
	containerID, err := m.docker.RunEnvironment(ctx, docker.RunEnvironmentConfig{
		TenantID:    tenantID,
		EnvID:       id,
		Image:       tmpl.BaseImage,
		MemoryMB:    int64(tmpl.MemoryMB),
		CPUMillis:   int64(tmpl.CPUMillis),
		VolumeName:  volumeName,
		NetworkName: tenantNet,
		Labels:      map[string]string{},
		EgressAllow: egressAllow,
	})
	if err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("run environment: %w", err)
	}

	// Update DB with container_id, status=running, lease
	leaseExpiresAt := now + int64(leaseDuration)
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET container_id = ?, status = 'running',
		 lease_expires_at = ?, last_activity_at = ?, updated_at = ?
		 WHERE id = ?`,
		containerID, leaseExpiresAt, now, now, id,
	)
	if err != nil {
		log.Printf("environments: failed to update container_id for %s: %v", id, err)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("update environment: %w", err)
	}

	return &Environment{
		ID:                   id,
		TenantID:             tenantID,
		Name:                 req.Name,
		TemplateID:           req.TemplateID,
		Status:               "running",
		ContainerID:          containerID,
		VolumeName:           volumeName,
		LeaseExpiresAt:       &leaseExpiresAt,
		LeaseDurationSeconds: leaseDuration,
		LastActivityAt:       &now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}, nil
}

// Get returns an environment by ID, scoped to tenant.
func (m *Manager) Get(ctx context.Context, tenantID, envID string) (*Environment, error) {
	e := &Environment{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, template_id, status, container_id, volume_name,
		 lease_expires_at, lease_duration_seconds, last_activity_at, created_at, updated_at
		 FROM environments WHERE id = ? AND tenant_id = ?`,
		envID, tenantID,
	).Scan(&e.ID, &e.TenantID, &e.Name, &e.TemplateID, &e.Status, &e.ContainerID,
		&e.VolumeName, &e.LeaseExpiresAt, &e.LeaseDurationSeconds, &e.LastActivityAt,
		&e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("environment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	return e, nil
}

// List returns environments for a tenant with pagination.
func (m *Manager) List(ctx context.Context, tenantID string, limit, offset int) ([]*Environment, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, template_id, status, container_id, volume_name,
		 lease_expires_at, lease_duration_seconds, last_activity_at, created_at, updated_at
		 FROM environments WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	result := make([]*Environment, 0)
	for rows.Next() {
		e := &Environment{}
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Name, &e.TemplateID, &e.Status,
			&e.ContainerID, &e.VolumeName, &e.LeaseExpiresAt, &e.LeaseDurationSeconds,
			&e.LastActivityAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
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

	// Clean up preview Traefik route files before DB cascade deletes the rows
	m.cleanupPreviews(ctx, envID)

	// Stop and remove container (ignore not-found errors)
	if e.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, e.ContainerID); err != nil {
			if !isNotFoundError(err) {
				log.Printf("environments: stop container %s: %v", e.ContainerID, err)
			}
		}
		if err := m.docker.RemoveContainer(ctx, e.ContainerID); err != nil {
			if !isNotFoundError(err) {
				log.Printf("environments: remove container %s: %v", e.ContainerID, err)
			}
		}
	}

	// Remove volume
	if e.VolumeName != "" {
		if err := m.docker.RemoveVolume(ctx, e.VolumeName); err != nil {
			if !isNotFoundError(err) {
				log.Printf("environments: remove volume %s: %v", e.VolumeName, err)
			}
		}
	}

	// Delete record
	_, err = m.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ? AND tenant_id = ?`, envID, tenantID)
	if err != nil {
		return fmt.Errorf("delete environment record: %w", err)
	}
	return nil
}

// Start starts a stopped environment.
func (m *Manager) Start(ctx context.Context, tenantID, envID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "stopped" {
		return apierr.Validation(fmt.Sprintf("environment must be stopped to start (current: %s)", e.Status))
	}

	if err := m.docker.StartContainer(ctx, e.ContainerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	now := time.Now().Unix()
	leaseExpiresAt := now + int64(e.LeaseDurationSeconds)
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'running', lease_expires_at = ?,
		 last_activity_at = ?, updated_at = ? WHERE id = ?`,
		leaseExpiresAt, now, now, envID,
	)
	return err
}

// Stop stops a running environment.
func (m *Manager) Stop(ctx context.Context, tenantID, envID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "running" {
		return apierr.Validation(fmt.Sprintf("environment must be running to stop (current: %s)", e.Status))
	}

	if err := m.docker.StopContainer(ctx, e.ContainerID); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}

	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ?`,
		now, envID,
	)
	return err
}

// Exec executes a command in a running environment.
func (m *Manager) Exec(ctx context.Context, tenantID, envID string, req ExecRequest) (*ExecResponse, error) {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return nil, err
	}
	if e.Status != "running" {
		return nil, apierr.Validation(fmt.Sprintf("environment must be running to exec (current: %s)", e.Status))
	}

	// Validate command
	if len(req.Command) == 0 {
		return nil, apierr.Validation("command is required")
	}

	// Set defaults
	if req.WorkDir == "" {
		req.WorkDir = "/workspace"
	}
	if req.Timeout <= 0 {
		req.Timeout = 60
	}
	if req.Timeout > 300 {
		req.Timeout = 300
	}

	// Create exec
	execID, err := m.docker.ExecCreate(ctx, e.ContainerID, req.Command, req.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	// Run exec with timeout
	start := time.Now()
	timeout := time.Duration(req.Timeout) * time.Second
	stdout, stderr, exitCode, err := m.docker.ExecRun(ctx, execID, timeout)

	durationMs := time.Since(start).Milliseconds()

	timedOut := false
	if err != nil && ctx.Err() != nil {
		timedOut = true
	}
	// If the exec timed out but we still got partial output, return it
	if err != nil && !timedOut {
		return nil, fmt.Errorf("exec run: %w", err)
	}

	// Update last_activity_at
	now := time.Now().Unix()
	_, _ = m.db.ExecContext(ctx,
		`UPDATE environments SET last_activity_at = ?, updated_at = ? WHERE id = ?`,
		now, now, envID,
	)

	// Check truncation
	truncated := len(stdout) >= 1024*1024 || len(stderr) >= 1024*1024

	return &ExecResponse{
		ExitCode:   exitCode,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		Truncated:  truncated,
		TimedOut:   timedOut,
		DurationMs: durationMs,
	}, nil
}

// ExtendLease extends the lease of a running environment.
func (m *Manager) ExtendLease(ctx context.Context, tenantID, envID string, durationSec int) error {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "running" {
		return apierr.Validation(fmt.Sprintf("environment must be running to extend lease (current: %s)", e.Status))
	}
	if durationSec < 300 || durationSec > 86400 {
		return apierr.Validation("lease duration must be between 300 and 86400 seconds")
	}

	now := time.Now().Unix()
	leaseExpiresAt := now + int64(durationSec)
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		leaseExpiresAt, now, envID,
	)
	return err
}

// ReconcileStale cleans up stuck or expired environments.
func (m *Manager) ReconcileStale(ctx context.Context) error {
	now := time.Now().Unix()
	fiveMinAgo := now - 300

	// Mark stuck "creating" > 5min as "failed"
	_, err := m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'failed', updated_at = ?
		 WHERE status = 'creating' AND created_at < ?`,
		now, fiveMinAgo,
	)
	if err != nil {
		return fmt.Errorf("reconcile creating: %w", err)
	}

	// Check running envs — if container missing, mark "stopped"
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id FROM environments WHERE status = 'running'`)
	if err != nil {
		return fmt.Errorf("reconcile running query: %w", err)
	}
	defer rows.Close()

	var running []struct{ id, containerID string }
	for rows.Next() {
		var r struct{ id, containerID string }
		if err := rows.Scan(&r.id, &r.containerID); err != nil {
			log.Printf("environments: reconcile scan: %v", err)
			continue
		}
		running = append(running, r)
	}
	rows.Close()

	for _, r := range running {
		if r.containerID != "" {
			_, err := m.docker.InspectContainer(ctx, r.containerID)
			if err != nil {
				log.Printf("environments: container %s missing for env %s, marking stopped", r.containerID, r.id)
				m.updateStatus(ctx, r.id, "stopped")
			}
		}
	}

	// Expire leases: running envs past lease_expires_at -> stop
	expiredRows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id FROM environments
		 WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		now,
	)
	if err != nil {
		return fmt.Errorf("reconcile expired query: %w", err)
	}
	defer expiredRows.Close()

	var expired []struct{ id, containerID string }
	for expiredRows.Next() {
		var e struct{ id, containerID string }
		if err := expiredRows.Scan(&e.id, &e.containerID); err != nil {
			log.Printf("environments: reconcile expired scan: %v", err)
			continue
		}
		expired = append(expired, e)
	}
	expiredRows.Close()

	for _, e := range expired {
		log.Printf("environments: lease expired for env %s, stopping", e.id)
		if e.containerID != "" {
			_ = m.docker.StopContainer(ctx, e.containerID)
		}
		m.updateStatus(ctx, e.id, "stopped")
	}

	return nil
}

// GetTemplate returns a template by ID.
func (m *Manager) GetTemplate(ctx context.Context, templateID string) (*Template, error) {
	t := &Template{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, name, base_image, description, memory_mb, cpu_millicores, disk_mb,
		 egress_policy, created_at, updated_at
		 FROM environment_templates WHERE id = ?`,
		templateID,
	).Scan(&t.ID, &t.Name, &t.BaseImage, &t.Description, &t.MemoryMB, &t.CPUMillis,
		&t.DiskMB, &t.EgressPolicy, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		// Also try by name
		err = m.db.QueryRowContext(ctx,
			`SELECT id, name, base_image, description, memory_mb, cpu_millicores, disk_mb,
			 egress_policy, created_at, updated_at
			 FROM environment_templates WHERE name = ?`,
			templateID,
		).Scan(&t.ID, &t.Name, &t.BaseImage, &t.Description, &t.MemoryMB, &t.CPUMillis,
			&t.DiskMB, &t.EgressPolicy, &t.CreatedAt, &t.UpdatedAt)
	}
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound(fmt.Sprintf("template %q not found", templateID))
	}
	if err != nil {
		return nil, fmt.Errorf("get template: %w", err)
	}
	return t, nil
}

// ListTemplates returns all available templates.
func (m *Manager) ListTemplates(ctx context.Context) ([]*Template, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, name, base_image, description, memory_mb, cpu_millicores, disk_mb,
		 egress_policy, created_at, updated_at
		 FROM environment_templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()

	result := make([]*Template, 0)
	for rows.Next() {
		t := &Template{}
		if err := rows.Scan(&t.ID, &t.Name, &t.BaseImage, &t.Description, &t.MemoryMB,
			&t.CPUMillis, &t.DiskMB, &t.EgressPolicy, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// validateName checks that an environment name is safe.
func validateName(name string) error {
	if name == "" {
		return apierr.Validation("environment name is required")
	}
	if len(name) > 63 {
		return apierr.Validation("environment name must be 63 characters or fewer")
	}
	if !namePattern.MatchString(name) {
		return apierr.Validation("environment name must start with a letter and contain only alphanumeric characters, hyphens, and underscores")
	}
	return nil
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) {
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(freshCtx,
		`UPDATE environments SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("environments: failed to update status for %s: %v", id, err)
	}
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// isNotFoundError returns true if the error indicates a resource doesn't exist.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such volume") ||
		strings.Contains(msg, "404")
}

// ---------------------------------------------------------------------------
// Workspace Sync
// ---------------------------------------------------------------------------

// SyncWorkspace syncs code into an environment's /workspace directory via git.
func (m *Manager) SyncWorkspace(ctx context.Context, tenantID, envID string, req SyncRequest) error {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	if e.Status != "running" {
		return apierr.Validation(fmt.Sprintf("environment must be running to sync (current: %s)", e.Status))
	}

	if req.GitURL == "" {
		return apierr.Validation("git_url is required")
	}
	if !strings.HasPrefix(req.GitURL, "https://") {
		return apierr.Validation("git_url must start with https://")
	}

	ref := req.GitRef
	if ref == "" {
		ref = "HEAD"
	}

	// Build the git clone/pull command to run inside the container.
	// If /workspace/.git exists, fetch and checkout; otherwise clone fresh.
	gitCmd := fmt.Sprintf(
		`cd /workspace && if [ -d .git ]; then git fetch origin && git checkout %s; else git clone --depth=1 %s .; fi`,
		ref, req.GitURL,
	)

	// Create and run exec with a 5-minute timeout
	execID, err := m.docker.ExecCreate(ctx, e.ContainerID, []string{"sh", "-c", gitCmd}, "/workspace")
	if err != nil {
		return fmt.Errorf("sync exec create: %w", err)
	}

	_, _, exitCode, err := m.docker.ExecRun(ctx, execID, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("sync exec run: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("git sync failed with exit code %d", exitCode)
	}

	// Update last_activity_at
	now := time.Now().Unix()
	_, _ = m.db.ExecContext(ctx,
		`UPDATE environments SET last_activity_at = ?, updated_at = ? WHERE id = ?`,
		now, now, envID,
	)

	return nil
}

// ---------------------------------------------------------------------------
// Preview Routing
// ---------------------------------------------------------------------------

// CreatePreview creates a preview route for an environment port.
func (m *Manager) CreatePreview(ctx context.Context, tenantID, envID string, req CreatePreviewRequest) (*Preview, error) {
	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return nil, err
	}
	if e.Status != "running" {
		return nil, apierr.Validation(fmt.Sprintf("environment must be running to create preview (current: %s)", e.Status))
	}

	// Validate name
	if !previewNamePattern.MatchString(req.Name) {
		return nil, apierr.Validation("preview name must be 1-63 characters, alphanumeric and hyphens, starting with alphanumeric")
	}

	// Validate port
	if req.Port < 1 || req.Port > 65535 {
		return nil, apierr.Validation("port must be between 1 and 65535")
	}

	// Generate ID and DNS label
	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// DNS label: env-{first8(envID)}-{name}
	envPrefix := envID
	if len(envPrefix) > 8 {
		envPrefix = envPrefix[:8]
	}
	dnsLabel := fmt.Sprintf("env-%s-%s", envPrefix, req.Name)

	now := time.Now().Unix()

	// Insert DB row
	_, err = m.db.ExecContext(ctx,
		`INSERT INTO environment_previews (id, environment_id, tenant_id, name, port, dns_label, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, envID, tenantID, req.Name, req.Port, dnsLabel, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, apierr.Conflict(fmt.Sprintf("preview with name %q already exists for this environment", req.Name))
		}
		return nil, fmt.Errorf("insert preview: %w", err)
	}

	// Write Traefik route
	containerName := fmt.Sprintf("ah-env-%s-%s", tenantID, envID)
	if err := m.writePreviewTraefikRoute(id, dnsLabel, containerName, req.Port); err != nil {
		// Rollback DB on Traefik write failure
		_, _ = m.db.ExecContext(ctx, `DELETE FROM environment_previews WHERE id = ?`, id)
		return nil, fmt.Errorf("write traefik route: %w", err)
	}

	url := ""
	if m.baseDomain != "" {
		url = fmt.Sprintf("https://%s.%s", dnsLabel, m.baseDomain)
	}

	return &Preview{
		ID:            id,
		EnvironmentID: envID,
		TenantID:      tenantID,
		Name:          req.Name,
		Port:          req.Port,
		DNSLabel:      dnsLabel,
		URL:           url,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// ListPreviews returns all preview routes for an environment.
func (m *Manager) ListPreviews(ctx context.Context, tenantID, envID string) ([]*Preview, error) {
	// Verify environment belongs to tenant
	if _, err := m.Get(ctx, tenantID, envID); err != nil {
		return nil, err
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, environment_id, tenant_id, name, port, dns_label, created_at, updated_at
		 FROM environment_previews WHERE environment_id = ? AND tenant_id = ?
		 ORDER BY created_at DESC`,
		envID, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list previews: %w", err)
	}
	defer rows.Close()

	result := make([]*Preview, 0)
	for rows.Next() {
		p := &Preview{}
		if err := rows.Scan(&p.ID, &p.EnvironmentID, &p.TenantID, &p.Name,
			&p.Port, &p.DNSLabel, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan preview: %w", err)
		}
		if m.baseDomain != "" {
			p.URL = fmt.Sprintf("https://%s.%s", p.DNSLabel, m.baseDomain)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// DeletePreview removes a preview route.
func (m *Manager) DeletePreview(ctx context.Context, tenantID, envID, previewID string) error {
	// Verify the preview exists and belongs to the right tenant/env
	var id string
	err := m.db.QueryRowContext(ctx,
		`SELECT id FROM environment_previews WHERE id = ? AND environment_id = ? AND tenant_id = ?`,
		previewID, envID, tenantID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return apierr.NotFound("preview not found")
	}
	if err != nil {
		return fmt.Errorf("get preview: %w", err)
	}

	// Delete Traefik config file
	m.deletePreviewTraefikRoute(previewID)

	// Delete DB row
	_, err = m.db.ExecContext(ctx, `DELETE FROM environment_previews WHERE id = ?`, previewID)
	if err != nil {
		return fmt.Errorf("delete preview: %w", err)
	}
	return nil
}

// writePreviewTraefikRoute writes a Traefik dynamic config file for a preview.
func (m *Manager) writePreviewTraefikRoute(previewID, dnsLabel, containerName string, port int) error {
	if m.traefikConfigDir == "" {
		return nil
	}

	var content string
	if m.baseDomain != "" {
		host := fmt.Sprintf("%s.%s", dnsLabel, m.baseDomain)
		content = fmt.Sprintf(`http:
  routers:
    env-preview-%s:
      rule: "Host(`+"`%s`"+`)"
      service: "env-preview-%s"
      entryPoints:
        - websecure
      tls:
        certResolver: letsencrypt
  services:
    env-preview-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%d"
`, previewID, host, previewID, previewID, containerName, port)
	} else {
		// Localhost mode
		content = fmt.Sprintf(`http:
  routers:
    env-preview-%s:
      rule: "Host(`+"`%s.localhost`"+`)"
      service: "env-preview-%s"
      entryPoints:
        - web
  services:
    env-preview-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%d"
`, previewID, dnsLabel, previewID, previewID, containerName, port)
	}

	path := filepath.Join(m.traefikConfigDir, "env-preview-"+previewID+".yml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0640); err != nil {
		return fmt.Errorf("write traefik route tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename traefik route %s: %w", path, err)
	}
	return nil
}

// deletePreviewTraefikRoute removes the Traefik config file for a preview.
func (m *Manager) deletePreviewTraefikRoute(previewID string) {
	if m.traefikConfigDir == "" {
		return
	}
	path := filepath.Join(m.traefikConfigDir, "env-preview-"+previewID+".yml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("environments: failed to remove traefik route %s: %v", path, err)
	}
}

// cleanupPreviews deletes all preview Traefik route files for an environment
// and removes the DB rows (CASCADE handles DB, but we also need file cleanup).
func (m *Manager) cleanupPreviews(ctx context.Context, envID string) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id FROM environment_previews WHERE environment_id = ?`, envID)
	if err != nil {
		log.Printf("environments: cleanup previews query: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var previewID string
		if err := rows.Scan(&previewID); err != nil {
			continue
		}
		m.deletePreviewTraefikRoute(previewID)
	}
}
