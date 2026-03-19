package environments

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// IdleDetector stops dev environments that have been idle beyond their timeout.
type IdleDetector struct {
	db       *sql.DB
	docker   docker.Client
	interval time.Duration
}

// NewIdleDetector creates an idle detector with the given check interval.
func NewIdleDetector(db *sql.DB, dockerClient docker.Client, interval time.Duration) *IdleDetector {
	return &IdleDetector{
		db:       db,
		docker:   dockerClient,
		interval: interval,
	}
}

// Run starts the idle detection loop. Blocks until ctx is cancelled.
func (d *IdleDetector) Run(ctx context.Context) {
	log.Printf("idle-detector: starting (interval=%s)", d.interval)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("idle-detector: stopped")
			return
		case <-ticker.C:
			d.safeCheck(ctx)
		}
	}
}

func (d *IdleDetector) safeCheck(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("idle-detector: PANIC recovered: %v\n%s", rec, string(debug.Stack()))
		}
	}()
	tickCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()
	if err := d.checkOnce(tickCtx); err != nil {
		log.Printf("idle-detector: error: %v", err)
	}
}

// CheckOnce runs a single idle check pass. Exported for tests.
func (d *IdleDetector) CheckOnce(ctx context.Context) error {
	return d.checkOnce(ctx)
}

func (d *IdleDetector) checkOnce(ctx context.Context) error {
	now := time.Now().Unix()
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, container_id FROM environments
		 WHERE status = 'running' AND last_activity_at IS NOT NULL
		 AND (? - last_activity_at) > idle_timeout_sec`, now)
	if err != nil {
		return err
	}
	defer rows.Close()

	type idleEnv struct{ id, containerID string }
	var idle []idleEnv
	for rows.Next() {
		var e idleEnv
		if err := rows.Scan(&e.id, &e.containerID); err != nil {
			continue
		}
		idle = append(idle, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate idle environments: %w", err)
	}

	for _, e := range idle {
		log.Printf("idle-detector: stopping idle environment %s", e.id)
		if e.containerID != "" {
			if err := d.docker.StopContainer(ctx, e.containerID); err != nil {
				log.Printf("idle-detector: failed to stop container for %s: %v", e.id, err)
			}
		}
		_, err := d.db.ExecContext(ctx,
			`UPDATE environments SET status = 'stopped', updated_at = ? WHERE id = ?`,
			time.Now().Unix(), e.id)
		if err != nil {
			log.Printf("idle-detector: failed to update status for %s: %v", e.id, err)
		}
	}

	if len(idle) > 0 {
		log.Printf("idle-detector: stopped %d idle environments", len(idle))
	}
	return nil
}
