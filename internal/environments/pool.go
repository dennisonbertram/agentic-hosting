package environments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// PoolConfig configures the warm container pool.
type PoolConfig struct {
	Enabled      bool          // whether the pool is active
	PoolSize     int           // target containers per template (default: 2)
	MaxTotal     int           // max warm containers across all templates (default: 6)
	RefillPeriod time.Duration // how often to check pool (default: 60s)
}

// ErrPoolEmpty is returned when no warm container is available for the requested template.
var ErrPoolEmpty = errors.New("warm pool: no container available")

// PoolManager manages a pool of pre-warmed containers for fast environment creation.
type PoolManager struct {
	db     *sql.DB
	docker docker.Client
	config PoolConfig
	mu     sync.Mutex
}

// NewPoolManager creates a new pool manager.
func NewPoolManager(db *sql.DB, docker docker.Client, cfg PoolConfig) *PoolManager {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 2
	}
	if cfg.MaxTotal <= 0 {
		cfg.MaxTotal = 6
	}
	if cfg.RefillPeriod <= 0 {
		cfg.RefillPeriod = 60 * time.Second
	}
	return &PoolManager{
		db:     db,
		docker: docker,
		config: cfg,
	}
}

// Run starts the background refill loop. Blocks until ctx is cancelled.
func (p *PoolManager) Run(ctx context.Context) {
	if !p.config.Enabled {
		log.Printf("warm-pool: disabled")
		return
	}
	log.Printf("warm-pool: starting (pool_size=%d, max_total=%d, refill=%s)",
		p.config.PoolSize, p.config.MaxTotal, p.config.RefillPeriod)

	// Initial refill after a short delay to let migrations complete.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}

	p.safeRefill(ctx)

	ticker := time.NewTicker(p.config.RefillPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("warm-pool: stopped")
			return
		case <-ticker.C:
			p.safeRefill(ctx)
		}
	}
}

func (p *PoolManager) safeRefill(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("warm-pool: PANIC recovered during refill: %v", r)
		}
	}()
	if err := p.refill(ctx); err != nil {
		log.Printf("warm-pool: refill error: %v", err)
	}
}

// pruneStaleEntries removes warm_pool rows whose containers no longer exist in Docker.
// This prevents ghost rows from making the pool appear full after server restarts or
// docker prune events. Must be called with p.mu held.
func (p *PoolManager) pruneStaleEntries(ctx context.Context) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM warm_pool WHERE status = 'ready'`)
	if err != nil {
		log.Printf("warm-pool: pruneStaleEntries query failed: %v", err)
		return
	}
	defer rows.Close()

	type entry struct {
		id          string
		containerID string
		volumeName  string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.containerID, &e.volumeName); err != nil {
			log.Printf("warm-pool: pruneStaleEntries scan failed: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("warm-pool: pruneStaleEntries iterate failed: %v", err)
		return
	}

	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		_, inspectErr := p.docker.InspectContainer(ctx, e.containerID)
		if inspectErr == nil {
			// Container exists — keep the row.
			continue
		}
		// Container gone — delete the row and try to clean up the volume.
		log.Printf("warm-pool: pruning stale entry %s (container %s gone): %v",
			e.id, e.containerID[:min(12, len(e.containerID))], inspectErr)
		if _, delErr := p.db.ExecContext(ctx,
			`DELETE FROM warm_pool WHERE id = ?`, e.id); delErr != nil {
			log.Printf("warm-pool: failed to delete stale entry %s: %v", e.id, delErr)
		}
		_ = p.docker.RemoveVolume(ctx, e.volumeName)
	}
}

func (p *PoolManager) refill(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Remove rows that point to non-existent Docker containers before counting.
	p.pruneStaleEntries(ctx)

	// Get total ready count.
	var totalReady int
	if err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&totalReady); err != nil {
		return fmt.Errorf("count total ready: %w", err)
	}

	// Get all templates.
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, base_image, memory_mb, cpu_millicores, disk_mb FROM environment_templates`)
	if err != nil {
		return fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()

	type tmplInfo struct {
		ID        string
		Image     string
		MemoryMB  int64
		CPUMillis int64
		DiskMB    int64
	}
	var templates []tmplInfo
	for rows.Next() {
		var t tmplInfo
		if err := rows.Scan(&t.ID, &t.Image, &t.MemoryMB, &t.CPUMillis, &t.DiskMB); err != nil {
			return fmt.Errorf("scan template: %w", err)
		}
		templates = append(templates, t)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate templates: %w", err)
	}

	for _, tmpl := range templates {
		if totalReady >= p.config.MaxTotal {
			break
		}

		var readyCount int
		if err := p.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM warm_pool WHERE template_id = ? AND status = 'ready'`,
			tmpl.ID).Scan(&readyCount); err != nil {
			log.Printf("warm-pool: count ready for %s failed: %v", tmpl.ID, err)
			continue
		}

		for readyCount < p.config.PoolSize && totalReady < p.config.MaxTotal {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			poolEntryID := fmt.Sprintf("wp_%d", time.Now().UnixNano())
			volumeName := fmt.Sprintf("ah-pool-%s", poolEntryID)

			// Create volume.
			if err := p.docker.CreateVolume(ctx, volumeName); err != nil {
				log.Printf("warm-pool: create volume %s failed: %v", volumeName, err)
				break
			}

			// Ensure pool network exists (shared, non-internal for pool containers).
			poolNet := "ah-pool"
			if _, err := p.docker.EnsureNetwork(ctx, poolNet); err != nil {
				log.Printf("warm-pool: ensure pool network failed: %v", err)
				_ = p.docker.RemoveVolume(ctx, volumeName)
				break
			}

			containerID, err := p.docker.RunEnvironment(ctx, docker.RunEnvironmentConfig{
				TenantID:    "pool",
				EnvID:       poolEntryID,
				Image:       tmpl.Image,
				MemoryMB:    tmpl.MemoryMB,
				CPUMillis:   tmpl.CPUMillis,
				VolumeName:  volumeName,
				NetworkName: poolNet,
				Labels: map[string]string{
					"ah.managed":  "true",
					"ah.type":     "warm-pool",
					"ah.template": tmpl.ID,
				},
			})
			if err != nil {
				log.Printf("warm-pool: run container for %s failed: %v", tmpl.ID, err)
				_ = p.docker.RemoveVolume(ctx, volumeName)
				break
			}

			now := time.Now().Unix()
			if _, err := p.db.ExecContext(ctx,
				`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
				 VALUES (?, ?, ?, ?, 'ready', ?)`,
				poolEntryID, tmpl.ID, containerID, volumeName, now); err != nil {
				log.Printf("warm-pool: insert row failed: %v", err)
				// Clean up the container and volume.
				_ = p.docker.StopContainer(ctx, containerID)
				_ = p.docker.RemoveContainer(ctx, containerID)
				_ = p.docker.RemoveVolume(ctx, volumeName)
				break
			}

			readyCount++
			totalReady++
			log.Printf("warm-pool: created container for template %s (total ready: %d)", tmpl.ID, totalReady)
		}
	}

	stats, _ := p.statsLocked(ctx)
	log.Printf("warm-pool: stats %v (total ready: %d)", stats, totalReady)
	return nil
}

// Acquire claims a warm container for the given template. Returns container ID
// and volume name. The caller is responsible for renaming/reconnecting the container.
// Returns ErrPoolEmpty if no container is available.
func (p *PoolManager) Acquire(ctx context.Context, templateID string) (containerID, volumeName string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Claim one ready container for this template.
	// SQLite doesn't support LIMIT in UPDATE ... RETURNING, so we SELECT first.
	var id string
	err = tx.QueryRowContext(ctx,
		`SELECT id, container_id, volume_name FROM warm_pool
		 WHERE template_id = ? AND status = 'ready'
		 ORDER BY created_at ASC LIMIT 1`, templateID).Scan(&id, &containerID, &volumeName)
	if err == sql.ErrNoRows {
		return "", "", ErrPoolEmpty
	}
	if err != nil {
		return "", "", fmt.Errorf("select ready container: %w", err)
	}

	// Mark as claimed.
	if _, err := tx.ExecContext(ctx,
		`UPDATE warm_pool SET status = 'claimed' WHERE id = ?`, id); err != nil {
		return "", "", fmt.Errorf("claim container: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit: %w", err)
	}

	// Delete the claimed row.
	if _, err := p.db.ExecContext(ctx,
		`DELETE FROM warm_pool WHERE id = ?`, id); err != nil {
		log.Printf("warm-pool: delete claimed row %s failed (non-fatal): %v", id, err)
	}

	return containerID, volumeName, nil
}

// Stats returns the number of ready containers per template.
func (p *PoolManager) Stats(ctx context.Context) (map[string]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statsLocked(ctx)
}

func (p *PoolManager) statsLocked(ctx context.Context) (map[string]int, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT template_id, COUNT(*) FROM warm_pool WHERE status = 'ready' GROUP BY template_id`)
	if err != nil {
		return nil, fmt.Errorf("query stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[string]int)
	for rows.Next() {
		var tmplID string
		var count int
		if err := rows.Scan(&tmplID, &count); err != nil {
			return nil, fmt.Errorf("scan stats: %w", err)
		}
		stats[tmplID] = count
	}
	return stats, rows.Err()
}

// Drain stops and removes all warm pool containers and cleans up the DB.
func (p *PoolManager) Drain(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rows, err := p.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM warm_pool`)
	if err != nil {
		return fmt.Errorf("list pool entries: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id          string
		containerID string
		volumeName  string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.containerID, &e.volumeName); err != nil {
			log.Printf("warm-pool: drain scan error: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pool entries: %w", err)
	}

	for _, e := range entries {
		log.Printf("warm-pool: draining container %s", e.containerID[:min(12, len(e.containerID))])
		_ = p.docker.StopContainer(ctx, e.containerID)
		_ = p.docker.RemoveContainer(ctx, e.containerID)
		_ = p.docker.RemoveVolume(ctx, e.volumeName)
		if _, err := p.db.ExecContext(ctx, `DELETE FROM warm_pool WHERE id = ?`, e.id); err != nil {
			log.Printf("warm-pool: drain delete row %s failed: %v", e.id, err)
		}
	}

	log.Printf("warm-pool: drained %d containers", len(entries))
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
