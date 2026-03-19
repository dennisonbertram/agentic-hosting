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
	var resourceConfig string
	var maxMemoryMB sql.NullInt64
	var maxCPUCores sql.NullFloat64
	err = m.db.QueryRowContext(ctx,
		`SELECT max_memory_mb, max_cpu_cores FROM tenant_quotas WHERE tenant_id = ?`,
		tenantID,
	).Scan(&maxMemoryMB, &maxCPUCores)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query tenant quotas: %w", err)
	}
	if err == nil {
		rcJSON, err := json.Marshal(map[string]interface{}{
			"max_memory_mb": maxMemoryMB.Int64,
			"max_cpu_cores": maxCPUCores.Float64,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal resource config: %w", err)
		}
		resourceConfig = string(rcJSON)
	}

	now := time.Now().Unix()

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshotID, tenantID, serviceID, req.Name, req.Description, imageRef, string(envJSON), resourceConfig, svcPort, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert snapshot: %w", err)
	}

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

// List returns snapshots for a tenant with pagination.
func (m *Manager) List(ctx context.Context, tenantID string, limit, offset int) ([]*Snapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, service_id, name, description, image_ref, resource_config, port, created_at
		 FROM snapshots WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
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
		log.Printf("WARNING: snapshot image %s cleanup skipped (image removal not implemented in docker client)", snap.ImageRef)
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
// snapshot. This is used internally during create-from-snapshot flows.
func (m *Manager) RestoreEnvVars(ctx context.Context, snapshotID string) (map[string]string, error) {
	var envEncrypted string
	err := m.db.QueryRowContext(ctx,
		`SELECT env_encrypted FROM snapshots WHERE id = ?`,
		snapshotID,
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

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
