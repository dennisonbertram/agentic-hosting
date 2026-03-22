// Package deployments manages deployment event records for services.
package deployments

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
)

// Status constants for deployment lifecycle.
const (
	StatusPending   = "pending"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusFailed    = "failed"
	StatusCrashed   = "crashed"
	StatusStopped   = "stopped"
	StatusCancelled = "cancelled"
)

// Trigger constants for deployment attribution.
const (
	TriggerManual       = "manual"
	TriggerBuild        = "build"
	TriggerRestart      = "restart"
	TriggerReconciler   = "reconciler"
	TriggerAutoRecovery = "auto_recovery"
)

// Deployment represents a single deployment event for a service.
type Deployment struct {
	ID           string `json:"id"`
	ServiceID    string `json:"service_id"`
	TenantID     string `json:"tenant_id"`
	BuildID      string `json:"build_id,omitempty"`
	Image        string `json:"image"`
	Status       string `json:"status"`
	Trigger      string `json:"trigger"`
	ContainerID  string `json:"container_id,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	StartedAt    int64  `json:"started_at"`
	CompletedAt  *int64 `json:"completed_at,omitempty"`
	CancelledAt  *int64 `json:"cancelled_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// Store handles deployment record persistence.
type Store struct {
	db *sql.DB
}

// NewStore creates a deployment store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Create inserts a new deployment record.
func (s *Store) Create(ctx context.Context, d *Deployment) error {
	if d.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		d.ID = id
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().Unix()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deployments (id, service_id, tenant_id, build_id, image, status, trigger, container_id, error_message, started_at, completed_at, cancelled_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.ServiceID, d.TenantID, nullString(d.BuildID), d.Image, d.Status, d.Trigger,
		d.ContainerID, d.ErrorMessage, d.StartedAt, d.CompletedAt, d.CancelledAt, d.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert deployment: %w", err)
	}
	return nil
}

// updateOpts holds optional fields for UpdateStatus.
type updateOpts struct {
	containerID  *string
	errorMessage *string
	completedAt  *int64
}

// UpdateOption configures optional fields on UpdateStatus.
type UpdateOption func(*updateOpts)

// WithContainerID sets the container_id on status update.
func WithContainerID(cid string) UpdateOption {
	return func(o *updateOpts) {
		o.containerID = &cid
	}
}

// WithError sets the error_message on status update.
func WithError(msg string) UpdateOption {
	return func(o *updateOpts) {
		o.errorMessage = &msg
	}
}

// WithCompletedAt sets the completed_at timestamp on status update.
func WithCompletedAt(ts int64) UpdateOption {
	return func(o *updateOpts) {
		o.completedAt = &ts
	}
}

// UpdateStatus transitions a deployment to a new status, optionally setting
// container_id, error_message, and completed_at.
func (s *Store) UpdateStatus(ctx context.Context, deploymentID, status string, opts ...UpdateOption) error {
	o := &updateOpts{}
	for _, fn := range opts {
		fn(o)
	}

	query := `UPDATE deployments SET status = ?`
	args := []interface{}{status}

	if o.containerID != nil {
		query += `, container_id = ?`
		args = append(args, *o.containerID)
	}
	if o.errorMessage != nil {
		query += `, error_message = ?`
		args = append(args, *o.errorMessage)
	}
	if o.completedAt != nil {
		query += `, completed_at = ?`
		args = append(args, *o.completedAt)
	}

	query += ` WHERE id = ?`
	args = append(args, deploymentID)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update deployment status: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return apierr.NotFound("deployment not found")
	}
	return nil
}

// Cancel transitions a deployment to cancelled status. Only deployments with
// status "pending" or "deploying" can be cancelled. Returns a Conflict error
// if the deployment is already in a terminal state (running, failed, crashed,
// stopped, or cancelled).
func (s *Store) Cancel(ctx context.Context, tenantID, deploymentID string) (*Deployment, error) {
	d, err := s.Get(ctx, tenantID, deploymentID)
	if err != nil {
		return nil, err
	}

	if d.Status != StatusPending && d.Status != StatusDeploying {
		return nil, apierr.Conflict(fmt.Sprintf("deployment cannot be cancelled (status: %s)", d.Status))
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, cancelled_at = ?, completed_at = ? WHERE id = ?`,
		StatusCancelled, now, now, deploymentID,
	)
	if err != nil {
		return nil, fmt.Errorf("cancel deployment: %w", err)
	}

	d.Status = StatusCancelled
	d.CancelledAt = &now
	d.CompletedAt = &now
	return d, nil
}

// Get retrieves a single deployment by ID, scoped to tenant.
func (s *Store) Get(ctx context.Context, tenantID, deploymentID string) (*Deployment, error) {
	d := &Deployment{}
	var buildID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, service_id, tenant_id, build_id, image, status, trigger, container_id, error_message, started_at, completed_at, cancelled_at, created_at
		 FROM deployments WHERE id = ? AND tenant_id = ?`,
		deploymentID, tenantID,
	).Scan(&d.ID, &d.ServiceID, &d.TenantID, &buildID, &d.Image, &d.Status, &d.Trigger,
		&d.ContainerID, &d.ErrorMessage, &d.StartedAt, &d.CompletedAt, &d.CancelledAt, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("deployment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	if buildID.Valid {
		d.BuildID = buildID.String
	}
	return d, nil
}

// ListByService returns paginated deployments for a service, newest first.
func (s *Store) ListByService(ctx context.Context, tenantID, serviceID string, limit, offset int) ([]*Deployment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, tenant_id, build_id, image, status, trigger, container_id, error_message, started_at, completed_at, cancelled_at, created_at
		 FROM deployments
		 WHERE tenant_id = ? AND service_id = ?
		 ORDER BY created_at DESC
		 LIMIT ? OFFSET ?`,
		tenantID, serviceID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list deployments by service: %w", err)
	}
	defer rows.Close()

	return scanDeployments(rows)
}

// ListByTenant returns paginated deployments across all services for a tenant, newest first.
func (s *Store) ListByTenant(ctx context.Context, tenantID string, limit, offset int) ([]*Deployment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, tenant_id, build_id, image, status, trigger, container_id, error_message, started_at, completed_at, cancelled_at, created_at
		 FROM deployments
		 WHERE tenant_id = ?
		 ORDER BY created_at DESC
		 LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list deployments by tenant: %w", err)
	}
	defer rows.Close()

	return scanDeployments(rows)
}

// LatestForService returns the most recent deployment for a service.
func (s *Store) LatestForService(ctx context.Context, serviceID string) (*Deployment, error) {
	d := &Deployment{}
	var buildID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, service_id, tenant_id, build_id, image, status, trigger, container_id, error_message, started_at, completed_at, cancelled_at, created_at
		 FROM deployments
		 WHERE service_id = ?
		 ORDER BY created_at DESC
		 LIMIT 1`,
		serviceID,
	).Scan(&d.ID, &d.ServiceID, &d.TenantID, &buildID, &d.Image, &d.Status, &d.Trigger,
		&d.ContainerID, &d.ErrorMessage, &d.StartedAt, &d.CompletedAt, &d.CancelledAt, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("no deployments found for service")
	}
	if err != nil {
		return nil, fmt.Errorf("latest deployment for service: %w", err)
	}
	if buildID.Valid {
		d.BuildID = buildID.String
	}
	return d, nil
}

// scanDeployments scans rows into a slice of Deployment pointers.
func scanDeployments(rows *sql.Rows) ([]*Deployment, error) {
	var result []*Deployment
	for rows.Next() {
		d := &Deployment{}
		var buildID sql.NullString
		if err := rows.Scan(&d.ID, &d.ServiceID, &d.TenantID, &buildID, &d.Image, &d.Status, &d.Trigger,
			&d.ContainerID, &d.ErrorMessage, &d.StartedAt, &d.CompletedAt, &d.CancelledAt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		if buildID.Valid {
			d.BuildID = buildID.String
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployments: %w", err)
	}
	// Return empty slice instead of nil for JSON serialization.
	if result == nil {
		result = []*Deployment{}
	}
	return result, nil
}

// nullString converts an empty string to sql.NullString{Valid: false},
// and a non-empty string to sql.NullString{Valid: true, String: s}.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: s}
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
