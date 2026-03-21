package services

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// DeployImage deploys a pre-built image (from nixpacks build) for a service.
// Similar to Deploy but skips image pull (image is already local or in registry).
// buildID links this deployment to the originating build record (may be empty).
func (m *Manager) DeployImage(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error {
	if m.docker == nil {
		return fmt.Errorf("docker client not configured")
	}
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}

	// Acquire deploy slot
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		return fmt.Errorf("deploy queue full; try again later")
	}

	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// Check disk space before deploy
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return fmt.Errorf("disk check: %w", err)
	}

	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")

	// Record deployment event.
	deployNow := time.Now().Unix()
	deployID := m.recordDeployment(ctx, &deployments.Deployment{
		ServiceID: serviceID,
		TenantID:  tenantID,
		BuildID:   buildID,
		Image:     imageTag,
		Status:    deployments.StatusDeploying,
		Trigger:   deployments.TriggerBuild,
		StartedAt: deployNow,
	})

	// Stop and remove old container if exists
	if svc.ContainerID != "" {
		_ = m.docker.StopContainer(ctx, svc.ContainerID)
		_ = m.docker.RemoveContainer(ctx, svc.ContainerID)
	}

	// Ensure per-tenant network
	_, err = m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID))
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("network setup failed: %v", err))
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(fmt.Sprintf("network setup failed: %v", err)), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	// Update the service image to the built one
	_, err = m.db.ExecContext(ctx,
		`UPDATE services SET image = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		imageTag, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(fmt.Sprintf("update service image: %v", err)), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("update service image: %w", err)
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("env vars load failed: %v", err))
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(fmt.Sprintf("env vars load failed: %v", err)), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := svc.Port
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		var parsed int
		if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}

	limits := m.getResourceLimits(ctx, tenantID)

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, imageTag, port, envVars, traefikLabels(serviceID, svc.DNSLabel, m.baseDomain, port), limits)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("container start failed: %v", err))
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(fmt.Sprintf("container start failed: %v", err)), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("run container: %w", err)
	}

	now := time.Now().Unix()
	url := publicURL(serviceID, svc.DNSLabel, m.baseDomain)
	res, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = 'running', container_id = ?, url = ?, last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		containerID, url, now, serviceID, tenantID,
	)
	if err != nil {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(fmt.Sprintf("db update failed: %v", err)), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("WARNING: service %s was deleted during deploy; removing orphan container %s", serviceID, containerID)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError("service deleted during deploy"), deployments.WithCompletedAt(time.Now().Unix()))
		return fmt.Errorf("service deleted during deploy")
	}

	m.updateDeploymentStatus(ctx, deployID, deployments.StatusRunning, deployments.WithContainerID(containerID), deployments.WithCompletedAt(time.Now().Unix()))

	// Write Traefik file-provider route (non-fatal on error)
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s: %v", serviceID, err)
	}

	return nil
}
