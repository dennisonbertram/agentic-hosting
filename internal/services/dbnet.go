package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// attachDatabaseNetworks connects a service container to each of this tenant's
// database networks whose DNS alias appears in the service environment.
// Other tenants' databases are never considered, so a copied connection string
// cannot join another tenant's network.
func (m *Manager) attachDatabaseNetworks(ctx context.Context, tenantID, containerID string, env map[string]string) error {
	if containerID == "" {
		return nil
	}
	ids, err := m.tenantDatabaseIDs(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, dbID := range ids {
		if !envMentionsDatabase(env, dbID) {
			continue
		}
		netName := docker.DatabaseNetworkName(tenantID, dbID)
		netID, err := m.docker.EnsureDatabaseNetwork(ctx, netName)
		if err != nil {
			return fmt.Errorf("ensure database network %s: %w", netName, err)
		}
		if err := m.docker.ConnectNetwork(ctx, netID, containerID); err != nil {
			return fmt.Errorf("connect %s to database network %s: %w", containerID, netName, err)
		}
	}
	return nil
}

func (m *Manager) tenantDatabaseIDs(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id FROM databases WHERE tenant_id = ? AND status != 'failed'`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list tenant databases: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan tenant database: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func envMentionsDatabase(env map[string]string, dbID string) bool {
	alias := docker.DatabaseAlias(dbID)
	for _, v := range env {
		if strings.Contains(v, alias) {
			return true
		}
	}
	return false
}
