// Package snapshots manages point-in-time snapshots of deployed services,
// capturing the Docker image tag, environment variables, and resource config.
package snapshots

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Snapshot represents a point-in-time capture of a service's image, port,
// and resource configuration.
type Snapshot struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	ServiceID      string `json:"service_id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	ImageRef       string `json:"image_ref"`
	ResourceConfig string `json:"resource_config,omitempty"`
	Port           int    `json:"port"`
	CreatedAt      int64  `json:"created_at"`
}

// CreateRequest holds the user-supplied fields for creating a snapshot.
type CreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Manager provides snapshot CRUD backed by SQLite and Docker.
type Manager struct {
	db        *sql.DB
	docker    docker.Client
	masterKey []byte
}

// NewManager creates a snapshot Manager.
func NewManager(db *sql.DB, docker docker.Client, masterKey []byte) *Manager {
	return &Manager{db: db, docker: docker, masterKey: masterKey}
}

// Create captures a snapshot from a running service. It tags the service's
// Docker image, copies encrypted env vars, and stores resource config.
func (m *Manager) Create(ctx context.Context, tenantID, serviceID string, req CreateRequest) (*Snapshot, error) {
	// Validate name.
	if req.Name == "" {
		return nil, apierr.Validation("snapshot name is required")
	}
	if len(req.Name) > 128 {
		return nil, apierr.Validation("snapshot name must be 128 characters or fewer")
	}
	if len(req.Description) > 1024 {
		return nil, apierr.Validation("snapshot description must be 1024 characters or fewer")
	}

	// Look up the source service and verify tenant ownership.
	var svcID, svcTenantID, svcName, svcStatus, svcImage string
	var svcPort int
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, image, port FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&svcID, &svcTenantID, &svcName, &svcStatus, &svcImage, &svcPort)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("service not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query service: %w", err)
	}

	if svcImage == "" {
		return nil, apierr.Conflict("service has no built image to snapshot")
	}

	snapshotID, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate snapshot id: %w", err)
	}

	// Tag the Docker image for the snapshot.
	imageRef := fmt.Sprintf("127.0.0.1:5000/snapshots/%s:%s", tenantID, snapshotID)
	if err := m.docker.TagImage(ctx, svcImage, imageRef); err != nil {
		return nil, fmt.Errorf("tag image for snapshot: %w", err)
	}

	// If anything below fails, best-effort remove the orphaned tag.
	committed := false
	defer func() {
		if !committed {
			if err := m.docker.RemoveImage(ctx, imageRef); err != nil {
				log.Printf("WARNING: failed to clean up orphaned snapshot image %s: %v", imageRef, err)
			}
		}
	}()

	// Capture encrypted env vars as a JSON blob.
	rows, err := m.db.QueryContext(ctx,
		`SELECT key, value_encrypted FROM service_env WHERE service_id = ?`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query service env: %w", err)
	}
	defer rows.Close()

	envBlob := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan service env row: %w", err)
		}
		envBlob[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate service env rows: %w", err)
	}

	envJSON, err := json.Marshal(envBlob)
	if err != nil {
		return nil, fmt.Errorf("marshal env blob: %w", err)
	}

	// Capture resource config from tenant quotas.
	resourceConfig := "{}"
	var maxMemoryMB sql.NullInt64
	var maxCPUCores sql.NullFloat64
	err = m.db.QueryRowContext(ctx,
		`SELECT max_memory_mb, max_cpu_cores FROM tenant_quotas WHERE tenant_id = ?`,
		tenantID,
	).Scan(&maxMemoryMB, &maxCPUCores)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query tenant quotas: %w", err)
	}
	if err == sql.ErrNoRows {
		log.Printf("WARNING: no tenant quotas found for tenant %s during snapshot creation", tenantID)
	}
	if maxMemoryMB.Valid || maxCPUCores.Valid {
		rc := make(map[string]interface{})
		if maxMemoryMB.Valid {
			rc["max_memory_mb"] = maxMemoryMB.Int64
		}
		if maxCPUCores.Valid {
			rc["max_cpu_cores"] = maxCPUCores.Float64
		}
		rcJSON, err := json.Marshal(rc)
		if err != nil {
			return nil, fmt.Errorf("marshal resource config: %w", err)
		}
		resourceConfig = string(rcJSON)
	}

	now := time.Now().Unix()

	// Use a transaction so we can rollback the DB insert if it fails,
	// keeping the tagged image as best-effort cleanup.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshotID, tenantID, serviceID, req.Name, req.Description, imageRef, string(envJSON), resourceConfig, svcPort, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert snapshot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit snapshot: %w", err)
	}
	committed = true

	return &Snapshot{
		ID:             snapshotID,
		TenantID:       tenantID,
		ServiceID:      serviceID,
		Name:           req.Name,
		Description:    req.Description,
		ImageRef:       imageRef,
		ResourceConfig: resourceConfig,
		Port:           svcPort,
		CreatedAt:      now,
	}, nil
}

// ListFilter holds optional filter parameters for listing snapshots.
type ListFilter struct {
	ServiceID string // exact match on service_id
	Name      string // substring match on name (LIKE %name%)
	Since     int64  // filter created_at >= since (unix timestamp)
}

// List returns snapshots for a tenant with pagination and optional filters.
func (m *Manager) List(ctx context.Context, tenantID string, limit, offset int, filter *ListFilter) ([]*Snapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	query := `SELECT id, tenant_id, service_id, name, description, image_ref, resource_config, port, created_at
		 FROM snapshots WHERE tenant_id = ?`
	args := []interface{}{tenantID}

	if filter != nil {
		if filter.ServiceID != "" {
			query += ` AND service_id = ?`
			args = append(args, filter.ServiceID)
		}
		if filter.Name != "" {
			query += ` AND name LIKE ?`
			args = append(args, "%"+filter.Name+"%")
		}
		if filter.Since > 0 {
			query += ` AND created_at >= ?`
			args = append(args, filter.Since)
		}
	}

	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows.Close()

	snapshots := make([]*Snapshot, 0)
	for rows.Next() {
		s := &Snapshot{}
		if err := rows.Scan(&s.ID, &s.TenantID, &s.ServiceID, &s.Name, &s.Description,
			&s.ImageRef, &s.ResourceConfig, &s.Port, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan snapshot row: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshot rows: %w", err)
	}

	return snapshots, nil
}

// Get returns a single snapshot, verifying tenant ownership.
func (m *Manager) Get(ctx context.Context, tenantID, snapshotID string) (*Snapshot, error) {
	s := &Snapshot{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, service_id, name, description, image_ref, resource_config, port, created_at
		 FROM snapshots WHERE id = ? AND tenant_id = ?`,
		snapshotID, tenantID,
	).Scan(&s.ID, &s.TenantID, &s.ServiceID, &s.Name, &s.Description,
		&s.ImageRef, &s.ResourceConfig, &s.Port, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("snapshot not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query snapshot: %w", err)
	}
	return s, nil
}

// Delete removes a snapshot and attempts to untag the associated Docker image.
func (m *Manager) Delete(ctx context.Context, tenantID, snapshotID string) error {
	// Verify ownership by fetching first.
	snap, err := m.Get(ctx, tenantID, snapshotID)
	if err != nil {
		return err
	}

	// Best-effort image removal — log but don't fail the delete.
	if snap.ImageRef != "" {
		if err := m.docker.RemoveImage(ctx, snap.ImageRef); err != nil {
			log.Printf("WARNING: failed to remove snapshot image %s: %v", snap.ImageRef, err)
		}
	}

	_, err = m.db.ExecContext(ctx,
		`DELETE FROM snapshots WHERE id = ? AND tenant_id = ?`,
		snapshotID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("delete snapshot: %w", err)
	}

	return nil
}

// RestoreEnvVars decrypts and returns the environment variables stored in a
// snapshot. Requires tenantID for tenant isolation — never call without it.
func (m *Manager) RestoreEnvVars(ctx context.Context, tenantID, snapshotID string) (map[string]string, error) {
	var envEncrypted string
	err := m.db.QueryRowContext(ctx,
		`SELECT env_encrypted FROM snapshots WHERE id = ? AND tenant_id = ?`,
		snapshotID, tenantID,
	).Scan(&envEncrypted)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("snapshot not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query snapshot env: %w", err)
	}

	if envEncrypted == "" {
		return map[string]string{}, nil
	}

	// Unmarshal the encrypted env blob.
	var encryptedMap map[string]string
	if err := json.Unmarshal([]byte(envEncrypted), &encryptedMap); err != nil {
		return nil, fmt.Errorf("unmarshal env blob: %w", err)
	}

	// Decrypt each value.
	plaintext := make(map[string]string, len(encryptedMap))
	for k, cipherHex := range encryptedMap {
		decrypted, err := crypto.Decrypt(cipherHex, m.masterKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt env var %q: %w", k, err)
		}
		plaintext[k] = string(decrypted)
	}

	return plaintext, nil
}

// GetEnvKeys returns the env var keys stored in a snapshot with masked values.
// This allows callers to see which env vars exist without revealing their values.
func (m *Manager) GetEnvKeys(ctx context.Context, tenantID, snapshotID string) (map[string]string, error) {
	var envEncrypted string
	err := m.db.QueryRowContext(ctx,
		`SELECT env_encrypted FROM snapshots WHERE id = ? AND tenant_id = ?`,
		snapshotID, tenantID,
	).Scan(&envEncrypted)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("snapshot not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query snapshot env keys: %w", err)
	}

	if envEncrypted == "" {
		return map[string]string{}, nil
	}

	// Unmarshal just to extract keys — values stay encrypted and are not returned.
	var encryptedMap map[string]string
	if err := json.Unmarshal([]byte(envEncrypted), &encryptedMap); err != nil {
		return nil, fmt.Errorf("unmarshal env blob: %w", err)
	}

	masked := make(map[string]string, len(encryptedMap))
	for k := range encryptedMap {
		masked[k] = "********"
	}
	return masked, nil
}

// CleanExpired removes snapshots that exceed per-service count or age limits.
// It deletes the oldest snapshots first when over the count limit and removes
// any snapshot older than maxAge. Returns the number of snapshots removed.
func (m *Manager) CleanExpired(ctx context.Context, maxPerService int, maxAge time.Duration) (int, error) {
	var removed int

	// 1. Enforce per-service count limit (delete oldest when over limit).
	if maxPerService > 0 {
		n, err := m.cleanByCount(ctx, maxPerService)
		if err != nil {
			return removed, fmt.Errorf("clean by count: %w", err)
		}
		removed += n
	}

	// 2. Enforce age limit (delete snapshots older than maxAge).
	if maxAge > 0 {
		n, err := m.cleanByAge(ctx, maxAge)
		if err != nil {
			return removed, fmt.Errorf("clean by age: %w", err)
		}
		removed += n
	}

	return removed, nil
}

// cleanByCount finds services with more than maxPerService snapshots and
// deletes the oldest ones exceeding the limit.
func (m *Manager) cleanByCount(ctx context.Context, maxPerService int) (int, error) {
	// Find services that exceed the snapshot count limit.
	rows, err := m.db.QueryContext(ctx,
		`SELECT service_id, COUNT(*) as cnt FROM snapshots GROUP BY service_id HAVING cnt > ?`,
		maxPerService,
	)
	if err != nil {
		return 0, fmt.Errorf("query over-limit services: %w", err)
	}
	defer rows.Close()

	type overLimit struct {
		serviceID string
		count     int
	}
	var services []overLimit
	for rows.Next() {
		var ol overLimit
		if err := rows.Scan(&ol.serviceID, &ol.count); err != nil {
			return 0, fmt.Errorf("scan over-limit row: %w", err)
		}
		services = append(services, ol)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate over-limit rows: %w", err)
	}

	var removed int
	for _, svc := range services {
		excess := svc.count - maxPerService
		// Select the oldest snapshots beyond the limit.
		snapRows, err := m.db.QueryContext(ctx,
			`SELECT id, tenant_id, image_ref FROM snapshots
			 WHERE service_id = ?
			 ORDER BY created_at ASC
			 LIMIT ?`,
			svc.serviceID, excess,
		)
		if err != nil {
			log.Printf("gc/snapshots: failed to query excess snapshots for service %s: %v", svc.serviceID, err)
			continue
		}

		type snapInfo struct {
			id       string
			tenantID string
			imageRef string
		}
		var toDelete []snapInfo
		for snapRows.Next() {
			var s snapInfo
			if err := snapRows.Scan(&s.id, &s.tenantID, &s.imageRef); err != nil {
				log.Printf("gc/snapshots: failed to scan snapshot row: %v", err)
				continue
			}
			toDelete = append(toDelete, s)
		}
		snapRows.Close()

		for _, s := range toDelete {
			if err := m.deleteSnapshot(ctx, s.id, s.tenantID, s.imageRef); err != nil {
				log.Printf("gc/snapshots: failed to delete snapshot %s: %v", s.id, err)
				continue
			}
			removed++
		}
	}

	return removed, nil
}

// cleanByAge deletes all snapshots older than maxAge.
func (m *Manager) cleanByAge(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge).Unix()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, image_ref FROM snapshots WHERE created_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("query expired snapshots: %w", err)
	}
	defer rows.Close()

	type snapInfo struct {
		id       string
		tenantID string
		imageRef string
	}
	var toDelete []snapInfo
	for rows.Next() {
		var s snapInfo
		if err := rows.Scan(&s.id, &s.tenantID, &s.imageRef); err != nil {
			log.Printf("gc/snapshots: failed to scan expired snapshot: %v", err)
			continue
		}
		toDelete = append(toDelete, s)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired rows: %w", err)
	}

	var removed int
	for _, s := range toDelete {
		if err := m.deleteSnapshot(ctx, s.id, s.tenantID, s.imageRef); err != nil {
			log.Printf("gc/snapshots: failed to delete expired snapshot %s: %v", s.id, err)
			continue
		}
		removed++
	}

	return removed, nil
}

// deleteSnapshot removes a snapshot's Docker image and DB row.
func (m *Manager) deleteSnapshot(ctx context.Context, id, tenantID, imageRef string) error {
	// Best-effort image removal.
	if imageRef != "" {
		if err := m.docker.RemoveImage(ctx, imageRef); err != nil {
			log.Printf("gc/snapshots: WARNING: failed to remove image %s: %v", imageRef, err)
		}
	}

	_, err := m.db.ExecContext(ctx,
		`DELETE FROM snapshots WHERE id = ? AND tenant_id = ?`,
		id, tenantID,
	)
	if err != nil {
		return fmt.Errorf("delete snapshot row %s: %w", id, err)
	}

	return nil
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
