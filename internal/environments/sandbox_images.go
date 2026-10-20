package environments

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
)

// Sandbox image pulls. A sandbox create can bring a public image of any size
// onto the host, so pulls are bounded host-wide: at most a few run at once,
// a public image over the size limit is removed again and refused, and the
// disk check is repeated once the image is on disk.

// Defaults, matching config.DefaultSandboxMaxConcurrentPulls and
// config.DefaultSandboxMaxImageMB (config is not imported here).
const (
	defaultSandboxMaxConcurrentPulls = 2
	defaultSandboxMaxImageMB         = 4096
	sandboxImageCleanupTimeout       = 2 * time.Minute
)

// SetSandboxPullLimits sets the host-wide limit on concurrent sandbox image
// pulls and the largest public image (in MB) a sandbox create may pull.
// Values below 1 keep the defaults. Call it once at startup, before the API
// serves requests.
func (m *Manager) SetSandboxPullLimits(maxConcurrent, maxImageMB int) {
	if maxConcurrent < 1 {
		maxConcurrent = defaultSandboxMaxConcurrentPulls
	}
	if maxImageMB < 1 {
		maxImageMB = defaultSandboxMaxImageMB
	}
	m.sandboxPullSem = make(chan struct{}, maxConcurrent)
	m.sandboxMaxImageMB = maxImageMB
}

// prepareSandboxImage makes a sandbox's image available before its
// container is created. It runs on the cold-start path, after the quota
// INSERT, so a tenant at its sandbox quota cannot trigger pulls.
//
//   - Template images are the operator's, pinned and pre-pulled: a local
//     copy is used without asking the registry, so template creates do not
//     depend on (or use up) the registry's rate limit. A missing one is
//     pulled.
//   - Images chosen by the tenant are always pulled, even when present
//     locally: the pull is what stops a tenant from running an image that
//     exists only on this host. A pulled image over the size limit is
//     refused (400), and removed when this create brought it onto the host
//     (fresh, from the pulled_images tracking).
//   - Snapshot and service images pull with a local fallback.
//
// Every pull waits for a slot in the host-wide pull semaphore, and the disk
// check runs again afterwards (503 if the pull crossed the block threshold).
func (m *Manager) prepareSandboxImage(ctx context.Context, sourceType string, r *resolvedSource, fresh bool, check func() error) error {
	if sourceType == SandboxSourceTemplate {
		if ok, err := m.docker.ImageExists(ctx, r.image); err == nil && ok {
			return nil
		}
	}
	if err := m.pullSandboxImage(ctx, r.image, r.localImage); err != nil {
		return err
	}
	public := sourceType == SandboxSourceImage && !r.localImage
	if public {
		size, err := m.docker.ImageSize(ctx, r.image)
		if err != nil {
			return fmt.Errorf("inspect image %s: %w", r.image, err)
		}
		if limit := int64(m.sandboxMaxImageMB) << 20; size > limit {
			log.Printf("environments: sandbox image %s is %d MB, over the %d MB limit", r.image, size>>20, m.sandboxMaxImageMB)
			m.removeFreshSandboxImage(ctx, r.image, fresh)
			return apierr.Validation(fmt.Sprintf("image too large: %s is %d MB; sandboxes on this server allow at most %d MB", r.image, size>>20, m.sandboxMaxImageMB))
		}
	}
	if err := check(); err != nil {
		log.Printf("environments: disk check after pulling %s refused sandbox create: %v", r.image, err)
		if public {
			m.removeFreshSandboxImage(ctx, r.image, fresh)
		}
		return apierr.Unavailable("insufficient disk space; try again later", err)
	}
	return nil
}

// pullSandboxImage pulls image while holding a slot of the host-wide pull
// semaphore. With local set, a failed pull is tolerated when the image is
// already present (snapshot and service build tags never exist upstream).
func (m *Manager) pullSandboxImage(ctx context.Context, image string, local bool) error {
	select {
	case m.sandboxPullSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("wait for an image pull slot: %w", ctx.Err())
	}
	defer func() { <-m.sandboxPullSem }()

	err := m.docker.PullImage(ctx, image)
	if err == nil {
		return nil
	}
	if local {
		if ok, _ := m.docker.ImageExists(ctx, image); ok {
			log.Printf("WARNING: pull of %s failed (%v); using local image", image, err)
			return nil
		}
	}
	return fmt.Errorf("pull image %s: %w", image, err)
}

// removeFreshSandboxImage removes a refused image that this create pulled
// onto the host (it was absent before and is recorded in pulled_images),
// and forgets it. Images that were already present are left alone. Docker
// refuses to remove an image a container still uses. Best effort: GC
// removes what is left once nothing uses it.
func (m *Manager) removeFreshSandboxImage(ctx context.Context, ref string, fresh bool) {
	if !fresh {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxImageCleanupTimeout)
	defer cancel()
	if err := m.docker.RemoveImage(cctx, ref); err != nil {
		log.Printf("environments: remove refused sandbox image %s (GC retries): %v", ref, err)
		return
	}
	if _, err := m.db.ExecContext(cctx, `DELETE FROM pulled_images WHERE ref = ?`, ref); err != nil {
		log.Printf("environments: forget removed sandbox image %s: %v", ref, err)
	}
}
