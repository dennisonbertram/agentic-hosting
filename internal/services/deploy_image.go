package services

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
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

	release, err := m.acquireDeploySlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Check disk space before deploy
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return errDiskLow(err)
	}

	// LOCK ORDER: deployQueue -> deploySem -> lockService (same as Deploy/Restart).
	unlock := m.lockService(serviceID)
	defer unlock()

	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.CircuitOpen {
		return apierr.Conflict("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}

	m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")

	// Record deployment event.
	deployID := m.recordDeployment(ctx, &deployments.Deployment{
		ServiceID: serviceID,
		TenantID:  tenantID,
		BuildID:   buildID,
		Image:     imageTag,
		Status:    deployments.StatusDeploying,
		Trigger:   deployments.TriggerBuild,
		StartedAt: time.Now().Unix(),
	})

	// Stop and remove the old container, if any.
	if err := m.removeOldContainer(ctx, svc); err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("remove old container failed: %v", err))
		return fmt.Errorf("remove old container: %w", err)
	}

	// Ensure per-tenant network
	if _, err := m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID)); err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("network setup failed: %v", err))
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	// Update the service image to the built one
	_, err = m.db.ExecContext(ctx,
		`UPDATE services SET image = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		imageTag, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("update service image: %v", err))
		return fmt.Errorf("update service image: %w", err)
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("env vars load failed: %v", err))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := resolvePort(svc.Port, envVars)

	// Re-check the tenant under the service lock right before creating the
	// container: a suspension during the build or semaphore wait must not get
	// a fresh container.
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		m.updateDeploymentStatus(context.WithoutCancel(ctx), deployID, deployments.StatusFailed, deployments.WithError("tenant not active"), deployments.WithCompletedAt(time.Now().Unix()))
		return err
	}

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, imageTag, port, envVars, nil, m.getResourceLimits(ctx, tenantID))
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("container start failed: %v", err))
		return fmt.Errorf("run container: %w", err)
	}
	if err := m.activateContainer(ctx, tenantID, svc, containerID, deployID, envVars); err != nil {
		return err
	}

	// Write Traefik file-provider route (non-fatal on error). No re-read of the
	// row is needed first: INVARIANT: every deletion of a services row goes
	// through Manager.Delete, which takes the service lock held here, so the
	// service cannot vanish before the route lands. A new path that deletes
	// service rows must take lockService too, or reinstate a getOwned check here.
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s: %v", serviceID, err)
	}

	return nil
}
