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

// staleDeployAge and staleEnvCreateAge bound how long a row may sit in a
// transitional state before the reconciler marks it failed.
const (
	staleDeployAge    = 10 * time.Minute
	staleEnvCreateAge = 5 * time.Minute
)

// tally counts rows inspected and rows changed by one reconciliation pass.
type tally struct{ checked, fixed int }

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

	var t tally
	// 1. Crash state machine for running/crashed services (see package doc).
	if err := r.reconcileServices(ctx, &t); err != nil {
		return err
	}
	// 1b. Auto-recover open circuits whose retry_at has elapsed.
	r.recoverCircuits(ctx, &t)
	// 2. Services stuck in "deploying".
	r.failStaleDeploys(ctx, &t)
	// 3. Databases marked "ready" whose container is gone.
	r.reconcileDatabases(ctx, containerSet, &t)
	// 4. Environments marked "running" whose container is gone or exited.
	r.reconcileEnvironments(ctx, &t)
	// 4a. Environments stuck in "creating".
	r.failStaleEnvCreates(ctx, &t)
	// 4b. Expired environment leases.
	r.expireLeases(ctx, containerSet, &t)
	// 5. Split-brain detection (log only).
	r.detectSplitBrain(ctx)

	if t.checked > 0 || t.fixed > 0 {
		log.Printf("reconciler: checked=%d fixed=%d", t.checked, t.fixed)
	}
	return nil
}

// recordServiceEvent records a reconciler-originated deployment event for a
// service, looking up its current image.
func (r *Reconciler) recordServiceEvent(ctx context.Context, id, tenantID, image, status, trigger, errMsg string) {
	if r.deployStore == nil {
		return
	}
	if image == "" {
		_ = r.db.QueryRowContext(ctx, `SELECT image FROM services WHERE id = ? AND tenant_id = ?`, id, tenantID).Scan(&image)
	}
	now := time.Now().Unix()
	r.recordDeployment(ctx, &deployments.Deployment{
		ServiceID:    id,
		TenantID:     tenantID,
		Image:        image,
		Status:       status,
		Trigger:      trigger,
		ErrorMessage: errMsg,
		StartedAt:    now,
		CompletedAt:  &now,
	})
}

type svcRecord struct {
	id, containerID, tenantID, status string
	crashCount, circuitOpenCount      int
	windowStart, lastRestartCount     sql.NullInt64
}

func (r *Reconciler) reconcileServices(ctx context.Context, t *tally) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id, tenant_id, status, crash_count, crash_window_start, circuit_open_count, last_restart_count
		 FROM services WHERE status IN ('running', 'crashed') AND circuit_open = 0 AND container_id != ''`)
	if err != nil {
		return err
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
		t.checked++
		if r.reconcileService(ctx, s) {
			t.fixed++
		}
	}
	return nil
}

// reconcileService applies one tick of the crash state machine to a service and
// reports whether it changed state.
func (r *Reconciler) reconcileService(ctx context.Context, s svcRecord) bool {
	info, inspectErr := r.docker.InspectContainer(ctx, s.containerID)
	gone := false
	if inspectErr != nil {
		if !isNotFoundError(inspectErr) {
			// Transient Docker error — do NOT change state.
			log.Printf("reconciler: transient Docker error for service %s (container %s), skipping: %v",
				s.id, shortID(s.containerID), inspectErr)
			return false
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

	// A crashed row already at the threshold means an earlier breaker trip could not
	// stop the container; retry it.
	if crashes == 0 && s.status == "crashed" && s.crashCount >= crashThreshold &&
		s.windowStart.Valid && now-s.windowStart.Int64 < crashWindowSecs {
		return r.tripBreaker(ctx, s.id, s.tenantID, s.containerID, s.circuitOpenCount)
	}

	if crashes == 0 {
		newStatus := s.status
		if s.status == "crashed" && info != nil && info.Status == "running" &&
			(info.HealthStatus == "" || info.HealthStatus == "healthy" || info.HealthStatus == "none") {
			newStatus = "running"
		}
		if newStatus == s.status && newBaseline == s.lastRestartCount {
			return false
		}
		res, err := r.db.ExecContext(ctx, `
			UPDATE services SET status = ?, last_restart_count = ?,
				updated_at = CASE WHEN status = ? THEN updated_at ELSE ? END
			WHERE id = ? AND tenant_id = ? AND status = ? AND container_id = ?`,
			newStatus, newBaseline, newStatus, now, s.id, s.tenantID, s.status, s.containerID)
		if err != nil {
			log.Printf("reconciler: failed to update service %s: %v", s.id, err)
			return false
		}
		if n, _ := res.RowsAffected(); n > 0 && newStatus != s.status {
			log.Printf("reconciler: service %s recovered, container running again", s.id)
			return true
		}
		return false
	}

	// Crash window and circuit breaker, computed here so one CAS UPDATE sets everything.
	windowStart, crashCount := s.windowStart.Int64, s.crashCount+crashes
	if !s.windowStart.Valid || now-windowStart >= crashWindowSecs {
		windowStart, crashCount = now, crashes
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE services SET
			status = 'crashed',
			crash_window_start = ?,
			crash_count = ?,
			last_crashed_at = ?,
			last_error = ?,
			last_restart_count = ?,
			updated_at = ?
		WHERE id = ? AND tenant_id = ? AND status = ? AND container_id = ?`,
		windowStart, crashCount, now, crashReason, newBaseline,
		now, s.id, s.tenantID, s.status, s.containerID)
	if err != nil {
		log.Printf("reconciler: failed to mark service %s as crashed: %v", s.id, err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("reconciler: service %s changed during reconcile, skipping crash handling", s.id)
		return false
	}

	if crashCount >= crashThreshold {
		r.tripBreaker(ctx, s.id, s.tenantID, s.containerID, s.circuitOpenCount)
	} else if needRestart {
		r.restartUnhealthy(ctx, s)
	}
	log.Printf("reconciler: service %s marked crashed (%s)", s.id, crashReason)
	r.recordServiceEvent(ctx, s.id, s.tenantID, "", deployments.StatusCrashed, deployments.TriggerReconciler, crashReason)
	return true
}

// restartUnhealthy restarts an unhealthy container. A manual stop would stay
// stopped under unless-stopped, so the reconciler restarts instead of stopping.
func (r *Reconciler) restartUnhealthy(ctx context.Context, s svcRecord) {
	log.Printf("reconciler: service %s container %s is unhealthy — restarting", s.id, shortID(s.containerID))
	if err := r.docker.RestartContainer(ctx, s.containerID); err != nil {
		log.Printf("reconciler: failed to restart unhealthy container for service %s: %v", s.id, err)
		return
	}
	// Manager.Stop does not share a lock with the reconciler. If it marked the
	// row stopped (or replaced the container) meanwhile, undo the restart.
	var curStatus, curCID string
	qerr := r.db.QueryRowContext(ctx, `SELECT status, container_id FROM services WHERE id = ? AND tenant_id = ?`,
		s.id, s.tenantID).Scan(&curStatus, &curCID)
	if qerr == nil && curCID == s.containerID && curStatus != "running" && curStatus != "crashed" {
		log.Printf("reconciler: service %s changed to %s during unhealthy restart, stopping container again", s.id, curStatus)
		if serr := r.docker.StopContainer(ctx, s.containerID); serr != nil && !isNotFoundError(serr) {
			log.Printf("reconciler: failed to re-stop container for service %s: %v", s.id, serr)
		}
	}
}

// recoverCircuits resets open circuits whose retry_at has elapsed. The container
// was removed when the circuit opened and nothing restarts it here; the service
// stays stopped until it is started or redeployed.
func (r *Reconciler) recoverCircuits(ctx context.Context, t *tally) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, tenant_id FROM services WHERE circuit_open = 1 AND circuit_retry_at IS NOT NULL AND circuit_retry_at <= ?`,
		time.Now().Unix())
	if err != nil {
		log.Printf("reconciler: failed to query circuit recovery candidates: %v", err)
		return
	}
	type recoverRecord struct{ id, tenantID string }
	var toRecover []recoverRecord
	for rows.Next() {
		var rec recoverRecord
		if err := rows.Scan(&rec.id, &rec.tenantID); err != nil {
			continue
		}
		toRecover = append(toRecover, rec)
	}
	rows.Close()

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
		log.Printf("reconciler: service %s circuit reset, service left stopped (start or redeploy to resume)", rec.id)
		r.recordServiceEvent(ctx, rec.id, rec.tenantID, "", deployments.StatusStopped, deployments.TriggerAutoRecovery, "")
		t.fixed++
	}
}

// failStaleDeploys marks services stuck in "deploying" as failed. Each row is
// claimed with its own compare-and-swap so a deploy that finishes between the
// scan and the update is neither failed nor recorded as a timeout.
func (r *Reconciler) failStaleDeploys(ctx context.Context, t *tally) {
	const reason = "deploy timed out (reconciler)"
	cutoff := time.Now().Add(-staleDeployAge).Unix()
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, tenant_id, image FROM services WHERE status = 'deploying' AND updated_at < ?`, cutoff)
	if err != nil {
		log.Printf("reconciler: failed to query stale deploys: %v", err)
		return
	}
	type staleRecord struct {
		id, tenantID string
		image        sql.NullString
	}
	var stale []staleRecord
	for rows.Next() {
		var s staleRecord
		if err := rows.Scan(&s.id, &s.tenantID, &s.image); err == nil {
			stale = append(stale, s)
		}
	}
	rows.Close()

	var n int
	for _, s := range stale {
		res, err := r.db.ExecContext(ctx,
			`UPDATE services SET status = 'failed', last_error = ?, updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND status = 'deploying' AND updated_at < ?`,
			reason, time.Now().Unix(), s.id, s.tenantID, cutoff)
		if err != nil {
			log.Printf("reconciler: failed to mark stale deploy %s: %v", s.id, err)
			continue
		}
		if c, _ := res.RowsAffected(); c == 0 {
			continue
		}
		n++
		r.recordServiceEvent(ctx, s.id, s.tenantID, s.image.String, deployments.StatusFailed, deployments.TriggerReconciler, reason)
	}
	if n > 0 {
		log.Printf("reconciler: marked %d stale deploys as failed", n)
		t.fixed += n
	}
}

// reconcileDatabases marks a ready database unavailable when its container is
// definitively gone. The update is a compare-and-swap so a concurrent
// re-provision or delete is not overwritten.
func (r *Reconciler) reconcileDatabases(ctx context.Context, containerSet map[string]bool, t *tally) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, tenant_id, container_id FROM databases WHERE status = 'ready' AND container_id != ''`)
	if err != nil {
		log.Printf("reconciler: failed to query databases: %v", err)
		return
	}
	type dbRecord struct{ id, tenantID, containerID string }
	var readyDBs []dbRecord
	for rows.Next() {
		var d dbRecord
		if err := rows.Scan(&d.id, &d.tenantID, &d.containerID); err != nil {
			continue
		}
		readyDBs = append(readyDBs, d)
	}
	rows.Close()

	for _, d := range readyDBs {
		t.checked++
		if containerSet[d.containerID] {
			continue
		}
		// Only mark unavailable on definitive not-found.
		_, inspectErr := r.docker.InspectContainer(ctx, d.containerID)
		if inspectErr == nil {
			continue
		}
		if !isNotFoundError(inspectErr) {
			log.Printf("reconciler: transient Docker error for database %s, skipping: %v", d.id, inspectErr)
			continue
		}
		res, err := r.db.ExecContext(ctx,
			`UPDATE databases SET status = 'unavailable', updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND status = 'ready' AND container_id = ?`,
			time.Now().Unix(), d.id, d.tenantID, d.containerID)
		if err != nil {
			log.Printf("reconciler: failed to mark database %s as unavailable: %v", d.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("reconciler: database %s marked unavailable (container not found)", d.id)
			t.fixed++
		}
	}
}

// reconcileEnvironments marks a running environment stopped when its container
// is definitively gone, exited or dead. Transient Docker errors change nothing.
func (r *Reconciler) reconcileEnvironments(ctx context.Context, t *tally) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id, tenant_id FROM environments WHERE status = 'running' AND container_id != ''`)
	if err != nil {
		log.Printf("reconciler: failed to query environments: %v", err)
		return
	}
	type envRecord struct{ id, containerID, tenantID string }
	var runningEnvs []envRecord
	for rows.Next() {
		var e envRecord
		if err := rows.Scan(&e.id, &e.containerID, &e.tenantID); err != nil {
			continue
		}
		runningEnvs = append(runningEnvs, e)
	}
	rows.Close()

	for _, e := range runningEnvs {
		t.checked++
		var why string
		info, inspectErr := r.docker.InspectContainer(ctx, e.containerID)
		switch {
		case inspectErr != nil && isNotFoundError(inspectErr):
			why = "container not found"
		case inspectErr != nil:
			log.Printf("reconciler: transient Docker error for environment %s, skipping: %v", e.id, inspectErr)
			continue
		case info.Status == "exited" || info.Status == "dead":
			why = "container " + info.Status
		default:
			continue
		}
		res, err := r.db.ExecContext(ctx,
			`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'running' AND container_id = ?`,
			time.Now().Unix(), e.id, e.tenantID, e.containerID)
		if err != nil {
			log.Printf("reconciler: failed to mark environment %s as stopped: %v", e.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("reconciler: environment %s marked stopped (%s)", e.id, why)
			t.fixed++
		}
	}
}

// failStaleEnvCreates marks environments stuck in "creating" as failed.
func (r *Reconciler) failStaleEnvCreates(ctx context.Context, t *tally) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE environments SET status = 'failed',
		 updated_at = ? WHERE status = 'creating' AND updated_at < ?`,
		time.Now().Unix(), time.Now().Add(-staleEnvCreateAge).Unix())
	if err != nil {
		log.Printf("reconciler: failed to mark stale environment creates: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("reconciler: marked %d stale environment creates as failed", n)
		t.fixed += int(n)
	}
}

// expireLeases stops environments whose lease has expired. The row is claimed
// first (re-checking the lease) so a concurrent renewal wins cleanly and is
// never followed by a stop; a failed stop is retried on later ticks.
func (r *Reconciler) expireLeases(ctx context.Context, containerSet map[string]bool, t *tally) {
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
			// Re-check the row after the inspect: environments.Start extends the
			// lease before starting the container, so a row that no longer has an
			// expired lease (or is no longer 'stopped') is being started by a user.
			var still int
			if err := r.db.QueryRowContext(ctx,
				`SELECT 1 FROM environments WHERE container_id = ? AND status = 'stopped' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
				cid, time.Now().Unix()).Scan(&still); err != nil {
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
		return
	}
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
		res, err := r.db.ExecContext(ctx,
			`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND status = 'running' AND COALESCE(container_id, '') = ? AND lease_expires_at < ?`,
			time.Now().Unix(), l.id, l.tenantID, l.containerID, leaseNow)
		if err != nil {
			log.Printf("reconciler: failed to expire environment lease %s: %v", l.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if l.containerID != "" {
			if stopErr := r.docker.StopContainer(ctx, l.containerID); stopErr != nil && !isNotFoundError(stopErr) {
				log.Printf("reconciler: failed to stop expired environment container %s, will retry: %v", shortID(l.containerID), stopErr)
			}
		}
		log.Printf("reconciler: expired environment lease %s", l.id)
		t.fixed++
	}
}

// detectSplitBrain logs ah.service-labeled containers the DB does not track
// for that service. It intentionally does NOT repair DB state from container
// labels, as labels can be spoofed by any actor with Docker access; GC removes
// orphaned containers after minResourceAge.
func (r *Reconciler) detectSplitBrain(ctx context.Context) {
	allSvcContainers, err := r.docker.ListContainersByLabel(ctx, "ah.service", "")
	if err != nil {
		return
	}
	for _, cid := range allSvcContainers {
		labels := r.docker.GetContainerLabels(ctx, cid)
		svcID, tenantID := labels["ah.service"], labels["ah.tenant"]
		if svcID == "" || tenantID == "" {
			continue
		}
		var dbContainerID sql.NullString
		if err := r.db.QueryRowContext(ctx,
			`SELECT container_id FROM services WHERE id = ? AND tenant_id = ?`,
			svcID, tenantID).Scan(&dbContainerID); err != nil {
			continue // service not in DB, GC will handle
		}
		if dbContainerID.String != cid {
			log.Printf("reconciler: WARNING: split-brain detected — container %s claims service %s but DB has container_id=%q; GC will clean up after age gate",
				shortID(cid), svcID, dbContainerID.String)
		}
	}
}

// tripBreaker stops and removes the container, and only then commits circuit_open=1 and
// clears container_id. If the container cannot be stopped or removed, the row stays
// crashed and watched so the next tick retries. Reports whether the circuit opened.
func (r *Reconciler) tripBreaker(ctx context.Context, id, tenantID, containerID string, openCount int) bool {
	log.Printf("reconciler: service %s circuit breaker OPEN — stopping container", id)
	// Stop first to defeat the restart policy, then remove.
	if err := r.docker.StopContainer(ctx, containerID); err != nil && !isNotFoundError(err) {
		log.Printf("reconciler: service %s breaker trip: stop %s failed, will retry: %v", id, shortID(containerID), err)
		return false
	}
	if err := r.docker.RemoveContainer(ctx, containerID); err != nil && !isNotFoundError(err) {
		log.Printf("reconciler: service %s breaker trip: remove %s failed, will retry: %v", id, shortID(containerID), err)
		return false
	}
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE services SET circuit_open = 1, container_id = '', circuit_retry_at = ?,
			circuit_open_count = circuit_open_count + 1, updated_at = ?
		WHERE id = ? AND tenant_id = ? AND status = 'crashed' AND container_id = ?`,
		now.Add(circuitRetryBackoff(openCount+1)).Unix(), now.Unix(), id, tenantID, containerID)
	if err != nil {
		log.Printf("reconciler: service %s breaker trip: commit failed: %v", id, err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}
