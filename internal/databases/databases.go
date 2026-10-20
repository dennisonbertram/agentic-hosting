// Package databases manages Postgres and Redis database provisioning.
package databases

import (
	"bufio"
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// namePattern matches valid database names: starts with a letter, contains
// only alphanumeric characters, hyphens, and underscores, and does not end
// with a hyphen. Length is enforced separately for a clearer error message.
var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*[a-zA-Z0-9_]$`)

// ValidateName checks that a database name is safe for use as a Docker
// container/volume suffix: 1-63 characters, starts with a letter, only
// alphanumeric plus hyphens/underscores, cannot end with a hyphen.
func ValidateName(name string) error {
	if name == "" {
		return apierr.Validation("database name is required")
	}
	if len(name) > 63 {
		return apierr.Validation("database name must be 63 characters or fewer")
	}
	// Single-character names are valid as long as they are a letter.
	if len(name) == 1 {
		if name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z' {
			return nil
		}
		return apierr.Validation("database name must start with a letter and contain only alphanumeric characters, hyphens, and underscores")
	}
	if !namePattern.MatchString(name) {
		return apierr.Validation("database name must start with a letter, contain only alphanumeric characters, hyphens, and underscores, and cannot end with a hyphen")
	}
	return nil
}

// Database represents a provisioned database.
type Database struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	ContainerID      string `json:"-"`
	Host             string `json:"host,omitempty"`
	Port             int    `json:"port,omitempty"`
	DBName           string `json:"db_name,omitempty"`
	Username         string `json:"username,omitempty"`
	ConnectionString string `json:"connection_string,omitempty"` // only populated on create + explicit get
	VolumeName       string `json:"-"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a database.
type CreateRequest struct {
	Name string `json:"name"`
	Type string `json:"type"` // "postgres" or "redis"
}

// Manager manages database lifecycle.
type Manager struct {
	db        *sql.DB
	docker    docker.Client
	masterKey []byte
	mu        sync.Mutex // protects port allocation
	// healthTimeout bounds readiness waits after a (re)start; 0 means 30s.
	healthTimeout time.Duration
}

// NewManager creates a database manager.
func NewManager(db *sql.DB, dockerClient docker.Client, masterKey []byte) *Manager {
	if dockerClient == nil {
		panic("databases: NewManager requires non-nil docker client")
	}
	mgr := &Manager{
		db:        db,
		docker:    dockerClient,
		masterKey: masterKey,
	}
	mgr.ReconcileStale()
	mgr.migrateLoopbackURLs()
	return mgr
}

// ReconcileStale marks databases stuck in "provisioning" as "failed" and
// attempts to clean up their Docker resources. Called on startup.
func (m *Manager) ReconcileStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, COALESCE(container_id, ''), volume_name FROM databases WHERE status = 'provisioning'`)
	if err != nil {
		log.Printf("databases: reconcile query failed: %v", err)
		return
	}
	defer rows.Close()

	var stale []struct{ id, containerID, volumeName string }
	for rows.Next() {
		var s struct{ id, containerID, volumeName string }
		if err := rows.Scan(&s.id, &s.containerID, &s.volumeName); err != nil {
			log.Printf("databases: reconcile scan failed: %v", err)
			continue
		}
		stale = append(stale, s)
	}

	for _, s := range stale {
		log.Printf("databases: reconciling stale database %s", s.id)
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
		log.Printf("databases: reconciled %d stale databases", len(stale))
	}
}

// Create provisions a new database.
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Database, error) {
	if req.Type != "postgres" && req.Type != "redis" {
		return nil, apierr.Validation(fmt.Sprintf("invalid database type %q; must be \"postgres\" or \"redis\"", req.Type))
	}
	if err := ValidateName(req.Name); err != nil {
		return nil, err
	}

	// Check disk space before provisioning
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, apierr.Unavailable("insufficient disk space; try again later", err)
	}

	// Docker's ContainerCreate does not fetch a missing image. Ensure the
	// database image exists before quota checks and before inserting a
	// provisioning record or creating a volume. Pulling before the quota
	// transaction avoids holding or extending the check-to-insert window.
	image := databaseImage(req.Type)
	// Like docker.pullOrUseLocal, tolerate a failed pull (registry down, rate
	// limited) when the image is already in the local store.
	// RunDatabase pulls again; that second pull is not avoided here.
	if err := m.docker.PullImage(ctx, image); err != nil {
		if ok, _ := m.docker.ImageExists(ctx, image); !ok {
			return nil, fmt.Errorf("pull database image: %w", err)
		}
		log.Printf("WARNING: pull of %s failed (%v); using local image", image, err)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}

	var maxDatabases int
	if err := m.db.QueryRowContext(ctx,
		`SELECT max_databases FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxDatabases); err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}

	// Generate password
	password, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}

	volumeName := fmt.Sprintf("ah-db-%s", id)
	containerName := containerNameFor(tenantID, id)

	// Encrypt password
	passwordEnc, err := crypto.Encrypt([]byte(password), m.masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}

	// Find free port and insert atomically — retry on UNIQUE constraint violation
	var port int
	var dbName, username, connStr string
	now := time.Now().Unix()
	const maxPortRetries = 5
	for attempt := 0; attempt < maxPortRetries; attempt++ {
		port, err = m.findFreePort(req.Type)
		if err != nil {
			return nil, fmt.Errorf("find free port: %w", err)
		}

		switch req.Type {
		case "postgres":
			dbName = "ah"
			username = "ah"
			connStr = buildConnectionString(req.Type, id, username, password, dbName)
		case "redis":
			connStr = buildConnectionString(req.Type, id, "", password, "")
		}

		connStrEnc, encErr := crypto.Encrypt([]byte(connStr), m.masterKey)
		if encErr != nil {
			return nil, fmt.Errorf("encrypt connection string: %w", encErr)
		}

		// Insert DB record — UNIQUE index on port prevents race conditions
		// The quota count is part of the INSERT so concurrent creates cannot
		// both pass the check.
		var res sql.Result
		res, err = m.db.ExecContext(ctx,
			`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
			 password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at)
			 SELECT ?, ?, ?, ?, 'provisioning', ?, ?, ?, ?, ?, ?, ?, ?, ?
			 WHERE (SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed') < ?`,
			id, tenantID, req.Name, req.Type, docker.DatabaseAlias(id), port, dbName, username,
			passwordEnc, connStrEnc, volumeName, now, now, tenantID, maxDatabases,
		)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, apierr.QuotaExceeded(fmt.Sprintf("quota exceeded: maximum %d databases reached", maxDatabases))
			}
		}
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue // retry with different port
			}
			return nil, fmt.Errorf("insert database: %w", err)
		}
		break // success
	}
	if err != nil {
		return nil, fmt.Errorf("insert database after %d retries: %w", maxPortRetries, err)
	}

	// Docker cleanup on failure must survive a cancelled request context (the
	// client may disconnect during the health wait), or the container leaks.
	cleanupCtx := context.WithoutCancel(ctx)
	// cleanup removes Docker resources after a failed or interrupted provision.
	// When the row is already gone (a concurrent Delete ran first), that Delete
	// may have missed the per-database network created afterwards; remove it.
	cleanup := func(containerID string, rowGone bool) {
		cctx, cancel := context.WithTimeout(cleanupCtx, containerCleanupTimeout)
		defer cancel()
		if containerID != "" {
			m.removeContainer(cctx, containerID)
		}
		_ = m.docker.RemoveVolume(cctx, volumeName)
		if rowGone {
			m.removeDatabaseNetwork(cctx, tenantID, id)
		}
	}

	// Create Docker volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Run container
	containerID, err := m.runContainer(ctx, req.Type, tenantID, id, containerName, volumeName, port, dbName, username, password)
	if err != nil {
		cleanup("", m.markFailed(ctx, id))
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Update container ID — check rows affected to detect concurrent Delete.
	// Guarded on 'provisioning': a tenant suspension that ran before this write
	// found no container_id, removed nothing and set the row 'stopped'.
	result, updateErr := m.db.ExecContext(ctx,
		`UPDATE databases SET container_id = ?, updated_at = ? WHERE id = ? AND status = 'provisioning'`,
		containerID, time.Now().Unix(), id,
	)
	if updateErr != nil {
		// The row still exists: mark it failed so it stops counting toward quota.
		log.Printf("databases: failed to update container_id for %s, cleaning up: %v", id, updateErr)
		cleanup(containerID, m.markFailed(ctx, id))
		return nil, fmt.Errorf("record database container: %w", updateErr)
	}
	if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
		var exists int
		if qErr := m.db.QueryRowContext(cleanupCtx, `SELECT COUNT(*) FROM databases WHERE id = ?`, id).Scan(&exists); qErr == nil && exists > 0 {
			// Stopped (tenant suspended) before the container was recorded: the
			// suspension could not remove it, so do it here. The volume is kept
			// for reactivation, which starts the row from it.
			log.Printf("databases: %s was stopped during provisioning (tenant suspended?); removing unrecorded container %s", id, containerID)
			m.removeProvisionedContainer(cleanupCtx, containerID)
			return nil, apierr.Conflict("database was stopped during provisioning (tenant suspended)")
		}
		log.Printf("databases: record %s was deleted during provisioning, cleaning up", id)
		cleanup(containerID, true)
		return nil, fmt.Errorf("database record deleted during provisioning")
	}

	// Wait for health check
	if !m.waitHealthy(req.Type, port, password) {
		cleanup(containerID, m.markFailed(ctx, id))
		return nil, fmt.Errorf("database health check failed after %s", m.readyTimeout())
	}

	// CAS provisioning -> ready: a tenant suspension that landed after the
	// health probe has already set 'stopped' and removed the container; an
	// unconditional update would leave a 'ready' row with no container that
	// reactivation (which only restarts 'stopped' rows) never starts.
	switch m.casStatus(ctx, id, "provisioning", "ready") {
	case casApplied:
	case casChanged:
		// Normally the suspension already removed this container; remove it by
		// ID anyway (a no-op then) so it can never outlive the suspension. The
		// volume is kept for reactivation.
		log.Printf("databases: %s changed state during provisioning (tenant suspended?); leaving it for reactivation", id)
		m.removeProvisionedContainer(cleanupCtx, containerID)
		return nil, apierr.Conflict("database was stopped during provisioning (tenant suspended)")
	case casError:
		// The row may still exist and own its per-database network: mark it
		// failed (frees quota) and let markFailed decide whether it is gone.
		cleanup(containerID, m.markFailed(ctx, id))
		return nil, fmt.Errorf("mark database ready failed")
	default:
		// Row was deleted during provisioning — clean up
		cleanup(containerID, true)
		return nil, fmt.Errorf("database record deleted during provisioning")
	}

	return &Database{
		ID:               id,
		TenantID:         tenantID,
		Name:             req.Name,
		Type:             req.Type,
		Status:           "ready",
		Host:             docker.DatabaseAlias(id),
		Port:             EnginePort(req.Type),
		DBName:           dbName,
		Username:         username,
		ConnectionString: connStr,
		VolumeName:       volumeName,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

func databaseImage(dbType string) string {
	switch dbType {
	case "postgres":
		return "postgres:16-alpine"
	case "redis":
		return "redis:7-alpine"
	default:
		return ""
	}
}

// Get returns a database by ID, scoped to tenant. Connection string NOT included.
func (m *Manager) Get(ctx context.Context, tenantID, dbID string) (*Database, error) {
	d := &Database{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, type, status, COALESCE(container_id, ''), host, port, db_name, username,
		 volume_name, created_at, updated_at
		 FROM databases WHERE id = ? AND tenant_id = ?`,
		dbID, tenantID,
	).Scan(&d.ID, &d.TenantID, &d.Name, &d.Type, &d.Status, &d.ContainerID,
		&d.Host, &d.Port, &d.DBName, &d.Username,
		&d.VolumeName, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("database not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get database: %w", err)
	}
	exposeEngineAddress(d)
	return d, nil
}

// List returns all databases for a tenant. Connection strings NOT included.
func (m *Manager) List(ctx context.Context, tenantID string) ([]*Database, error) {
	return m.ListPaginated(ctx, tenantID, 100, 0)
}

// ListPaginated returns databases for a tenant with limit and offset.
func (m *Manager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*Database, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, type, status, host, port, db_name, username, created_at, updated_at
		 FROM databases WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var result []*Database
	for rows.Next() {
		d := &Database{}
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Name, &d.Type, &d.Status,
			&d.Host, &d.Port, &d.DBName, &d.Username, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan database: %w", err)
		}
		exposeEngineAddress(d)
		result = append(result, d)
	}
	return result, rows.Err()
}

// GetConnectionString returns the decrypted connection string.
func (m *Manager) GetConnectionString(ctx context.Context, tenantID, dbID string) (string, error) {
	var connStrEnc string
	err := m.db.QueryRowContext(ctx,
		`SELECT connection_string_encrypted FROM databases WHERE id = ? AND tenant_id = ?`,
		dbID, tenantID,
	).Scan(&connStrEnc)
	if err == sql.ErrNoRows {
		return "", apierr.NotFound("database not found")
	}
	if err != nil {
		return "", fmt.Errorf("get connection string: %w", err)
	}
	plaintext, err := crypto.Decrypt(connStrEnc, m.masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt connection string: %w", err)
	}
	return string(plaintext), nil
}

// containerCleanupTimeout bounds container stop+remove in Delete so a stalled
// Docker daemon cannot hang the request forever. A variable so tests can shorten it.
var containerCleanupTimeout = time.Minute

// Delete destroys a database: stops container, removes volume, deletes record.
// Only deletes the DB record after Docker cleanup succeeds to avoid orphaning resources.
func (m *Manager) Delete(ctx context.Context, tenantID, dbID string) error {
	d, err := m.Get(ctx, tenantID, dbID)
	if err != nil {
		return err
	}

	// Cleanup must outlive the request: a slow wipe under gVisor used to
	// exhaust the route timeout and leave no time to remove the volume.
	// Detach from request cancellation; wipe and removal get separate budgets.
	ctx = context.WithoutCancel(ctx)

	// Stop and remove container
	if d.ContainerID != "" {
		ctrCtx, ctrCancel := context.WithTimeout(ctx, containerCleanupTimeout)
		defer ctrCancel()
		if err := m.docker.StopContainer(ctrCtx, d.ContainerID); err != nil {
			log.Printf("databases: stop container %s: %v", d.ContainerID, err)
			// Container may not exist (already stopped/removed) — continue
		}
		if err := m.docker.RemoveContainer(ctrCtx, d.ContainerID); err != nil {
			// If container removal fails (not just "not found"), keep the record
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("databases: remove container %s failed, keeping record: %v", d.ContainerID, err)
				return fmt.Errorf("failed to remove database container")
			}
		}
	}

	// Securely wipe volume data before removal to prevent future tenants from
	// recovering data (issue #9). Best-effort: a wipe failure is logged but does
	// not block removal, since the volume will be deleted immediately after.
	if d.VolumeName != "" {
		if err := m.wipeVolume(ctx, d.VolumeName); err != nil {
			log.Printf("databases: wipe volume %s failed (continuing with removal): %v", d.VolumeName, err)
		}
	}

	// Remove volume
	if d.VolumeName != "" {
		rmCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := m.docker.RemoveVolume(rmCtx, d.VolumeName); err != nil {
			if !strings.Contains(err.Error(), "no such volume") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("databases: remove volume %s failed, keeping record: %v", d.VolumeName, err)
				return fmt.Errorf("failed to remove database volume")
			}
		}
	}

	// Delete record only after Docker cleanup succeeded
	_, err = m.db.ExecContext(ctx, `DELETE FROM databases WHERE id = ? AND tenant_id = ?`, dbID, tenantID)
	if err != nil {
		return fmt.Errorf("delete database record: %w", err)
	}

	m.removeDatabaseNetwork(ctx, tenantID, dbID)
	return nil
}

// StopAllForTenant stops and removes all database containers for the tenant,
// updating each record's status to "stopped". The database records are kept so
// they can be restarted if the tenant is ever reactivated. Errors are logged
// but do not abort processing of remaining databases.
func (m *Manager) StopAllForTenant(ctx context.Context, tenantID string) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, COALESCE(container_id, ''), status FROM databases WHERE tenant_id = ?
		 AND (status NOT IN ('failed', 'stopped') OR (status = 'failed' AND COALESCE(container_id, '') != ''))`,
		tenantID,
	)
	if err != nil {
		log.Printf("databases: StopAllForTenant query failed for tenant %s: %v", tenantID, err)
		return
	}
	defer rows.Close()

	var dbs []struct{ id, containerID, status string }
	for rows.Next() {
		var d struct{ id, containerID, status string }
		if err := rows.Scan(&d.id, &d.containerID, &d.status); err != nil {
			log.Printf("databases: StopAllForTenant scan failed: %v", err)
			continue
		}
		dbs = append(dbs, d)
	}
	if err := rows.Err(); err != nil {
		log.Printf("databases: StopAllForTenant rows error: %v", err)
	}

	for _, d := range dbs {
		if d.containerID != "" {
			if err := m.docker.StopContainer(ctx, d.containerID); err != nil {
				log.Printf("databases: StopAllForTenant stop container %s: %v", d.containerID, err)
			}
			if err := m.docker.RemoveContainer(ctx, d.containerID); err != nil {
				if !strings.Contains(err.Error(), "No such container") &&
					!strings.Contains(err.Error(), "not found") {
					log.Printf("databases: StopAllForTenant remove container %s: %v", d.containerID, err)
				}
			}
		}
		if d.status == "failed" {
			// A failed row must not become restartable; just drop its stale container.
			_, _ = m.db.ExecContext(ctx, `UPDATE databases SET container_id = NULL WHERE id = ?`, d.id)
			continue
		}
		m.updateStatus(ctx, d.id, "stopped")
	}
	if len(dbs) > 0 {
		log.Printf("databases: StopAllForTenant stopped %d databases for tenant %s", len(dbs), tenantID)
	}
}

// wipeVolume runs a short-lived container to overwrite all files in the named
// volume with zeros, ensuring a future tenant cannot recover data after a
// database is deleted. The wipe uses a 5-minute timeout to bound the operation.
func (m *Manager) wipeVolume(ctx context.Context, volumeName string) error {
	wipeCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return m.docker.WipeVolume(wipeCtx, volumeName)
}

// casResult is the outcome of casStatus.
type casResult int

const (
	casApplied casResult = iota // row was in the expected state and was updated
	casChanged                  // row exists but is no longer in the expected state
	casGone                     // row does not exist
	casError                    // the update or the existence check failed; row state unknown
)

// casStatus moves a database row from one status to another only if it is
// still in the expected state, on a fresh context like updateStatus.
func (m *Manager) casStatus(ctx context.Context, id, from, to string) casResult {
	freshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	res, err := m.db.ExecContext(freshCtx,
		`UPDATE databases SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, time.Now().Unix(), id, from)
	if err != nil {
		log.Printf("databases: failed to update status for %s: %v", id, err)
		return casError
	}
	n, err := res.RowsAffected()
	if err != nil {
		log.Printf("databases: failed to read rows affected for %s: %v", id, err)
		return casError
	}
	if n > 0 {
		return casApplied
	}
	var one int
	switch err := m.db.QueryRowContext(freshCtx, `SELECT 1 FROM databases WHERE id = ?`, id).Scan(&one); {
	case err == nil:
		return casChanged
	case errors.Is(err, sql.ErrNoRows):
		return casGone
	default:
		log.Printf("databases: failed to check whether %s exists: %v", id, err)
		return casError
	}
}

// markFailed sets a row to 'failed' on a detached context and reports whether
// the row is confirmed gone (the update matched no row). A database error is
// not treated as "gone": the row may still exist in 'provisioning' and own its
// per-database network, so callers must not tear that network down.
func (m *Manager) markFailed(ctx context.Context, id string) (rowGone bool) {
	freshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	res, err := m.db.ExecContext(freshCtx,
		`UPDATE databases SET status = 'failed', updated_at = ? WHERE id = ?`,
		time.Now().Unix(), id)
	if err != nil {
		log.Printf("databases: failed to mark %s failed: %v", id, err)
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n == 0
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) bool {
	// Use a fresh context to ensure status updates succeed even if the
	// request context is cancelled or timed out.
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := m.db.ExecContext(freshCtx,
		`UPDATE databases SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("databases: failed to update status for %s: %v", id, err)
		return false
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("databases: record %s was deleted, status update skipped", id)
		return false
	}
	return true
}

func (m *Manager) findFreePort(dbType string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var minPort, maxPort int
	switch dbType {
	case "postgres":
		minPort, maxPort = 5432, 6000
	case "redis":
		minPort, maxPort = 6379, 7000
	}

	// Check which ports are already allocated in DB
	rows, err := m.db.Query(`SELECT port FROM databases WHERE port IS NOT NULL AND status NOT IN ('failed')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	usedPorts := make(map[int]bool)
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, fmt.Errorf("scan port: %w", err)
		}
		usedPorts[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate ports: %w", err)
	}

	// Find first free port
	for port := minPort; port <= maxPort; port++ {
		if usedPorts[port] {
			continue
		}
		// Also check if port is actually available on the host
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", minPort, maxPort)
}

func containerNameFor(tenantID, id string) string {
	if len(tenantID) > 8 {
		tenantID = tenantID[:8]
	}
	return fmt.Sprintf("ah-db-%s-%s", tenantID, id[:16])
}

func (m *Manager) runContainer(ctx context.Context, dbType, tenantID, dbID, name, volume string, port int, dbName, user, password string) (string, error) {
	netName := docker.DatabaseNetworkName(tenantID, dbID)
	if _, err := m.docker.EnsureDatabaseNetwork(ctx, netName); err != nil {
		return "", fmt.Errorf("ensure database network: %w", err)
	}
	if dbType == "redis" {
		return m.runRedis(ctx, name, volume, port, password, netName, docker.DatabaseAlias(dbID))
	}
	return m.runPostgres(ctx, name, volume, port, dbName, user, password, netName, docker.DatabaseAlias(dbID))
}

// StartAllForTenant recreates the containers of a tenant's stopped databases
// (see StopAllForTenant) from the stored config, reusing each database's
// volume, port and password. A failure leaves that database "stopped" (not
// "failed") and is logged: its volume still holds the data, so the row must
// keep its port reservation and network ownership and stay eligible for a
// later start (e.g. a slow Postgres crash recovery that outlasts the health
// probe). "failed" is terminal: it frees the port and only Delete, which wipes
// the volume, remains. The remaining databases are still processed.
func (m *Manager) StartAllForTenant(ctx context.Context, tenantID string) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, type, port, COALESCE(db_name, ''), COALESCE(username, ''), password_encrypted, volume_name
		 FROM databases WHERE tenant_id = ? AND status = 'stopped'`, tenantID)
	if err != nil {
		log.Printf("databases: StartAllForTenant query failed for tenant %s: %v", tenantID, err)
		return
	}
	type stopped struct {
		id, typ, dbName, user, passEnc, volume string
		port                                   int
	}
	var dbs []stopped
	for rows.Next() {
		var d stopped
		if err := rows.Scan(&d.id, &d.typ, &d.port, &d.dbName, &d.user, &d.passEnc, &d.volume); err != nil {
			log.Printf("databases: StartAllForTenant scan failed: %v", err)
			continue
		}
		dbs = append(dbs, d)
	}
	rows.Close()

	for _, d := range dbs {
		if !m.tenantActive(ctx, tenantID) {
			log.Printf("databases: StartAllForTenant: tenant %s no longer active, not starting %s", tenantID, d.id)
			return
		}
		pw, err := crypto.Decrypt(d.passEnc, m.masterKey)
		if err != nil {
			log.Printf("databases: StartAllForTenant decrypt password for %s: %v (left stopped)", d.id, err)
			continue
		}
		password := string(pw)
		containerID, err := m.runContainer(ctx, d.typ, tenantID, d.id, containerNameFor(tenantID, d.id), d.volume, d.port, d.dbName, d.user, password)
		if err != nil {
			log.Printf("databases: StartAllForTenant run container for %s: %v (left stopped)", d.id, err)
			continue
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE databases SET container_id = ?, updated_at = ? WHERE id = ?`,
			containerID, time.Now().Unix(), d.id)
		if !m.waitHealthy(d.typ, d.port, password) {
			log.Printf("databases: StartAllForTenant health check failed for %s (left stopped; volume kept)", d.id)
			m.removeContainer(ctx, containerID) // volume kept: it holds the data
			_, _ = m.db.ExecContext(ctx, `UPDATE databases SET container_id = '', updated_at = ? WHERE id = ? AND container_id = ?`,
				time.Now().Unix(), d.id, containerID)
			continue
		}
		if !m.tenantActive(ctx, tenantID) {
			// Suspended while starting: undo so the container is not left running.
			m.removeContainer(ctx, containerID)
			m.updateStatus(ctx, d.id, "stopped")
			return
		}
		m.updateStatus(ctx, d.id, "ready")
	}
}

// readyTimeout bounds readiness waits after a container (re)start.
func (m *Manager) readyTimeout() time.Duration {
	if m.healthTimeout > 0 {
		return m.healthTimeout
	}
	return 30 * time.Second
}

// waitHealthy probes the engine on its host-loopback port until it is ready
// or readyTimeout elapses.
func (m *Manager) waitHealthy(dbType string, port int, password string) bool {
	if dbType == "redis" {
		return m.waitForRedis(port, password, m.readyTimeout())
	}
	return m.waitForPostgres(port, password, m.readyTimeout())
}

// tenantActive reports whether the tenant is currently 'active'.
func (m *Manager) tenantActive(ctx context.Context, tenantID string) bool {
	var status string
	err := m.db.QueryRowContext(ctx, `SELECT status FROM tenants WHERE id = ?`, tenantID).Scan(&status)
	return err == nil && status == "active"
}

// removeProvisionedContainer removes a container Create started for a row that
// was stopped underneath it, on a bounded context, keeping the volume.
func (m *Manager) removeProvisionedContainer(ctx context.Context, containerID string) {
	cctx, cancel := context.WithTimeout(ctx, containerCleanupTimeout)
	defer cancel()
	m.removeContainer(cctx, containerID)
}

func (m *Manager) removeContainer(ctx context.Context, containerID string) {
	_ = m.docker.StopContainer(ctx, containerID)
	_ = m.docker.RemoveContainer(ctx, containerID)
}

func (m *Manager) runPostgres(ctx context.Context, name, volume string, port int, dbName, user, password, networkName, networkAlias string) (string, error) {
	return m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          name,
		Image:         databaseImage("postgres"),
		HostPort:      port,
		ContainerPort: 5432,
		Env: map[string]string{
			"POSTGRES_DB":       dbName,
			"POSTGRES_USER":     user,
			"POSTGRES_PASSWORD": password,
		},
		VolumeName:   volume,
		MountPath:    "/var/lib/postgresql/data",
		NetworkName:  networkName,
		NetworkAlias: networkAlias,
	})
}

func (m *Manager) runRedis(ctx context.Context, name, volume string, port int, password, networkName, networkAlias string) (string, error) {
	return m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          name,
		Image:         databaseImage("redis"),
		HostPort:      port,
		ContainerPort: 6379,
		Env:           map[string]string{},
		Cmd:           []string{"redis-server", "--requirepass", password},
		VolumeName:    volume,
		MountPath:     "/data",
		NetworkName:   networkName,
		NetworkAlias:  networkAlias,
	})
}

// waitForPostgres probes Postgres at the protocol level. It sends a minimal
// StartupMessage (protocol 3.0) over a raw TCP connection and checks that the
// server responds with any valid Postgres message byte (AuthenticationOK 'R',
// AuthenticationMD5Password 'R', or ErrorResponse 'E'). Any such response
// means the server process is initialised and accepting queries; a plain TCP
// accept with no bytes means the container is still starting up.
func (m *Manager) waitForPostgres(port int, password string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		if postgresReady(addr) {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

// postgresReady returns true when the Postgres server at addr has completed
// initialisation and is ready to authenticate connections.
func postgresReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Build a StartupMessage for protocol 3.0 with user "ah" and database "ah".
	// Wire format: int32 total_length | int32 protocol_version | key\0value\0 ... \0
	user := "ah"
	dbName := "ah"
	// Parameters section: "user\0<user>\0database\0<dbname>\0\0"
	params := "user\x00" + user + "\x00database\x00" + dbName + "\x00\x00"
	// Total length = 4 (length field) + 4 (protocol version) + len(params)
	totalLen := int32(4 + 4 + len(params))
	msg := make([]byte, totalLen)
	// Length (big-endian int32)
	msg[0] = byte(totalLen >> 24)
	msg[1] = byte(totalLen >> 16)
	msg[2] = byte(totalLen >> 8)
	msg[3] = byte(totalLen)
	// Protocol version 3.0 = 0x00030000
	msg[4] = 0x00
	msg[5] = 0x03
	msg[6] = 0x00
	msg[7] = 0x00
	copy(msg[8:], params)

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		return false
	}

	// Read the first byte of the server response. Any valid Postgres backend
	// message type here confirms the server is running:
	//   'R' = Authentication message (AuthenticationOK or auth challenge)
	//   'E' = ErrorResponse (e.g. wrong password, unknown user — still means up)
	//   'N' = NoticeResponse
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		return false
	}
	b := buf[0]
	return b == 'R' || b == 'E' || b == 'N'
}

// waitForRedis probes Redis with the configured password and then checks for
// a +PONG response. A container that accepts TCP but has not finished loading
// its data set or configuration will not be considered ready.
func (m *Manager) waitForRedis(port int, password string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		if redisReady(addr, password) {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

// redisReady returns true when the Redis server at addr accepts AUTH (when a
// password is configured) and responds to PING. Each probe has a fixed network
// deadline, and response lines are bounded to avoid hanging on a bad endpoint.
func redisReady(addr, password string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return false
	}

	reader := bufio.NewReaderSize(conn, 256)
	if password != "" {
		if _, err := fmt.Fprintf(conn, "*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password); err != nil {
			return false
		}
		if !redisSimpleReply(reader, "+OK\r\n") {
			return false
		}
	}
	if _, err := io.WriteString(conn, "*1\r\n$4\r\nPING\r\n"); err != nil {
		return false
	}
	return redisSimpleReply(reader, "+PONG\r\n")
}

func redisSimpleReply(reader *bufio.Reader, want string) bool {
	line, err := reader.ReadSlice('\n')
	return err == nil && string(line) == want
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
