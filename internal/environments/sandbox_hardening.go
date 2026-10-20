package environments

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
)

// Phase 2 sandbox hardening: opt-in internet egress and hard workspace size
// limits. Both are off unless the operator configures them, and then the
// sandbox behaviour is exactly phase 1's.

// Sandbox egress modes.
const (
	SandboxEgressNone     = "none"
	SandboxEgressInternet = "internet"
)

// Workspace (tmpfs) limits, in MB.
const (
	sandboxMinDiskMB = 16
	// SandboxMaxDiskMBCap is the largest max_sandbox_disk_mb an admin can set.
	SandboxMaxDiskMBCap = 8192
)

var (
	errEgressUnavailable = apierr.Forbidden("egress \"internet\" is not available on this server")
	errEgressNotAllowed  = apierr.Forbidden("egress \"internet\" is not enabled for this tenant")
)

// SetSandboxEgressNetwork enables egress "internet" for tenants with
// sandbox_egress_allowed=1: such sandboxes attach only to this network. The
// caller (cmd/ah) sets it only after creating the egress bridge and checking
// the host firewall rules. Empty disables egress (403 on request).
func (m *Manager) SetSandboxEgressNetwork(name string) {
	m.sandboxEgressNetwork = name
}

// DeleteEgressSandboxesIfDisabled runs once at startup, after
// SetSandboxEgressNetwork and before the API serves requests. When egress is
// disabled (not configured, or its host firewall rules could not be
// verified), every sandbox with egress "internet" is deleted: one still
// running would have internet access with no verified filter in front of it.
// With egress enabled it does nothing. It returns the number deleted; a
// sandbox whose teardown fails stays 'deleting' and the reaper retries it.
func (m *Manager) DeleteEgressSandboxesIfDisabled(ctx context.Context) (int, error) {
	if m.sandboxEgressNetwork != "" {
		return 0, nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, status FROM environments WHERE kind = 'sandbox' AND egress = ?`, SandboxEgressInternet)
	if err != nil {
		return 0, fmt.Errorf("select egress sandboxes: %w", err)
	}
	type candidate struct{ id, tenantID, status string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.tenantID, &c.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan egress sandbox: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	deleted := 0
	var firstErr error
	for _, c := range candidates {
		res, err := m.db.ExecContext(ctx,
			`UPDATE environments SET status = 'deleting', updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND kind = 'sandbox' AND status = ?`,
			time.Now().Unix(), c.id, c.tenantID, c.status)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim egress sandbox %s: %w", c.id, err)
			}
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // changed since the select
		}
		if err := m.teardownSandbox(ctx, c.tenantID, c.id); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete egress sandbox %s (the reaper retries): %w", c.id, err)
			}
			continue
		}
		log.Printf("AUDIT: action=sandbox.egress_disabled_delete tenant=%s sandbox=%s prior_status=%s", c.tenantID, c.id, c.status)
		deleted++
	}
	return deleted, firstErr
}

// SetSandboxRuntime sets the OCI runtime sandboxes run under (for example
// runsc-sbx, registered with --overlay2=root:memory). Empty uses the Docker
// client's runtime, like environments. cmd/ah checks the runtime is
// registered with Docker before calling this.
func (m *Manager) SetSandboxRuntime(runtime string) {
	m.sandboxRuntime = runtime
}

// SetSandboxWorkspaceTmpfs makes each sandbox's /workspace a tmpfs of its
// disk_mb instead of a named volume, so workspace writes are bounded.
func (m *Manager) SetSandboxWorkspaceTmpfs(enabled bool) {
	m.sandboxWorkspaceTmpfs = enabled
}

// parseSandboxEgress validates the requested egress mode ("" = none).
func parseSandboxEgress(egress string) (string, error) {
	switch egress {
	case "", SandboxEgressNone:
		return SandboxEgressNone, nil
	case SandboxEgressInternet:
		return SandboxEgressInternet, nil
	default:
		return "", apierr.Validation(fmt.Sprintf("unknown egress %q (want none or internet)", egress))
	}
}

// sandboxEgressGate decides whether a sandbox may have the requested
// egress, and returns the network to attach it to instead of the tenant
// network ("" = the tenant network). Egress "internet" needs both the
// operator's egress network and the tenant's sandbox_egress_allowed=1;
// otherwise it is 403.
func (m *Manager) sandboxEgressGate(egress string, q *sandboxQuota) (string, error) {
	if egress != SandboxEgressInternet {
		return "", nil
	}
	if m.sandboxEgressNetwork == "" {
		return "", errEgressUnavailable
	}
	if !q.egressAllowed {
		return "", errEgressNotAllowed
	}
	return m.sandboxEgressNetwork, nil
}

// resolveSandboxDisk returns the tmpfs /workspace size for a sandbox with
// memoryMB of memory, or 0 when the server keeps the volume workspace.
//
// The tmpfs is charged to the sandbox's memory cgroup, so disk_mb can never
// exceed memory_mb; it is also capped by the tenant's max_sandbox_disk_mb.
// The default is half the memory, within that cap, so a default sandbox
// keeps memory for its processes.
func (m *Manager) resolveSandboxDisk(requested *int, memoryMB int, q *sandboxQuota) (int, error) {
	if !m.sandboxWorkspaceTmpfs {
		if requested != nil {
			return 0, apierr.Validation("disk_mb is not supported on this server (sandbox workspaces are volumes without a size limit)")
		}
		return 0, nil
	}
	maxDisk := memoryMB
	if q.maxDiskMB < maxDisk {
		maxDisk = q.maxDiskMB
	}
	if maxDisk < sandboxMinDiskMB {
		return 0, apierr.Validation("tenant quota is too small for a sandbox workspace")
	}
	if requested != nil {
		if *requested < sandboxMinDiskMB || *requested > maxDisk {
			return 0, apierr.Validation(fmt.Sprintf("disk_mb must be between %d and %d (at most memory_mb and the tenant's max_sandbox_disk_mb)", sandboxMinDiskMB, maxDisk))
		}
		return *requested, nil
	}
	disk := memoryMB / 2
	if disk > maxDisk {
		disk = maxDisk
	}
	if disk < sandboxMinDiskMB {
		disk = sandboxMinDiskMB
	}
	return disk, nil
}

// LogSandboxHardening prints the effective phase 2 settings once at startup.
func (m *Manager) LogSandboxHardening() {
	egress := "disabled"
	if m.sandboxEgressNetwork != "" {
		egress = "network " + m.sandboxEgressNetwork
	}
	runtime := m.sandboxRuntime
	if runtime == "" {
		runtime = "(container runtime)"
	}
	log.Printf("sandboxes: egress %s; runtime %s; workspace tmpfs %t", egress, runtime, m.sandboxWorkspaceTmpfs)
}
