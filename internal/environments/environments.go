// Package environments manages sandboxed dev environment lifecycle.
package environments

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"errors"
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
var previewNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,49}$`)

// maxPreviewsPerEnv caps previews per environment; each gets its own TLS host.
const maxPreviewsPerEnv = 5

// Manager manages environment lifecycle.
type Manager struct {
	db                    *sql.DB
	docker                docker.Client
	pool                  *PoolManager
	baseDomain            string
	externalTLSTerminated bool
	traefikConfigDir      string
	mu                    sync.Mutex // for state transitions
}

// SetExternalTLSTerminated configures preview routes for deployments where a
// trusted upstream proxy terminates public TLS before traffic reaches Traefik.
// Enabling it assumes public requests reach these routes only through that
// configured proxy; the route middleware then sets X-Forwarded-Proto to https.
func (m *Manager) SetExternalTLSTerminated(enabled bool) {
	m.externalTLSTerminated = enabled
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

	var maxEnvironments int
	if err := m.db.QueryRowContext(ctx,
		`SELECT max_environments FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxEnvironments); err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}

	now := time.Now().Unix()
	volumeName := fmt.Sprintf("ah-env-%s", id)

	// Insert DB row with status=creating
	// The quota count is part of the INSERT so concurrent creates cannot both
	// pass the check. Nothing is claimed from the warm pool until this succeeds.
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO environments (id, tenant_id, name, template_id, status, volume_name,
		 lease_duration_seconds, created_at, updated_at)
		 SELECT ?, ?, ?, ?, 'creating', ?, ?, ?, ?
		 WHERE (SELECT COUNT(*) FROM environments WHERE tenant_id = ? AND status NOT IN ('failed')) < ?`,
		id, tenantID, req.Name, req.TemplateID, volumeName, leaseDuration, now, now, tenantID, maxEnvironments,
	)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, apierr.QuotaExceeded(fmt.Sprintf("quota exceeded: maximum %d environments reached", maxEnvironments))
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, apierr.Conflict(fmt.Sprintf("environment with name %q already exists", req.Name))
		}
		return nil, fmt.Errorf("insert environment: %w", err)
	}

	// Ensure tenant network (needed for both warm and cold paths).
	tenantNet := docker.TenantNetworkName(tenantID)
	if _, err := m.docker.EnsureNetwork(ctx, tenantNet); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	// Try the warm pool first — skip volume creation and RunEnvironment if we
	// get a pre-warmed container.
	var containerID string
	usedPool := false
	if m.pool != nil {
		poolContainerID, poolVolumeName, poolErr := m.pool.AcquireForEnvironment(ctx, tmpl.ID, tenantID, id)
		if poolErr == nil {
			log.Printf("environments: using warm container %s for env %s", poolContainerID[:min(12, len(poolContainerID))], id)
			transferErr := m.docker.NetworkDisconnect(ctx, "ah-pool", poolContainerID)
			if transferErr == nil {
				transferErr = m.docker.ConnectNetwork(ctx, tenantNet, poolContainerID)
			}
			if transferErr == nil {
				transferErr = m.docker.RenameContainer(ctx, poolContainerID,
					fmt.Sprintf("ah-env-%s-%s", tenantID, id))
			}
			if transferErr != nil {
				log.Printf("environments: warm container transfer failed, falling back to cold start: %v", transferErr)
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				_ = m.docker.StopContainer(cleanupCtx, poolContainerID)
				_ = m.docker.RemoveContainer(cleanupCtx, poolContainerID)
				_ = m.docker.RemoveVolume(cleanupCtx, poolVolumeName)
				_, _ = m.db.ExecContext(cleanupCtx, `DELETE FROM warm_pool WHERE container_id = ?`, poolContainerID)
				_, resetErr := m.db.ExecContext(cleanupCtx,
					`UPDATE environments SET container_id = '', volume_name = ? WHERE id = ?`, volumeName, id)
				cancel()
				if resetErr != nil {
					m.updateStatus(ctx, id, "failed")
					return nil, fmt.Errorf("reset environment after warm transfer failure: %w", resetErr)
				}
			} else {
				volumeName = poolVolumeName
				containerID = poolContainerID
				usedPool = true
			}
		}
		// If pool is empty (ErrPoolEmpty) or any other error, fall through to cold start.
	}

	if !usedPool {
		// Cold-start path: create a new volume and run a fresh container.
		if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
			m.updateStatus(ctx, id, "failed")
			return nil, fmt.Errorf("create volume: %w", err)
		}

		// Determine egress policy from template
		egressAllow := tmpl.EgressPolicy == "allow"

		containerID, err = m.docker.RunEnvironment(ctx, docker.RunEnvironmentConfig{
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
	}

	// Update DB with container_id, status=running, lease. The update only
	// applies while the row is still 'creating' and the tenant is active, so a
	// suspension that raced with this Create (StopAllForTenant skips rows with
	// no container yet) cannot leave a running container behind.
	leaseExpiresAt := now + int64(leaseDuration)
	var updateResult sql.Result
	updateResult, err = m.db.ExecContext(ctx,
		`UPDATE environments SET container_id = ?, status = 'running',
		 lease_expires_at = ?, last_activity_at = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND status = 'creating'
		 AND EXISTS (SELECT 1 FROM tenants WHERE tenants.id = environments.tenant_id AND tenants.status = 'active')`,
		containerID, leaseExpiresAt, now, now, id, tenantID,
	)
	if err != nil {
		log.Printf("environments: failed to update container_id for %s: %v", id, err)
		m.removeCreatedEnvironment(ctx, containerID, volumeName)
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("update environment: %w", err)
	}
	updated, err := updateResult.RowsAffected()
	if err != nil {
		m.removeCreatedEnvironment(ctx, containerID, volumeName)
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("check environment creation update: %w", err)
	}
	if updated != 1 {
		m.removeCreatedEnvironment(ctx, containerID, volumeName)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		// Tenant suspension marks in-flight rows 'stopped' without a container;
		// that row is stale now that its storage is gone, so delete it. A row
		// still 'creating' (tenant inactive, no suspension sweep) is marked failed.
		_, deleteErr := m.db.ExecContext(cleanupCtx,
			`DELETE FROM environments WHERE id = ? AND tenant_id = ? AND status = 'stopped'`, id, tenantID)
		cancel()
		if deleteErr != nil {
			log.Printf("environments: remove interrupted creation %s: %v", id, deleteErr)
		}
		m.updateStatus(ctx, id, "failed")
		return nil, apierr.Validation("environment creation was interrupted: tenant is not active")
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

// environmentColumns is the column list scanned by scanEnvironment.
const environmentColumns = `id, tenant_id, name, template_id, status, container_id, volume_name,
	 lease_expires_at, lease_duration_seconds, last_activity_at, created_at, updated_at`

func scanEnvironment(row interface{ Scan(...any) error }) (*Environment, error) {
	e := &Environment{}
	err := row.Scan(&e.ID, &e.TenantID, &e.Name, &e.TemplateID, &e.Status, &e.ContainerID,
		&e.VolumeName, &e.LeaseExpiresAt, &e.LeaseDurationSeconds, &e.LastActivityAt,
		&e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// Get returns an environment by ID, scoped to tenant.
func (m *Manager) Get(ctx context.Context, tenantID, envID string) (*Environment, error) {
	e, err := scanEnvironment(m.db.QueryRowContext(ctx,
		`SELECT `+environmentColumns+` FROM environments WHERE id = ? AND tenant_id = ?`,
		envID, tenantID))
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
		`SELECT `+environmentColumns+` FROM environments WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	result := make([]*Environment, 0)
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// Delete destroys an environment: stops container, removes volume, deletes record.
func (m *Manager) Delete(ctx context.Context, tenantID, envID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
	if err := m.removePreviewIngressNetwork(ctx, tenantID, envID, e.ContainerID); err != nil {
		log.Printf("environments: remove preview ingress network for %s: %v", envID, err)
	}

	// A claimed pool container keeps its warm_pool row (for GC) until now.
	if e.ContainerID != "" {
		_, _ = m.db.ExecContext(ctx, `DELETE FROM warm_pool WHERE container_id = ?`, e.ContainerID)
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
	needsIngress, err := m.hasPreviews(ctx, tenantID, envID)
	if err != nil {
		return err
	}

	// Tenant suspension removes the isolated tenant bridge. Recreate it for the
	// environment container; preview ingress uses a separate per-environment net.
	networkName := docker.TenantNetworkName(tenantID)
	if _, err := m.docker.EnsureNetwork(ctx, networkName); err != nil {
		return fmt.Errorf("ensure environment network: %w", err)
	}

	// Push the lease forward before starting the container. The reconciler's
	// retry pass stops running containers of 'stopped' rows whose lease has
	// expired; without this it could stop the container we are starting here
	// before the final UPDATE commits 'running'.
	{
		now := time.Now().Unix()
		hold := int64(e.LeaseDurationSeconds)
		if hold < 300 {
			hold = 300
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE environments SET lease_expires_at = ?, updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'stopped'`,
			now+hold, now, envID, tenantID); err != nil {
			return fmt.Errorf("extend lease before start: %w", err)
		}
	}

	containerID := e.ContainerID
	recreated := false
	if err := m.docker.StartContainer(ctx, e.ContainerID); err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("start container: %w", err)
		}
		// The container may have been removed while its named workspace volume
		// survived. Recreate it from the same template and keep that volume.
		if e.VolumeName == "" {
			return fmt.Errorf("workspace volume is missing; refusing to recreate an empty environment")
		}
		volumes, volumeErr := m.docker.ListVolumes(ctx, e.VolumeName)
		if volumeErr != nil {
			return fmt.Errorf("check workspace volume for recovery: %w", volumeErr)
		}
		volumeFound := false
		for _, name := range volumes {
			if name == e.VolumeName {
				volumeFound = true
				break
			}
		}
		if !volumeFound {
			return fmt.Errorf("workspace volume %q is missing; refusing to recreate an empty environment", e.VolumeName)
		}
		tmpl, tmplErr := m.GetTemplate(ctx, e.TemplateID)
		if tmplErr != nil {
			return fmt.Errorf("load environment template for recovery: %w", tmplErr)
		}
		containerID, err = m.docker.RunEnvironment(ctx, docker.RunEnvironmentConfig{
			TenantID: tenantID, EnvID: envID, Image: tmpl.BaseImage,
			MemoryMB: int64(tmpl.MemoryMB), CPUMillis: int64(tmpl.CPUMillis),
			VolumeName: e.VolumeName, NetworkName: networkName,
			EgressAllow: tmpl.EgressPolicy == "allow",
		})
		if err != nil {
			return fmt.Errorf("recreate missing environment container: %w", err)
		}
		recreated = true
	}
	// undo rolls back a start that could not be committed: optionally the
	// restored preview routes and ingress network, then the container (removed
	// only if this call created it). It runs detached from the request.
	undo := func(routes, ingress bool) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if routes {
			m.cleanupPreviews(cleanupCtx, envID)
		}
		_ = m.docker.StopContainer(cleanupCtx, containerID)
		if recreated {
			_ = m.docker.RemoveContainer(cleanupCtx, containerID)
		}
		if ingress {
			_ = m.removePreviewIngressNetwork(cleanupCtx, tenantID, envID, containerID)
		}
	}
	if needsIngress {
		if err := m.ensurePreviewIngress(ctx, tenantID, envID, containerID); err != nil {
			undo(false, true)
			return fmt.Errorf("restore preview ingress network: %w", err)
		}
		if err := m.restorePreviews(ctx, envID, tenantID); err != nil {
			undo(true, true)
			return fmt.Errorf("restore preview routes: %w", err)
		}
	}
	now := time.Now().Unix()
	leaseExpiresAt := now + int64(e.LeaseDurationSeconds)
	var updateResult sql.Result
	updateResult, err = m.db.ExecContext(ctx,
		`UPDATE environments SET container_id = ?, status = 'running', lease_expires_at = ?,
		 last_activity_at = ?, updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'stopped'
		 AND EXISTS (SELECT 1 FROM tenants WHERE tenants.id = environments.tenant_id AND tenants.status = 'active')`,
		containerID, leaseExpiresAt, now, now, envID, tenantID,
	)
	updated := int64(0)
	if err == nil {
		updated, err = updateResult.RowsAffected()
	}
	if err != nil || updated != 1 {
		undo(needsIngress, needsIngress)
		if err != nil {
			return err
		}
		return fmt.Errorf("environment start was interrupted before ownership could be committed")
	}
	return nil
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

	// A container that is already gone is as stopped as it gets; Start
	// recreates it from the workspace volume.
	if err := m.docker.StopContainer(ctx, e.ContainerID); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("stop container: %w", err)
	}

	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		now, envID, tenantID,
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

	// Backstop: coreutils timeout kills the process inside the container a few
	// seconds after ExecRun gives up on the connection, so it does not run forever.
	cmd := append([]string{"timeout", "-s", "KILL", fmt.Sprintf("%d", req.Timeout+5)}, req.Command...)

	// Create exec
	execID, err := m.docker.ExecCreate(ctx, e.ContainerID, cmd, req.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	// Run exec with timeout
	start := time.Now()
	timeout := time.Duration(req.Timeout) * time.Second
	stdout, stderr, exitCode, err := m.docker.ExecRun(ctx, execID, timeout)

	durationMs := time.Since(start).Milliseconds()

	timedOut := err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
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
	// Conditional on 'running': if the reconciler expired the lease after the
	// check above, report that instead of claiming an extension that the
	// already-stopped environment will not get.
	res, err := m.db.ExecContext(ctx,
		`UPDATE environments SET lease_expires_at = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND status = 'running'`,
		leaseExpiresAt, now, envID, tenantID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierr.Conflict("environment stopped while extending its lease")
	}
	return nil
}

// templateColumns is the column list scanned by scanTemplate.
const templateColumns = `id, name, base_image, description, memory_mb, cpu_millicores, disk_mb,
	 egress_policy, created_at, updated_at`

func scanTemplate(row interface{ Scan(...any) error }) (*Template, error) {
	t := &Template{}
	err := row.Scan(&t.ID, &t.Name, &t.BaseImage, &t.Description, &t.MemoryMB, &t.CPUMillis,
		&t.DiskMB, &t.EgressPolicy, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// GetTemplate returns a template by ID, falling back to lookup by name.
func (m *Manager) GetTemplate(ctx context.Context, templateID string) (*Template, error) {
	t, err := scanTemplate(m.db.QueryRowContext(ctx,
		`SELECT `+templateColumns+` FROM environment_templates WHERE id = ?`, templateID))
	if err == sql.ErrNoRows {
		t, err = scanTemplate(m.db.QueryRowContext(ctx,
			`SELECT `+templateColumns+` FROM environment_templates WHERE name = ?`, templateID))
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
		`SELECT `+templateColumns+` FROM environment_templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()

	result := make([]*Template, 0)
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
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
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such volume") ||
		strings.Contains(msg, "404")
}

// ---------------------------------------------------------------------------
// Workspace Sync
// ---------------------------------------------------------------------------

const syncTimeout = 5 * time.Minute

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

	// git would parse a ref starting with '-' as an option. No valid ref name
	// starts with '-' (git check-ref-format), so reject it outright.
	if strings.HasPrefix(req.GitRef, "-") {
		return apierr.Validation("git_ref must not start with '-'")
	}
	ref := req.GitRef
	if ref == "" {
		ref = "HEAD"
	}

	// The URL and ref are passed as positional parameters ($1, $2), never
	// spliced into the script, so quotes, spaces or ';' in them cannot change
	// the command. If /workspace/.git exists, fetch and check out the ref;
	// otherwise clone fresh and, when a ref was requested, check it out too
	// (a --depth=1 clone alone only ever yields the default branch).
	const gitScript = `cd /workspace && if [ -d .git ]; then git fetch origin && git checkout "$2"; ` +
		`else git clone --depth=1 "$1" . && if [ "$2" != HEAD ]; then git fetch --depth=1 origin "$2" && git checkout FETCH_HEAD; fi; fi`

	// Create and run exec with a 5-minute timeout
	execID, err := m.docker.ExecCreate(ctx, e.ContainerID, []string{
		"timeout", "-s", "KILL", fmt.Sprintf("%d", int((syncTimeout + 5*time.Second).Seconds())),
		"sh", "-c", gitScript, "sh", req.GitURL, ref,
	}, "/workspace")
	if err != nil {
		return fmt.Errorf("sync exec create: %w", err)
	}

	_, _, exitCode, err := m.docker.ExecRun(ctx, execID, syncTimeout)
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
	m.mu.Lock()
	defer m.mu.Unlock()

	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return nil, err
	}
	if e.Status != "running" {
		return nil, apierr.Validation(fmt.Sprintf("environment must be running to create preview (current: %s)", e.Status))
	}

	// Validate name
	if !previewNamePattern.MatchString(req.Name) {
		return nil, apierr.Validation("preview name must be 1-50 characters, alphanumeric and hyphens, starting with alphanumeric")
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

	// Insert DB row, enforcing the per-environment cap atomically.
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO environment_previews (id, environment_id, tenant_id, name, port, dns_label, created_at, updated_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE (SELECT COUNT(*) FROM environment_previews WHERE environment_id = ?) < ?`,
		id, envID, tenantID, req.Name, req.Port, dnsLabel, now, now, envID, maxPreviewsPerEnv,
	)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, apierr.QuotaExceeded(fmt.Sprintf("maximum %d previews per environment reached", maxPreviewsPerEnv))
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, apierr.Conflict(fmt.Sprintf("preview with name %q already exists for this environment", req.Name))
		}
		return nil, fmt.Errorf("insert preview: %w", err)
	}

	ingressID, err := m.docker.EnsureIngressNetwork(ctx, docker.EnvironmentIngressNetworkName(tenantID, envID))
	if err == nil {
		err = m.docker.ConnectNetwork(ctx, ingressID, "paas-traefik")
	}
	if err == nil {
		err = m.docker.ConnectNetworkAlias(ctx, ingressID, e.ContainerID, docker.EnvironmentIngressAlias(tenantID, envID))
	}
	if err != nil {
		_, _ = m.db.ExecContext(ctx, `DELETE FROM environment_previews WHERE id = ?`, id)
		m.cleanupUnusedPreviewIngress(ctx, tenantID, envID, e.ContainerID)
		return nil, fmt.Errorf("connect preview ingress network: %w", err)
	}

	// Write Traefik route
	containerName := docker.EnvironmentIngressAlias(tenantID, envID)
	if err := m.writePreviewTraefikRoute(id, dnsLabel, containerName, req.Port); err != nil {
		// Rollback DB on Traefik write failure
		_, _ = m.db.ExecContext(ctx, `DELETE FROM environment_previews WHERE id = ?`, id)
		m.cleanupUnusedPreviewIngress(ctx, tenantID, envID, e.ContainerID)
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
	m.mu.Lock()
	defer m.mu.Unlock()

	e, err := m.Get(ctx, tenantID, envID)
	if err != nil {
		return err
	}
	// Verify the preview exists and belongs to the right tenant/env
	var id string
	err = m.db.QueryRowContext(ctx,
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
	m.cleanupUnusedPreviewIngress(ctx, tenantID, envID, e.ContainerID)
	return nil
}

func (m *Manager) hasPreviews(ctx context.Context, tenantID, envID string) (bool, error) {
	var count int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM environment_previews WHERE environment_id = ? AND tenant_id = ?`,
		envID, tenantID).Scan(&count); err != nil {
		return false, fmt.Errorf("count environment previews: %w", err)
	}
	return count > 0, nil
}

func (m *Manager) ensurePreviewIngress(ctx context.Context, tenantID, envID, containerID string) error {
	networkID, err := m.docker.EnsureIngressNetwork(ctx, docker.EnvironmentIngressNetworkName(tenantID, envID))
	if err != nil {
		return fmt.Errorf("ensure preview ingress network: %w", err)
	}
	if err := m.docker.ConnectNetwork(ctx, networkID, "paas-traefik"); err != nil {
		return fmt.Errorf("connect Traefik to preview ingress network: %w", err)
	}
	if err := m.docker.ConnectNetworkAlias(ctx, networkID, containerID, docker.EnvironmentIngressAlias(tenantID, envID)); err != nil {
		return fmt.Errorf("connect environment to preview ingress network: %w", err)
	}
	return nil
}

func (m *Manager) cleanupUnusedPreviewIngress(ctx context.Context, tenantID, envID, containerID string) {
	hasPreviews, err := m.hasPreviews(ctx, tenantID, envID)
	if err != nil {
		log.Printf("environments: check previews before ingress cleanup for %s: %v", envID, err)
		return
	}
	if hasPreviews {
		return
	}
	if err := m.removePreviewIngressNetwork(ctx, tenantID, envID, containerID); err != nil {
		log.Printf("environments: remove unused preview ingress network for %s: %v", envID, err)
	}
}

func (m *Manager) removePreviewIngressNetwork(ctx context.Context, tenantID, envID, containerID string) error {
	name := docker.EnvironmentIngressNetworkName(tenantID, envID)
	networks, err := m.docker.NetworkList(ctx)
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	var networkID string
	for _, network := range networks {
		if network.Name == name {
			networkID = network.ID
			break
		}
	}
	if networkID == "" {
		return nil
	}
	for _, endpoint := range []string{containerID, "paas-traefik"} {
		if endpoint == "" {
			continue
		}
		if err := m.docker.NetworkDisconnect(ctx, networkID, endpoint); err != nil && !isNotFoundError(err) {
			return fmt.Errorf("disconnect %s from preview ingress network: %w", endpoint, err)
		}
	}
	if err := m.docker.RemoveNetwork(ctx, networkID); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("remove preview ingress network: %w", err)
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
		if m.externalTLSTerminated {
			content = fmt.Sprintf(`http:
  routers:
    env-preview-%s:
      rule: "Host(`+"`%s`"+`)"
      service: "env-preview-%s"
      entryPoints:
        - web
      middlewares:
        - env-preview-%s-public-https
  middlewares:
    env-preview-%s-public-https:
      headers:
        customRequestHeaders:
          X-Forwarded-Proto: "https"
  services:
    env-preview-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%d"
`, previewID, host, previewID, previewID, previewID, previewID, containerName, port)
		} else {
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
		}
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
