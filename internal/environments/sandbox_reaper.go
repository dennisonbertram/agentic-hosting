package environments

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

// Reaper timing. Stopped and failed sandboxes are kept for a grace period so
// their logs and row stay available for debugging; an expired TTL deletes
// immediately. A 'deleting' row older than deletingRetryAge is a teardown
// that failed or was interrupted by a restart, and is retried.
const (
	sandboxGracePeriod    = 10 * time.Minute
	sandboxDeletingRetry  = 2 * time.Minute
	sandboxReapBatchLimit = 100
)

// reapPredicate selects sandbox rows the reaper owns. Its three arguments are
// now, now-grace and now-deletingRetry (unix seconds). The same predicate is
// re-checked in the claim UPDATE, so a concurrent ExtendSandbox (which needs
// lease_expires_at >= now) and the claim cannot both win.
const reapPredicate = `kind = 'sandbox' AND (
	(status IN ('running', 'stopped') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?)
	OR (status IN ('stopped', 'failed') AND updated_at < ?)
	OR (status = 'deleting' AND updated_at < ?))`

// RunSandboxReaper deletes expired and dead sandboxes every interval until
// ctx is cancelled. A panic in one pass is logged and the loop continues.
func (m *Manager) RunSandboxReaper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		m.safeReap(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) safeReap(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("environments: sandbox reaper panic: %v\n%s", r, debug.Stack())
		}
	}()
	if _, err := m.ReapSandboxes(ctx); err != nil && ctx.Err() == nil {
		log.Printf("environments: sandbox reaper: %v", err)
	}
	if _, err := m.DeleteSandboxesForDiskPressure(ctx); err != nil && ctx.Err() == nil {
		log.Printf("environments: sandbox disk watchdog: %v", err)
	}
}

// DeleteSandboxesForDiskPressure is the reaper's disk watchdog. While the
// sandbox disk check (SetSandboxDiskCheck: the 90% block threshold on the
// data dir and the Docker root) fails, it deletes running sandboxes, largest
// disk_mb first (oldest first among equals), through the same strict
// teardown as DELETE, re-checking after each one. Sandboxes are disposable;
// a full disk would stop deploys, builds and SQLite writes for every tenant.
// It returns the number deleted.
func (m *Manager) DeleteSandboxesForDiskPressure(ctx context.Context) (int, error) {
	check := m.sandboxDiskCheck()
	if check() == nil {
		return 0, nil
	}
	deleted := 0
	for {
		pressure := check()
		if pressure == nil {
			return deleted, nil
		}
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		var id, tenantID string
		var diskMB int
		err := m.db.QueryRowContext(ctx,
			`SELECT id, tenant_id, disk_mb FROM environments WHERE kind = 'sandbox' AND status = 'running'
			 ORDER BY disk_mb DESC, created_at, id LIMIT 1`).Scan(&id, &tenantID, &diskMB)
		if err == sql.ErrNoRows {
			if deleted == 0 {
				log.Printf("environments: disk watchdog: %v; no running sandbox left to delete", pressure)
			}
			return deleted, nil
		}
		if err != nil {
			return deleted, fmt.Errorf("select sandbox for disk pressure: %w", err)
		}
		res, err := m.db.ExecContext(ctx,
			`UPDATE environments SET status = 'deleting', updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND kind = 'sandbox' AND status = 'running'`,
			time.Now().Unix(), id, tenantID)
		if err != nil {
			return deleted, fmt.Errorf("claim sandbox %s for disk pressure: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // deleted or claimed elsewhere since the select
		}
		// A failed teardown leaves the row 'deleting' (the reaper retries
		// it) and it is not selected again, so the loop still moves on.
		if err := m.teardownSandbox(ctx, tenantID, id); err != nil {
			log.Printf("environments: disk watchdog: delete sandbox %s (will retry): %v", id, err)
			continue
		}
		log.Printf("AUDIT: action=sandbox.disk_pressure_deleted tenant=%s sandbox=%s disk_mb=%d reason=%q", tenantID, id, diskMB, pressure.Error())
		deleted++
	}
}

// ReapSandboxes deletes sandboxes whose TTL has expired, stopped or failed
// sandboxes past the grace period, and stale 'deleting' rows. Each row is
// claimed ('deleting') with the selection predicate re-checked before its
// teardown. It returns the number of sandboxes removed.
func (m *Manager) ReapSandboxes(ctx context.Context) (int, error) {
	now := time.Now()
	args := []any{now.Unix(), now.Add(-sandboxGracePeriod).Unix(), now.Add(-sandboxDeletingRetry).Unix()}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, status FROM environments WHERE `+reapPredicate+` ORDER BY updated_at LIMIT ?`,
		append(args, sandboxReapBatchLimit)...)
	if err != nil {
		return 0, fmt.Errorf("select sandboxes to reap: %w", err)
	}
	type candidate struct{ id, tenantID, status string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.tenantID, &c.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan sandbox to reap: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	reaped := 0
	for _, c := range candidates {
		if ctx.Err() != nil {
			return reaped, ctx.Err()
		}
		claimArgs := append([]any{time.Now().Unix(), c.id, c.tenantID}, args...)
		res, err := m.db.ExecContext(ctx,
			`UPDATE environments SET status = 'deleting', updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND `+reapPredicate, claimArgs...)
		if err != nil {
			log.Printf("environments: claim sandbox %s for reaping: %v", c.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // extended, deleted or claimed elsewhere since the select
		}
		if err := m.teardownSandbox(ctx, c.tenantID, c.id); err != nil {
			log.Printf("environments: reap sandbox %s (will retry): %v", c.id, err)
			continue
		}
		action := "sandbox.expired"
		if c.status == "failed" || c.status == "deleting" {
			action = "sandbox.reaped"
		}
		log.Printf("AUDIT: action=%s tenant=%s sandbox=%s prior_status=%s", action, c.tenantID, c.id, c.status)
		reaped++
	}
	return reaped, nil
}
