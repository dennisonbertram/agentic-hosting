// Package gc cleans up orphaned Docker resources.
package gc

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// SnapshotCleaner is the interface used by GC to clean expired snapshots.
// The snapshots.Manager satisfies this interface.
type SnapshotCleaner interface {
	CleanExpired(ctx context.Context, maxPerService int, maxAge time.Duration) (int, error)
}

// GC cleans up orphaned containers, volumes, images, and build dirs.
type GC struct {
	db       *sql.DB
	docker   docker.Client
	interval time.Duration
	buildDir string

	// Snapshot retention (optional — nil disables cleanup).
	snapCleaner       SnapshotCleaner
	snapMaxPerService int
	snapMaxAge        time.Duration
}

// minResourceAge is the minimum time a resource must exist before GC considers
// deleting it. This prevents races with provisioning/deploy operations.
const minResourceAge = 10 * time.Minute

// New creates a garbage collector with the given interval.
func New(db *sql.DB, dockerClient docker.Client, interval time.Duration, buildDir string) *GC {
	return &GC{
		db:       db,
		docker:   dockerClient,
		interval: interval,
		buildDir: buildDir,
	}
}

// SetSnapshotCleaner configures snapshot retention cleanup. When set, the GC
// loop will periodically remove snapshots exceeding the per-service count or
// age limits.
func (g *GC) SetSnapshotCleaner(cleaner SnapshotCleaner, maxPerService int, maxAge time.Duration) {
	g.snapCleaner = cleaner
	g.snapMaxPerService = maxPerService
	g.snapMaxAge = maxAge
}

// Run starts the GC loop. Blocks until ctx is cancelled.
// Recovers from panics to keep the loop alive.
func (g *GC) Run(ctx context.Context) {
	log.Printf("gc: starting (interval=%s)", g.interval)
	// Delay first run by 2 minutes to let startup settle
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	// Run once immediately after delay
	g.safeCollect(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("gc: stopped")
			return
		case <-ticker.C:
			g.safeCollect(ctx)
		}
	}
}

func (g *GC) safeCollect(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("gc: PANIC recovered: %v\n%s", rec, string(debug.Stack()))
		}
	}()
	// Per-tick timeout prevents a hung Docker call from stalling GC forever.
	tickCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := g.collectOnce(tickCtx); err != nil {
		log.Printf("gc: error: %v", err)
	}
}

func (g *GC) collectOnce(ctx context.Context) error {
	var removed int

	// 1. Orphaned containers: ah-labeled containers not in DB
	orphanedContainers, err := g.findOrphanedContainers(ctx)
	if err != nil {
		log.Printf("gc: orphaned container check failed: %v", err)
	} else {
		for _, id := range orphanedContainers {
			log.Printf("gc: removing orphaned container %s", short(id))
			_ = g.docker.StopContainer(ctx, id)
			if err := g.docker.RemoveContainer(ctx, id); err != nil {
				log.Printf("gc: failed to remove container %s: %v", short(id), err)
			} else {
				removed++
			}
		}
	}

	// 2. Orphaned volumes: ah-db-* volumes not in DB
	orphanedVolumes, err := g.findOrphanedVolumes(ctx)
	if err != nil {
		log.Printf("gc: orphaned volume check failed: %v", err)
	} else {
		for _, name := range orphanedVolumes {
			log.Printf("gc: removing orphaned volume %s", name)
			if err := g.docker.RemoveVolumeSafe(ctx, name); err != nil {
				log.Printf("gc: failed to remove volume %s: %v", name, err)
			} else {
				removed++
			}
		}
	}

	// 3. Old build work dirs (older than 1 hour)
	buildDirsCleaned := g.cleanOldBuildDirs(g.buildDir, 1*time.Hour)
	removed += buildDirsCleaned

	// 4. Dangling images — prune images not referenced by any container
	pruned, pruneErr := g.docker.PruneDanglingImages(ctx)
	if pruneErr != nil {
		log.Printf("gc: image prune failed: %v", pruneErr)
	} else if pruned > 0 {
		log.Printf("gc: pruned %d dangling images", pruned)
		removed += pruned
	}

	removed += g.removeUnusedBuiltImages(ctx)
	if n, err := g.docker.PruneAnonymousVolumes(ctx); err != nil {
		log.Printf("gc: anonymous volume prune failed: %v", err)
	} else {
		removed += n
	}

	// 5. Orphaned tenant and per-workload ingress networks.
	orphanedNets, netErr := g.findOrphanedNetworks(ctx)
	if netErr != nil {
		log.Printf("gc: orphaned network check failed: %v", netErr)
	} else {
		for _, net := range orphanedNets {
			log.Printf("gc: removing orphaned network %s", net.Name)
			// Disconnect Traefik first (by container name); idempotent if not connected.
			if disconnErr := g.docker.NetworkDisconnect(ctx, net.Name, "paas-traefik"); disconnErr != nil {
				log.Printf("gc: failed to disconnect Traefik from network %s: %v", net.Name, disconnErr)
			}
			if rmErr := g.docker.RemoveNetwork(ctx, net.Name); rmErr != nil {
				log.Printf("gc: failed to remove network %s: %v", net.Name, rmErr)
			} else {
				removed++
			}
		}
	}

	// 6. Snapshot retention — clean expired snapshots by count and age.
	if g.snapCleaner != nil {
		snapRemoved, snapErr := g.snapCleaner.CleanExpired(ctx, g.snapMaxPerService, g.snapMaxAge)
		if snapErr != nil {
			log.Printf("gc: snapshot retention cleanup failed: %v", snapErr)
		} else if snapRemoved > 0 {
			log.Printf("gc: removed %d expired snapshots", snapRemoved)
			removed += snapRemoved
		}
	}

	if removed > 0 {
		log.Printf("gc: removed %d orphaned resources", removed)
	}

	return nil
}

// containerTables are the tables whose container_id column marks a container as in use.
// Ownership is decided by container ID only, never by labels: warm-pool containers
// keep ah.type=warm-pool while pooled and are renamed (not relabelled) when claimed.
var containerTables = []string{"services", "databases", "environments", "kanbans", "warm_pool"}

func (g *GC) findOrphanedContainers(ctx context.Context) ([]string, error) {
	// Candidates: every container carrying any ah label GC manages.
	var candidates []string
	seen := map[string]bool{}
	for _, q := range [][2]string{{"ah.service", ""}, {"ah.type", "database"}, {"ah.type", "environment"}, {"ah.type", "kanban"}, {"ah.type", "warm-pool"}} {
		ids, err := g.docker.ListContainersByLabel(ctx, q[0], q[1])
		if err != nil {
			if q[0] == "ah.service" || q[1] == "database" {
				return nil, err
			}
			log.Printf("gc: %s=%s container list failed: %v", q[0], q[1], err)
			continue
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
			}
		}
	}

	var orphaned []string
	cutoff := time.Now().Add(-minResourceAge)
	for _, id := range candidates {
		// Skip containers younger than minResourceAge to avoid deploy races.
		info, inspectErr := g.docker.InspectContainer(ctx, id)
		if inspectErr != nil || info.CreatedAt.After(cutoff) {
			continue // can't verify or too new: don't delete
		}
		used, err := g.containerInUse(ctx, id)
		if err != nil {
			// DB error — do NOT treat as orphaned.
			log.Printf("gc: DB error checking container %s, skipping: %v", short(id), err)
			continue
		}
		if !used {
			orphaned = append(orphaned, id)
		}
	}
	return orphaned, nil
}

// containerInUse reports whether any table references the container ID.
func (g *GC) containerInUse(ctx context.Context, id string) (bool, error) {
	for _, table := range containerTables {
		var count int
		if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE container_id = ?`, id).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (g *GC) findOrphanedVolumes(ctx context.Context) ([]string, error) {
	volumes, err := g.docker.ListVolumes(ctx, "ah-db-")
	if err != nil {
		return nil, err
	}

	var orphaned []string
	for _, name := range volumes {
		// Strict prefix match — Docker name filter can be substring-based
		if !strings.HasPrefix(name, "ah-db-") {
			continue
		}

		var count int
		err := g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM databases WHERE volume_name = ?`, name).Scan(&count)
		if err != nil {
			// DB error — do NOT treat as orphaned. Log and skip.
			log.Printf("gc: DB error checking volume %s, skipping: %v", name, err)
			continue
		}
		if count == 0 {
			orphaned = append(orphaned, name)
		}
	}

	// Check environment volumes
	envVolumes, err := g.docker.ListVolumes(ctx, "ah-env-")
	if err != nil {
		log.Printf("gc: environment volume list failed: %v", err)
	} else {
		for _, name := range envVolumes {
			if !strings.HasPrefix(name, "ah-env-") {
				continue
			}

			var count int
			err := g.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM environments WHERE volume_name = ?`, name).Scan(&count)
			if err != nil {
				log.Printf("gc: DB error checking environment volume %s, skipping: %v", name, err)
				continue
			}
			if count == 0 {
				orphaned = append(orphaned, name)
			}
		}
	}

	// Check kanban volumes
	kanbanVolumes, err := g.docker.ListVolumes(ctx, "ah-kanban-")
	if err != nil {
		log.Printf("gc: kanban volume list failed: %v", err)
	} else {
		for _, name := range kanbanVolumes {
			if !strings.HasPrefix(name, "ah-kanban-") {
				continue
			}

			var count int
			err := g.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM kanbans WHERE volume_name = ?`, name).Scan(&count)
			if err != nil {
				log.Printf("gc: DB error checking kanban volume %s, skipping: %v", name, err)
				continue
			}
			if count == 0 {
				orphaned = append(orphaned, name)
			}
		}
	}

	// Check warm pool volumes
	poolVolumes, err := g.docker.ListVolumes(ctx, "ah-pool-")
	if err != nil {
		log.Printf("gc: pool volume list failed: %v", err)
	} else {
		for _, name := range poolVolumes {
			if !strings.HasPrefix(name, "ah-pool-") {
				continue
			}

			var count int
			err := g.db.QueryRowContext(ctx,
				`SELECT (SELECT COUNT(*) FROM warm_pool WHERE volume_name = ?) +
				        (SELECT COUNT(*) FROM environments WHERE volume_name = ?)`, name, name).Scan(&count)
			if err != nil {
				log.Printf("gc: DB error checking pool volume %s, skipping: %v", name, err)
				continue
			}
			if count == 0 {
				orphaned = append(orphaned, name)
			}
		}
	}

	return orphaned, nil
}

// minNetworkAge protects networks created by a deploy that is still pulling its image.
const minNetworkAge = 15 * time.Minute

// minImageAge protects images from a build or snapshot that is not yet recorded in the DB.
const minImageAge = time.Hour

// findOrphanedNetworks lists tenant and per-workload ingress networks with no
// workload endpoint, older than minNetworkAge and not owned by a persisted
// workload. A single Traefik endpoint is also orphaned. Tenant networks contain
// only workloads; ingress networks contain Traefik plus one workload while active.
func (g *GC) findOrphanedNetworks(ctx context.Context) ([]docker.NetworkInfo, error) {
	ownedNetworks, err := g.persistedWorkloadNetworks(ctx)
	if err != nil {
		return nil, err
	}
	networks, err := g.docker.NetworkList(ctx)
	if err != nil {
		return nil, err
	}

	var orphaned []docker.NetworkInfo
	for _, net := range networks {
		if !strings.HasPrefix(net.Name, "ah-tenant-") && !strings.HasPrefix(net.Name, "ah-ingress-") {
			continue
		}
		if _, owned := ownedNetworks[net.Name]; owned {
			continue
		}
		if time.Since(net.Created) < minNetworkAge {
			continue
		}
		// NetworkList may omit endpoint membership. Inspect every possible
		// orphan before removal; on inspection failure, abort this cleanup pass.
		if net.Containers > 1 {
			continue
		}
		inspected, err := g.docker.NetworkInspect(ctx, net.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect candidate network %s: %w", net.Name, err)
		}
		if inspected.Containers == 0 || (inspected.Containers == 1 && onlyTraefik(inspected.ContainerNames)) {
			// The container list includes stopped containers, which have no live
			// endpoint; skip the network if any non-Traefik container still uses it.
			names, err := g.docker.ListNetworkContainers(ctx, net.Name)
			if err != nil {
				log.Printf("gc: cannot list containers on network %s, skipping: %v", net.Name, err)
				continue
			}
			inUse := false
			for _, n := range names {
				if strings.TrimPrefix(n, "/") != "paas-traefik" {
					inUse = true
				}
			}
			if !inUse {
				inspected.Created = net.Created
				orphaned = append(orphaned, inspected)
			}
		}
	}
	return orphaned, nil
}

// persistedWorkloadNetworks returns Docker networks referenced by persisted
// workloads. Stopped Docker containers have no live endpoint, but retain their
// network IDs and cannot restart if GC removes those networks. Database errors
// must fail closed because ownership is unknown.
func (g *GC) persistedWorkloadNetworks(ctx context.Context) (map[string]struct{}, error) {
	owned := make(map[string]struct{})
	services, err := g.db.QueryContext(ctx, `SELECT tenant_id, id FROM services`)
	if err != nil {
		return nil, fmt.Errorf("query services for ingress ownership: %w", err)
	}
	for services.Next() {
		var tenantID, serviceID string
		if err := services.Scan(&tenantID, &serviceID); err != nil {
			_ = services.Close()
			return nil, fmt.Errorf("scan service ingress owner: %w", err)
		}
		owned[docker.TenantNetworkName(tenantID)] = struct{}{}
		owned[docker.ServiceIngressNetworkName(tenantID, serviceID)] = struct{}{}
	}
	if err := services.Err(); err != nil {
		_ = services.Close()
		return nil, fmt.Errorf("read service ingress owners: %w", err)
	}
	if err := services.Close(); err != nil {
		return nil, fmt.Errorf("close service ingress owners: %w", err)
	}

	environments, err := g.db.QueryContext(ctx, `
		SELECT e.tenant_id, e.id,
			EXISTS (SELECT 1 FROM environment_previews p WHERE p.environment_id = e.id AND p.tenant_id = e.tenant_id)
		FROM environments e`)
	if err != nil {
		return nil, fmt.Errorf("query environment ingress ownership: %w", err)
	}
	for environments.Next() {
		var tenantID, environmentID string
		var hasPreview bool
		if err := environments.Scan(&tenantID, &environmentID, &hasPreview); err != nil {
			_ = environments.Close()
			return nil, fmt.Errorf("scan environment ingress owner: %w", err)
		}
		owned[docker.TenantNetworkName(tenantID)] = struct{}{}
		if hasPreview {
			owned[docker.EnvironmentIngressNetworkName(tenantID, environmentID)] = struct{}{}
		}
	}
	if err := environments.Err(); err != nil {
		_ = environments.Close()
		return nil, fmt.Errorf("read environment ingress owners: %w", err)
	}
	if err := environments.Close(); err != nil {
		return nil, fmt.Errorf("close environment ingress owners: %w", err)
	}
	return owned, nil
}

func onlyTraefik(containerNames []string) bool {
	if len(containerNames) != 1 {
		return false
	}
	return strings.TrimPrefix(containerNames[0], "/") == "paas-traefik"
}

// removeUnusedBuiltImages removes tenant build and snapshot images that no
// container uses and no service or snapshot row references.
func (g *GC) removeUnusedBuiltImages(ctx context.Context) int {
	images, err := g.docker.ListImages(ctx)
	if err != nil {
		log.Printf("gc: image list failed: %v", err)
		return 0
	}
	ctrImages, err := g.docker.ListContainerImages(ctx)
	if err != nil {
		log.Printf("gc: container image list failed: %v", err)
		return 0
	}
	inUse := map[string]bool{}
	for _, r := range ctrImages {
		inUse[r] = true
	}
	for _, q := range []string{`SELECT image FROM services`, `SELECT image_ref FROM snapshots`} {
		rows, err := g.db.QueryContext(ctx, q)
		if err != nil {
			log.Printf("gc: image reference query failed: %v", err)
			return 0 // unknown references: remove nothing
		}
		for rows.Next() {
			var ref sql.NullString
			if rows.Scan(&ref) == nil {
				inUse[ref.String] = true
			}
		}
		rows.Close()
	}

	removed := 0
	for _, img := range images {
		if inUse[img.ID] || time.Since(img.Created) < minImageAge {
			continue
		}
		for _, tag := range img.Tags {
			if !strings.HasPrefix(tag, "127.0.0.1:5000/ah/") && !strings.HasPrefix(tag, "127.0.0.1:5000/snapshots/") {
				continue
			}
			if inUse[tag] {
				continue
			}
			if err := g.docker.RemoveImage(ctx, tag); err != nil {
				log.Printf("gc: failed to remove image %s: %v", tag, err)
			} else {
				removed++
			}
		}
	}
	return removed
}

func (g *GC) cleanOldBuildDirs(basePath string, maxAge time.Duration) int {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return 0
	}

	cutoff := time.Now().Add(-maxAge)
	var removed int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// Security: skip symlinks to prevent traversal attacks
		if info.Mode()&os.ModeSymlink != 0 {
			log.Printf("gc: skipping symlink in build dir: %s", entry.Name())
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(basePath, entry.Name())
			// Verify resolved path is still under basePath
			resolved, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				log.Printf("gc: cannot resolve path %s, skipping: %v", path, evalErr)
				continue
			}
			absBase, _ := filepath.Abs(basePath)
			if !strings.HasPrefix(resolved, absBase+string(os.PathSeparator)) && resolved != absBase {
				log.Printf("gc: SECURITY: path %s resolves outside base %s, skipping", path, basePath)
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				log.Printf("gc: failed to remove build dir %s: %v", path, err)
			} else {
				log.Printf("gc: removed old build dir %s", entry.Name())
				removed++
			}
		}
	}
	return removed
}
