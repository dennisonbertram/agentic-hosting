// Package reconciler syncs DB state with actual Docker state.
//
// # Service crash state machine
//
// Containers run with restart policy unless-stopped, so Docker itself restarts
// a crashed container. The reconciler does not restart crashes; it observes
// them and decides when to give up.
//
// Each tick, every service with status 'running' or 'crashed', circuit_open=0
// and a container_id is inspected. Crash events seen on one tick are:
//   - RestartCount grew since the last tick (services.last_restart_count):
//     one event per restart. This is how a crash loop appears, because Docker
//     reports a looping container as 'restarting', not 'exited'.
//   - a 'running' row whose container is exited/dead, or definitively gone
//     (Docker not-found): one event. A 'crashed' row with a gone container is
//     not counted again.
//   - container running but health check 'unhealthy': one event, and the
//     reconciler restarts the container (a manual stop would never be undone
//     by unless-stopped).
//
// Transitions (all UPDATEs are compare-and-swap on status and container_id as
// read at scan time; if no row changed, a concurrent deploy won and no Docker
// action is taken):
//
//	running --crash event(s)--> crashed   crash_count += events in a 600s window
//	crashed --crash event(s)--> crashed   (still watched, count keeps growing)
//	any     --crash_count >= 5 in window--> circuit_open=1: container is
//	        stopped and removed; row leaves the scan until recovery
//	crashed --no events, Docker running and not unhealthy--> running
//	circuit_open --circuit_retry_at elapsed--> stopped, counters reset; the
//	        service is NOT restarted automatically (redeploy/start to resume)
//
// Known ceiling: a crash seen as 'exited' on one tick and then restarted by
// Docker is counted again by the RestartCount delta on a later tick, so the
// breaker can trip slightly early (rare: needs a tick inside the restart backoff).
package reconciler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	dockerclient "github.com/docker/docker/client"
)

const (
	crashThreshold  = 5   // crash events within the window that open the circuit
	crashWindowSecs = 600 // crash window length in seconds
)

// Reconciler periodically checks DB state against Docker state.
type Reconciler struct {
	db          *sql.DB
	docker      docker.Client
	interval    time.Duration
	deployStore *deployments.Store
}

// New creates a reconciler with the given interval.
func New(db *sql.DB, dockerClient docker.Client, interval time.Duration, deployStore *deployments.Store) *Reconciler {
	return &Reconciler{
		db:          db,
		docker:      dockerClient,
		interval:    interval,
		deployStore: deployStore,
	}
}

// recordDeployment creates a deployment record. Errors are logged but never propagated
// because deployment tracking is observability, not critical path.
func (r *Reconciler) recordDeployment(ctx context.Context, d *deployments.Deployment) {
	if r.deployStore == nil {
		return
	}
	if err := r.deployStore.Create(ctx, d); err != nil {
		log.Printf("reconciler: failed to record deployment for service %s: %v", d.ServiceID, err)
	}
}

// Run starts the reconciliation loop. Blocks until ctx is cancelled.
// Recovers from panics to keep the loop alive.
func (r *Reconciler) Run(ctx context.Context) {
	log.Printf("reconciler: starting (interval=%s)", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("reconciler: stopped")
			return
		case <-ticker.C:
			r.safeReconcile(ctx)
		}
	}
}

// ReconcileOnce runs a single reconciliation pass and returns any error.
// It is exported primarily for use in tests; production code uses Run.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	return r.reconcileOnce(ctx)
}

func (r *Reconciler) safeReconcile(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("reconciler: PANIC recovered: %v\n%s", rec, string(debug.Stack()))
		}
	}()
	// Per-tick timeout prevents a hung Docker call from stalling reconciliation forever.
	tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := r.reconcileOnce(tickCtx); err != nil {
		log.Printf("reconciler: error: %v", err)
	}
}

// isNotFoundError returns true only if Docker reported the object as not found.
// Transient errors (daemon down, timeout) return false, even if their text
// happens to contain "404".
func isNotFoundError(err error) bool {
	return err != nil && dockerclient.IsErrNotFound(err)
}

// shortID truncates a container ID for logs without panicking on short IDs.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// circuitRetryBackoff returns how long to wait before auto-recovering an open circuit,
// based on how many times it has been opened.
func circuitRetryBackoff(openCount int) time.Duration {
	switch {
	case openCount <= 1:
		return 30 * time.Minute
	case openCount == 2:
		return 1 * time.Hour
	default:
		return 4 * time.Hour
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	// Get all ah containers from Docker using the tenant label,
	// which is set on BOTH service and database containers.
	containerIDs, err := r.docker.ListContainersByLabel(ctx, "ah.tenant", "")
	if err != nil {
		// Docker daemon may be temporarily unavailable — do NOT mutate DB state.
		return fmt.Errorf("list ah containers (skipping reconciliation): %w", err)
	}

	containerSet := make(map[string]bool, len(containerIDs))
	for _, id := range containerIDs {
		containerSet[id] = true
	}

	var checked, fixed int

	// 1. Watch services marked "running" or "crashed" and apply the state machine
	// described at the top of this file.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id, tenant_id, status, crash_count, crash_window_start, circuit_open_count, last_restart_count
		 FROM services WHERE status IN ('running', 'crashed') AND circuit_open = 0 AND container_id != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type svcRecord struct {
		id, containerID, tenantID, status string
		crashCount, circuitOpenCount      int
		windowStart, lastRestartCount     sql.NullInt64
	}
	var watched []svcRecord
	for rows.Next() {
		var s svcRecord
		if err := rows.Scan(&s.id, &s.containerID, &s.tenantID, &s.status, &s.crashCount,
			&s.windowStart, &s.circuitOpenCount, &s.lastRestartCount); err != nil {
			continue
		}
		watched = append(watched, s)
	}
	rows.Close()

	for _, s := range watched {
		checked++

		info, inspectErr := r.docker.InspectContainer(ctx, s.containerID)
		gone := false
		if inspectErr != nil {
			if !isNotFoundError(inspectErr) {
				// Transient Docker error — do NOT change state.
				log.Printf("reconciler: transient Docker error for service %s (container %s), skipping: %v",
					s.id, shortID(s.containerID), inspectErr)
				continue
			}
			gone = true
		}

		crashes := 0
		crashReason := ""
		needRestart := false
		switch {
		case gone:
			if s.status == "running" {
				crashes, crashReason = 1, "container not found by reconciler"
			}
		default:
			if s.lastRestartCount.Valid && info.RestartCount > int(s.lastRestartCount.Int64) {
				crashes = info.RestartCount - int(s.lastRestartCount.Int64)
				crashReason = fmt.Sprintf("container restarted %d time(s) by Docker (state %s, exit code %d)",
					crashes, info.Status, info.ExitCode)
			}
			if info.Status == "exited" || info.Status == "dead" {
				if s.status == "running" && crashes == 0 {
					crashes = 1
				}
				if crashes > 0 {
					crashReason = fmt.Sprintf("container %s (exit code %d)", info.Status, info.ExitCode)
				}
			}
			if crashes == 0 && info.Status == "running" && info.HealthStatus == "unhealthy" {
				crashes, crashReason, needRestart = 1, "container unhealthy (health check failed)", true
			}
		}

		now := time.Now().Unix()
		newBaseline := s.lastRestartCount
		if info != nil {
			newBaseline = sql.NullInt64{Int64: int64(info.RestartCount), Valid: true}
		}

		if crashes == 0 {
			newStatus := s.status
			if s.status == "crashed" && info != nil && info.Status == "running" &&
				(info.HealthStatus == "" || info.HealthStatus == "healthy" || info.HealthStatus == "none") {
				newStatus = "running"
			}
			if newStatus == s.status && newBaseline == s.lastRestartCount {
				continue
			}
			res, err := r.db.ExecContext(ctx, `
				UPDATE services SET status = ?, last_restart_count = ?,
					updated_at = CASE WHEN status = ? THEN updated_at ELSE ? END
				WHERE id = ? AND tenant_id = ? AND status = ? AND container_id = ?`,
				newStatus, newBaseline, newStatus, now, s.id, s.tenantID, s.status, s.containerID)
			if err != nil {
				log.Printf("reconciler: failed to update service %s: %v", s.id, err)
			} else if n, _ := res.RowsAffected(); n > 0 && newStatus != s.status {
				log.Printf("reconciler: service %s recovered, container running again", s.id)
				fixed++
			}
			continue
		}

		// Crash window and circuit breaker, computed here so one CAS UPDATE sets everything.
		windowStart, crashCount := s.windowStart.Int64, s.crashCount+crashes
		if !s.windowStart.Valid || now-windowStart >= crashWindowSecs {
			windowStart, crashCount = now, crashes
		}
		open := crashCount >= crashThreshold
		openInt := 0
		if open {
			openInt = 1
		}
		res, err := r.db.ExecContext(ctx, `
			UPDATE services SET
				status = 'crashed',
				crash_window_start = ?,
				crash_count = ?,
				last_crashed_at = ?,
				last_error = ?,
				last_restart_count = ?,
				circuit_open = ?,
				circuit_retry_at = CASE WHEN ? = 1 THEN ? ELSE circuit_retry_at END,
				circuit_open_count = circuit_open_count + ?,
				updated_at = ?
			WHERE id = ? AND tenant_id = ? AND status = ? AND container_id = ?`,
			windowStart, crashCount, now, crashReason, newBaseline,
			openInt, openInt, time.Now().Add(circuitRetryBackoff(s.circuitOpenCount+1)).Unix(), openInt,
			now, s.id, s.tenantID, s.status, s.containerID)
		if err != nil {
			log.Printf("reconciler: failed to mark service %s as crashed: %v", s.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			log.Printf("reconciler: service %s changed during reconcile, skipping crash handling", s.id)
			continue
		}

		if open {
			log.Printf("reconciler: service %s circuit breaker OPEN — stopping container", s.id)
			// Stop first to defeat the restart policy, then remove.
			_ = r.docker.StopContainer(ctx, s.containerID)
			_ = r.docker.RemoveContainer(ctx, s.containerID)
		} else if needRestart {
			// A manual stop would stay stopped under unless-stopped, so restart instead.
			log.Printf("reconciler: service %s container %s is unhealthy — restarting", s.id, shortID(s.containerID))
			if rerr := r.docker.RestartContainer(ctx, s.containerID); rerr != nil {
				log.Printf("reconciler: failed to restart unhealthy container for service %s: %v", s.id, rerr)
			}
		}
		log.Printf("reconciler: service %s marked crashed (%s)", s.id, crashReason)
		// Record crash as deployment event. Need to look up the image for the record.
		var crashImage string
		r.db.QueryRowContext(ctx, `SELECT image FROM services WHERE id = ?`, s.id).Scan(&crashImage)
		crashNow := time.Now().Unix()
		r.recordDeployment(ctx, &deployments.Deployment{
			ServiceID:    s.id,
			TenantID:     s.tenantID,
			Image:        crashImage,
			Status:       deployments.StatusCrashed,
			Trigger:      deployments.TriggerReconciler,
			ErrorMessage: crashReason,
			StartedAt:    crashNow,
			CompletedAt:  &crashNow,
		})
		fixed++
	}

	// 1b. Auto-recover open circuits whose retry_at has elapsed.
	nowUnix := time.Now().Unix()
	recoveryRows, err := r.db.QueryContext(ctx,
		`SELECT id, tenant_id FROM services WHERE circuit_open = 1 AND circuit_retry_at IS NOT NULL AND circuit_retry_at <= ?`,
		nowUnix)
	if err != nil {
		log.Printf("reconciler: failed to query circuit recovery candidates: %v", err)
	} else {
		type recoverRecord struct{ id, tenantID string }
		var toRecover []recoverRecord
		for recoveryRows.Next() {
			var rec recoverRecord
			if err := recoveryRows.Scan(&rec.id, &rec.tenantID); err != nil {
				continue
			}
			toRecover = append(toRecover, rec)
		}
		recoveryRows.Close()

		for _, rec := range toRecover {
			log.Printf("reconciler: service %s circuit retry_at elapsed — resetting circuit breaker", rec.id)
			res, err := r.db.ExecContext(ctx, `
				UPDATE services SET
					circuit_open = 0,
					crash_count = 0,
					crash_window_start = NULL,
					circuit_retry_at = NULL,
					status = 'stopped',
					updated_at = ?
				WHERE id = ? AND tenant_id = ? AND circuit_open = 1`,
				time.Now().Unix(), rec.id, rec.tenantID)
			if err != nil {
				log.Printf("reconciler: failed to reset circuit for service %s: %v", rec.id, err)
				continue
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			// The container was removed when the circuit opened and nothing restarts it
			// here; the service stays stopped until it is started or redeployed.
			log.Printf("reconciler: service %s circuit reset, service left stopped (start or redeploy to resume)", rec.id)
			var recoveryImage string
			r.db.QueryRowContext(ctx, `SELECT image FROM services WHERE id = ?`, rec.id).Scan(&recoveryImage)
			recoveryNow := time.Now().Unix()
			r.recordDeployment(ctx, &deployments.Deployment{
				ServiceID:   rec.id,
				TenantID:    rec.tenantID,
				Image:       recoveryImage,
				Status:      deployments.StatusStopped,
				Trigger:     deployments.TriggerAutoRecovery,
				StartedAt:   recoveryNow,
				CompletedAt: &recoveryNow,
			})
			fixed++
		}
	}

	// 2. Check services stuck in "deploying" for > 10min
	tenMinAgo := time.Now().Add(-10 * time.Minute).Unix()

	// Query affected services before the bulk update so we can create deployment records.
	var staleServices []struct{ id, tenantID, image string }
	if r.deployStore != nil {
		staleRows, staleErr := r.db.QueryContext(ctx,
			`SELECT id, tenant_id, image FROM services WHERE status = 'deploying' AND updated_at < ?`,
			tenMinAgo)
		if staleErr == nil {
			for staleRows.Next() {
				var s struct{ id, tenantID, image string }
				if scanErr := staleRows.Scan(&s.id, &s.tenantID, &s.image); scanErr == nil {
					staleServices = append(staleServices, s)
				}
			}
			staleRows.Close()
		}
	}

	result, err := r.db.ExecContext(ctx,
		`UPDATE services SET status = 'failed', last_error = 'deploy timed out (reconciler)',
		 updated_at = ? WHERE status = 'deploying' AND updated_at < ?`,
		time.Now().Unix(), tenMinAgo)
	if err != nil {
		log.Printf("reconciler: failed to mark stale deploys: %v", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("reconciler: marked %d stale deploys as failed", n)
		// Record deployment events for each timed-out service.
		for _, s := range staleServices {
			staleNow := time.Now().Unix()
			r.recordDeployment(ctx, &deployments.Deployment{
				ServiceID:    s.id,
				TenantID:     s.tenantID,
				Image:        s.image,
				Status:       deployments.StatusFailed,
				Trigger:      deployments.TriggerReconciler,
				ErrorMessage: "deploy timed out (reconciler)",
				StartedAt:    staleNow,
				CompletedAt:  &staleNow,
			})
		}
		fixed += int(n)
	}

	// 3. Check databases marked "ready" — if container gone, mark "unavailable"
	dbRows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id FROM databases WHERE status = 'ready' AND container_id != ''`)
	if err != nil {
		log.Printf("reconciler: failed to query databases: %v", err)
	} else {
		defer dbRows.Close()
		type dbRecord struct{ id, containerID string }
		var readyDBs []dbRecord
		for dbRows.Next() {
			var d dbRecord
			if err := dbRows.Scan(&d.id, &d.containerID); err != nil {
				continue
			}
			readyDBs = append(readyDBs, d)
		}
		dbRows.Close()

		for _, d := range readyDBs {
			checked++
			if !containerSet[d.containerID] {
				// Only mark unavailable on definitive not-found
				_, inspectErr := r.docker.InspectContainer(ctx, d.containerID)
				if inspectErr != nil && isNotFoundError(inspectErr) {
					_, err := r.db.ExecContext(ctx,
						`UPDATE databases SET status = 'unavailable', updated_at = ? WHERE id = ?`,
						time.Now().Unix(), d.id)
					if err != nil {
						log.Printf("reconciler: failed to mark database %s as unavailable: %v", d.id, err)
					} else {
						log.Printf("reconciler: database %s marked unavailable (container not found)", d.id)
						fixed++
					}
				} else if inspectErr != nil {
					log.Printf("reconciler: transient Docker error for database %s, skipping: %v", d.id, inspectErr)
				}
			}
		}
	}

	// 4. Check environments marked "running" — if container gone, mark "stopped"
	envRows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id, tenant_id FROM environments WHERE status = 'running' AND container_id != ''`)
	if err != nil {
		log.Printf("reconciler: failed to query environments: %v", err)
	} else {
		type envRecord struct{ id, containerID, tenantID string }
		var runningEnvs []envRecord
		for envRows.Next() {
			var e envRecord
			if err := envRows.Scan(&e.id, &e.containerID, &e.tenantID); err != nil {
				continue
			}
			runningEnvs = append(runningEnvs, e)
		}
		envRows.Close()

		for _, e := range runningEnvs {
			checked++
			if !containerSet[e.containerID] {
				_, inspectErr := r.docker.InspectContainer(ctx, e.containerID)
				if inspectErr != nil && isNotFoundError(inspectErr) {
					_, err := r.db.ExecContext(ctx,
						`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'running' AND container_id = ?`,
						time.Now().Unix(), e.id, e.tenantID, e.containerID)
					if err != nil {
						log.Printf("reconciler: failed to mark environment %s as stopped: %v", e.id, err)
					} else {
						log.Printf("reconciler: environment %s marked stopped (container not found)", e.id)
						fixed++
					}
				} else if inspectErr != nil {
					log.Printf("reconciler: transient Docker error for environment %s, skipping: %v", e.id, inspectErr)
				}
			} else {
				info, inspectErr := r.docker.InspectContainer(ctx, e.containerID)
				if inspectErr != nil {
					continue
				}
				if info.Status == "exited" || info.Status == "dead" {
					_, err := r.db.ExecContext(ctx,
						`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'running' AND container_id = ?`,
						time.Now().Unix(), e.id, e.tenantID, e.containerID)
					if err != nil {
						log.Printf("reconciler: failed to mark environment %s as stopped: %v", e.id, err)
					} else {
						log.Printf("reconciler: environment %s marked stopped (container %s)", e.id, info.Status)
						fixed++
					}
				}
			}
		}
	}

	// 4a. Mark stuck 'creating' environments (> 5 min) as 'failed'
	fiveMinAgo := time.Now().Add(-5 * time.Minute).Unix()
	envCreateResult, err := r.db.ExecContext(ctx,
		`UPDATE environments SET status = 'failed',
		 updated_at = ? WHERE status = 'creating' AND updated_at < ?`,
		time.Now().Unix(), fiveMinAgo)
	if err != nil {
		log.Printf("reconciler: failed to mark stale environment creates: %v", err)
	} else if n, _ := envCreateResult.RowsAffected(); n > 0 {
		log.Printf("reconciler: marked %d stale environment creates as failed", n)
		fixed += int(n)
	}

	// 4b. Expire environment leases. Stop the container first and only then mark the
	// row stopped, so a failed stop leaves the row 'running' and is retried next tick.
	leaseNow := time.Now().Unix()

	// Retry: rows already stopped whose container is still running (an earlier stop failed).
	retryRows, retryErr := r.db.QueryContext(ctx,
		`SELECT container_id FROM environments WHERE status = 'stopped' AND container_id != '' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		leaseNow)
	if retryErr == nil {
		var stoppedCIDs []string
		for retryRows.Next() {
			var cid string
			if retryRows.Scan(&cid) == nil && containerSet[cid] {
				stoppedCIDs = append(stoppedCIDs, cid)
			}
		}
		retryRows.Close()
		for _, cid := range stoppedCIDs {
			info, ierr := r.docker.InspectContainer(ctx, cid)
			if ierr != nil || (info.Status != "running" && info.Status != "restarting") {
				continue
			}
			if stopErr := r.docker.StopContainer(ctx, cid); stopErr != nil {
				log.Printf("reconciler: failed to stop expired environment container %s: %v", shortID(cid), stopErr)
			}
		}
	}

	leaseRows, leaseErr := r.db.QueryContext(ctx,
		`SELECT id, tenant_id, container_id FROM environments WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		leaseNow)
	if leaseErr != nil {
		log.Printf("reconciler: failed to query expired environment leases: %v", leaseErr)
	} else {
		type leaseRecord struct{ id, tenantID, containerID string }
		var expired []leaseRecord
		for leaseRows.Next() {
			var l leaseRecord
			var cid sql.NullString
			if leaseRows.Scan(&l.id, &l.tenantID, &cid) == nil {
				l.containerID = cid.String
				expired = append(expired, l)
			}
		}
		leaseRows.Close()
		for _, l := range expired {
			if l.containerID != "" {
				if stopErr := r.docker.StopContainer(ctx, l.containerID); stopErr != nil && !isNotFoundError(stopErr) {
					log.Printf("reconciler: failed to stop expired environment container %s, will retry: %v", shortID(l.containerID), stopErr)
					continue
				}
			}
			res, err := r.db.ExecContext(ctx,
				`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'running' AND container_id = ?`,
				time.Now().Unix(), l.id, l.tenantID, l.containerID)
			if err != nil {
				log.Printf("reconciler: failed to expire environment lease %s: %v", l.id, err)
			} else if n, _ := res.RowsAffected(); n > 0 {
				log.Printf("reconciler: expired environment lease %s", l.id)
				fixed++
			}
		}
	}

	// 5. Detect split-brain containers: ah.service-labeled containers not tracked in DB.
	// We intentionally do NOT auto-repair DB state from container labels, as labels can
	// be spoofed by any actor with Docker access. Instead, we log warnings for operator
	// awareness and let GC handle cleanup of orphaned containers after minResourceAge.
	allSvcContainers, listErr := r.docker.ListContainersByLabel(ctx, "ah.service", "")
	if listErr == nil {
		for _, cid := range allSvcContainers {
			labels := r.docker.GetContainerLabels(ctx, cid)
			if labels == nil {
				continue
			}
			svcID := labels["ah.service"]
			tenantID := labels["ah.tenant"]
			if svcID == "" || tenantID == "" {
				continue
			}
			var dbContainerID sql.NullString
			err := r.db.QueryRowContext(ctx,
				`SELECT container_id FROM services WHERE id = ? AND tenant_id = ?`,
				svcID, tenantID).Scan(&dbContainerID)
			if err != nil {
				continue // service not in DB, GC will handle
			}
			currentDBCID := ""
			if dbContainerID.Valid {
				currentDBCID = dbContainerID.String
			}
			if currentDBCID != cid {
				log.Printf("reconciler: WARNING: split-brain detected — container %s claims service %s but DB has container_id=%q; GC will clean up after age gate",
					shortID(cid), svcID, currentDBCID)
			}
		}
	}

	if checked > 0 || fixed > 0 {
		log.Printf("reconciler: checked=%d fixed=%d", checked, fixed)
	}

	return nil
}
