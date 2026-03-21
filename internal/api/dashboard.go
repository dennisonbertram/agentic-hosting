package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
)

type ActivityEvent struct {
	ID           string `json:"id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name,omitempty"`
	Action       string `json:"action"`
	Status       string `json:"status,omitempty"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"created_at"`
}

func (s *Server) handleActivityList(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		if value > 200 {
			value = 200
		}
		limit = value
	}

	events, err := s.listActivityEvents(r.Context(), tenantID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list activity")
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) listActivityEvents(ctx context.Context, tenantID string, limit int) ([]ActivityEvent, error) {
	events := make([]ActivityEvent, 0, limit)

	if err := s.appendTenantEvents(ctx, tenantID, &events); err != nil {
		return nil, err
	}
	if err := s.appendServiceEvents(ctx, tenantID, &events); err != nil {
		return nil, err
	}
	if err := s.appendBuildEvents(ctx, tenantID, &events); err != nil {
		return nil, err
	}
	if err := s.appendDatabaseEvents(ctx, tenantID, &events); err != nil {
		return nil, err
	}
	if err := s.appendAPIKeyEvents(ctx, tenantID, &events); err != nil {
		return nil, err
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt == events[j].CreatedAt {
			return events[i].ID > events[j].ID
		}
		return events[i].CreatedAt > events[j].CreatedAt
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (s *Server) appendTenantEvents(ctx context.Context, tenantID string, events *[]ActivityEvent) error {
	var id, name, status string
	var createdAt, updatedAt int64
	err := s.store.StateDB.QueryRowContext(ctx,
		`SELECT id, name, status, created_at, updated_at FROM tenants WHERE id = ?`,
		tenantID,
	).Scan(&id, &name, &status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tenant activity: %w", err)
	}

	*events = append(*events, ActivityEvent{
		ID:           fmt.Sprintf("tenant-created-%s-%d", id, createdAt),
		ResourceType: "tenant",
		ResourceID:   id,
		ResourceName: name,
		Action:       "tenant.created",
		Status:       status,
		Message:      fmt.Sprintf("Tenant %s was created", name),
		CreatedAt:    createdAt,
	})
	if updatedAt > createdAt {
		action := "tenant.updated"
		message := fmt.Sprintf("Tenant %s was updated", name)
		if status != "active" {
			action = "tenant.suspended"
			message = fmt.Sprintf("Tenant %s was suspended", name)
		}
		*events = append(*events, ActivityEvent{
			ID:           fmt.Sprintf("tenant-updated-%s-%d", id, updatedAt),
			ResourceType: "tenant",
			ResourceID:   id,
			ResourceName: name,
			Action:       action,
			Status:       status,
			Message:      message,
			CreatedAt:    updatedAt,
		})
	}
	return nil
}

func (s *Server) appendServiceEvents(ctx context.Context, tenantID string, events *[]ActivityEvent) error {
	rows, err := s.store.StateDB.QueryContext(ctx,
		`SELECT id, name, status, COALESCE(last_error, ''), circuit_open, created_at, updated_at
		 FROM services
		 WHERE tenant_id = ?`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("service activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, name, status, lastError string
		var circuitOpen int
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &name, &status, &lastError, &circuitOpen, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan service activity: %w", err)
		}

		*events = append(*events, ActivityEvent{
			ID:           fmt.Sprintf("service-created-%s-%d", id, createdAt),
			ResourceType: "service",
			ResourceID:   id,
			ResourceName: name,
			Action:       "service.created",
			Status:       status,
			Message:      fmt.Sprintf("Service %s was created", name),
			CreatedAt:    createdAt,
		})

		if updatedAt > createdAt {
			action := "service.updated"
			message := fmt.Sprintf("Service %s changed state to %s", name, status)
			switch {
			case circuitOpen != 0:
				action = "service.circuit_open"
				message = fmt.Sprintf("Circuit breaker opened for service %s", name)
			case status == "failed" && lastError != "":
				action = "service.failed"
				message = fmt.Sprintf("Service %s failed: %s", name, lastError)
			case status == "running":
				action = "service.running"
				message = fmt.Sprintf("Service %s is running", name)
			case status == "stopped":
				action = "service.stopped"
				message = fmt.Sprintf("Service %s is stopped", name)
			case status == "deploying":
				action = "service.deploying"
				message = fmt.Sprintf("Service %s is deploying", name)
			}
			*events = append(*events, ActivityEvent{
				ID:           fmt.Sprintf("service-updated-%s-%d", id, updatedAt),
				ResourceType: "service",
				ResourceID:   id,
				ResourceName: name,
				Action:       action,
				Status:       status,
				Message:      message,
				CreatedAt:    updatedAt,
			})
		}
	}
	return rows.Err()
}

func (s *Server) appendBuildEvents(ctx context.Context, tenantID string, events *[]ActivityEvent) error {
	rows, err := s.store.StateDB.QueryContext(ctx,
		`SELECT b.id, b.service_id, COALESCE(s.name, ''), b.status, COALESCE(b.source_ref, ''), COALESCE(b.source_url, ''),
		        b.created_at, b.started_at, b.finished_at
		 FROM builds b
		 LEFT JOIN services s ON s.id = b.service_id
		 WHERE b.tenant_id = ?`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("build activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, serviceID, serviceName, status, sourceRef, sourceURL string
		var createdAt int64
		var startedAt, finishedAt sql.NullInt64
		if err := rows.Scan(&id, &serviceID, &serviceName, &status, &sourceRef, &sourceURL, &createdAt, &startedAt, &finishedAt); err != nil {
			return fmt.Errorf("scan build activity: %w", err)
		}

		refSuffix := ""
		if sourceRef != "" {
			refSuffix = fmt.Sprintf(" from %s", sourceRef)
		}

		*events = append(*events, ActivityEvent{
			ID:           fmt.Sprintf("build-created-%s-%d", id, createdAt),
			ResourceType: "build",
			ResourceID:   id,
			ResourceName: serviceName,
			Action:       "build.queued",
			Status:       status,
			Message:      fmt.Sprintf("Build queued for %s%s", defaultName(serviceName, serviceID), refSuffix),
			CreatedAt:    createdAt,
		})

		if startedAt.Valid {
			*events = append(*events, ActivityEvent{
				ID:           fmt.Sprintf("build-started-%s-%d", id, startedAt.Int64),
				ResourceType: "build",
				ResourceID:   id,
				ResourceName: serviceName,
				Action:       "build.started",
				Status:       status,
				Message:      fmt.Sprintf("Build started for %s", defaultName(serviceName, serviceID)),
				CreatedAt:    startedAt.Int64,
			})
		}

		if finishedAt.Valid {
			action := "build.finished"
			message := fmt.Sprintf("Build finished for %s", defaultName(serviceName, serviceID))
			if status == "succeeded" {
				action = "build.succeeded"
				message = fmt.Sprintf("Build succeeded for %s", defaultName(serviceName, serviceID))
			} else if status == "failed" {
				action = "build.failed"
				message = fmt.Sprintf("Build failed for %s", defaultName(serviceName, serviceID))
				if sourceURL != "" {
					message = fmt.Sprintf("Build failed for %s (%s)", defaultName(serviceName, serviceID), sourceURL)
				}
			}
			*events = append(*events, ActivityEvent{
				ID:           fmt.Sprintf("build-finished-%s-%d", id, finishedAt.Int64),
				ResourceType: "build",
				ResourceID:   id,
				ResourceName: serviceName,
				Action:       action,
				Status:       status,
				Message:      message,
				CreatedAt:    finishedAt.Int64,
			})
		}
	}
	return rows.Err()
}

func (s *Server) appendDatabaseEvents(ctx context.Context, tenantID string, events *[]ActivityEvent) error {
	rows, err := s.store.StateDB.QueryContext(ctx,
		`SELECT id, name, type, status, created_at, updated_at
		 FROM databases
		 WHERE tenant_id = ?`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("database activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, name, dbType, status string
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &name, &dbType, &status, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan database activity: %w", err)
		}

		*events = append(*events, ActivityEvent{
			ID:           fmt.Sprintf("database-created-%s-%d", id, createdAt),
			ResourceType: "database",
			ResourceID:   id,
			ResourceName: name,
			Action:       "database.created",
			Status:       status,
			Message:      fmt.Sprintf("%s database %s provisioning started", titleCase(dbType), name),
			CreatedAt:    createdAt,
		})

		if updatedAt > createdAt {
			action := "database.updated"
			message := fmt.Sprintf("Database %s changed state to %s", name, status)
			if status == "ready" {
				action = "database.ready"
				message = fmt.Sprintf("%s database %s is ready", titleCase(dbType), name)
			} else if status == "failed" {
				action = "database.failed"
				message = fmt.Sprintf("%s database %s failed", titleCase(dbType), name)
			}
			*events = append(*events, ActivityEvent{
				ID:           fmt.Sprintf("database-updated-%s-%d", id, updatedAt),
				ResourceType: "database",
				ResourceID:   id,
				ResourceName: name,
				Action:       action,
				Status:       status,
				Message:      message,
				CreatedAt:    updatedAt,
			})
		}
	}
	return rows.Err()
}

func (s *Server) appendAPIKeyEvents(ctx context.Context, tenantID string, events *[]ActivityEvent) error {
	rows, err := s.store.StateDB.QueryContext(ctx,
		`SELECT id, name, created_at, revoked_at
		 FROM api_keys
		 WHERE tenant_id = ?`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("api key activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, name string
		var createdAt int64
		var revokedAt sql.NullInt64
		if err := rows.Scan(&id, &name, &createdAt, &revokedAt); err != nil {
			return fmt.Errorf("scan api key activity: %w", err)
		}

		*events = append(*events, ActivityEvent{
			ID:           fmt.Sprintf("api-key-created-%s-%d", id, createdAt),
			ResourceType: "api_key",
			ResourceID:   id,
			ResourceName: name,
			Action:       "api_key.created",
			Message:      fmt.Sprintf("API key %s was created", name),
			CreatedAt:    createdAt,
		})
		if revokedAt.Valid {
			*events = append(*events, ActivityEvent{
				ID:           fmt.Sprintf("api-key-revoked-%s-%d", id, revokedAt.Int64),
				ResourceType: "api_key",
				ResourceID:   id,
				ResourceName: name,
				Action:       "api_key.revoked",
				Message:      fmt.Sprintf("API key %s was revoked", name),
				CreatedAt:    revokedAt.Int64,
			})
		}
	}
	return rows.Err()
}

func defaultName(name, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}

func titleCase(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
