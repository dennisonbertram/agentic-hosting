package environments

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// StopAllForTenant stops active environment containers and removes their
// Traefik files while keeping workspace and preview records for reactivation.
func (m *Manager) StopAllForTenant(_ context.Context, tenantID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id, status FROM environments WHERE tenant_id = ?`, tenantID)
	if err != nil {
		log.Printf("environments: list tenant %s for suspension: %v", tenantID, err)
		return
	}
	type envEntry struct{ id, containerID, status string }
	var entries []envEntry
	for rows.Next() {
		var entry envEntry
		if err := rows.Scan(&entry.id, &entry.containerID, &entry.status); err != nil {
			log.Printf("environments: scan tenant %s for suspension: %v", tenantID, err)
			continue
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("environments: read tenant %s for suspension: %v", tenantID, err)
	}
	_ = rows.Close()

	for _, entry := range entries {
		m.cleanupPreviews(ctx, entry.id)
		// Keep tenant and preview ingress networks while the stopped container
		// still references them. Docker removes the stopped endpoint, but start
		// cannot recover if these network IDs are deleted; GC reclaims them after
		// the environment record and preview ownership are removed.
		if entry.status != "running" && entry.status != "creating" {
			continue
		}
		if entry.containerID != "" {
			if err := m.docker.StopContainer(ctx, entry.containerID); err != nil && !isNotFoundError(err) {
				log.Printf("environments: stop container %s for suspended tenant %s: %v", entry.containerID, tenantID, err)
				continue
			}
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE environments SET status = 'stopped', updated_at = ?
			 WHERE id = ? AND tenant_id = ?`, time.Now().Unix(), entry.id, tenantID); err != nil {
			log.Printf("environments: mark %s stopped for suspended tenant %s: %v", entry.id, tenantID, err)
		}
	}
}

func (m *Manager) removeCreatedEnvironment(containerID, volumeName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if containerID != "" {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
	}
	if volumeName != "" {
		_ = m.docker.RemoveVolume(ctx, volumeName)
	}
}

// restorePreviews re-enables persisted preview routes after an environment is
// started again following tenant reactivation.
func (m *Manager) restorePreviews(ctx context.Context, envID, tenantID string) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, dns_label, port FROM environment_previews WHERE environment_id = ? AND tenant_id = ?`,
		envID, tenantID)
	if err != nil {
		return fmt.Errorf("list previews to restore for %s: %w", envID, err)
	}
	type previewRoute struct {
		id, dnsLabel string
		port         int
	}
	var routes []previewRoute
	for rows.Next() {
		var route previewRoute
		if err := rows.Scan(&route.id, &route.dnsLabel, &route.port); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan preview to restore for %s: %w", envID, err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read previews to restore for %s: %w", envID, err)
	}
	_ = rows.Close()
	containerName := docker.EnvironmentIngressAlias(tenantID, envID)
	for _, route := range routes {
		if err := m.writePreviewTraefikRoute(route.id, route.dnsLabel, containerName, route.port); err != nil {
			return fmt.Errorf("restore preview route %s: %w", route.id, err)
		}
	}
	return nil
}
