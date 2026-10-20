package environments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
)

// Disposable sandboxes are rows in the environments table with kind='sandbox'.
// They reuse the environment container (same isolation, quota-in-INSERT,
// warm pool, exec and teardown) but have a TTL after which the sandbox
// reaper deletes them, and no stop/start.

// Sandbox source types.
const (
	SandboxSourceTemplate = "template"
	SandboxSourceImage    = "image"
	SandboxSourceSnapshot = "snapshot"
	SandboxSourceService  = "service"
)

// Sandbox limits.
const (
	sandboxDefaultTTL       = 1800
	sandboxMinTTL           = 60
	sandboxDefaultMemoryMB  = 512
	sandboxDefaultCPUMillis = 1000
	sandboxMinMemoryMB      = 128
	sandboxMaxMemoryMB      = 2048
	sandboxMinCPUMillis     = 100
	sandboxMaxCPUMillis     = 2000
	sandboxMaxLogTail       = 10000
	sandboxDefaultLogTail   = 100
	sandboxDeleteTimeout    = 2 * time.Minute
)

// SnapshotSource resolves a tenant's snapshot to its image and env. It is
// wired in main so this package does not depend on the snapshots manager.
type SnapshotSource interface {
	// SnapshotImage returns the snapshot's image ref; a snapshot that does not
	// exist for tenantID must return an apierr NotFound.
	SnapshotImage(ctx context.Context, tenantID, snapshotID string) (string, error)
	// SnapshotEnv returns the snapshot's decrypted env vars.
	SnapshotEnv(ctx context.Context, tenantID, snapshotID string) (map[string]string, error)
}

// ServiceSource resolves a tenant's service to its current image and,
// when withEnv is set, its decrypted env vars.
type ServiceSource interface {
	ServiceImageAndEnv(ctx context.Context, tenantID, serviceID string, withEnv bool) (string, map[string]string, error)
}

// SandboxSource selects what a sandbox runs. Exactly the field matching Type
// is used.
type SandboxSource struct {
	Type       string `json:"type"`
	Template   string `json:"template,omitempty"`
	Image      string `json:"image,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	ServiceID  string `json:"service_id,omitempty"`
}

// SandboxCreateRequest holds parameters for creating a sandbox.
type SandboxCreateRequest struct {
	Name          string        `json:"name,omitempty"`
	Source        SandboxSource `json:"source"`
	TTLSeconds    *int          `json:"ttl_seconds,omitempty"`
	Run           string        `json:"run,omitempty"`
	IncludeEnv    bool          `json:"include_env,omitempty"`
	MemoryMB      *int          `json:"memory_mb,omitempty"`
	CPUMillicores *int          `json:"cpu_millicores,omitempty"`
	Egress        string        `json:"egress,omitempty"`
	// DiskMB sizes the tmpfs /workspace. Only accepted when the server runs
	// sandboxes with a tmpfs workspace (AH_SANDBOX_WORKSPACE_TMPFS).
	DiskMB *int `json:"disk_mb,omitempty"`
}

// SandboxSourceRef is the source recorded on a sandbox.
type SandboxSourceRef struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// Sandbox is the API view of a sandbox. It never carries the container ID,
// volume name or env values.
type Sandbox struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	Status         string           `json:"status"`
	Source         SandboxSourceRef `json:"source"`
	Image          string           `json:"image"`
	Run            string           `json:"run"`
	Egress         string           `json:"egress"`
	MemoryMB       int              `json:"memory_mb"`
	CPUMillicores  int              `json:"cpu_millicores"`
	DiskMB         int              `json:"disk_mb"` // tmpfs /workspace size; 0 = volume, no hard limit
	TTLSeconds     int              `json:"ttl_seconds"`
	CreatedAt      int64            `json:"created_at"`
	ExpiresAt      int64            `json:"expires_at"`
	MaxExpiresAt   int64            `json:"max_expires_at"`
	LastActivityAt int64            `json:"last_activity_at"`

	tenantID    string
	containerID string
	volumeName  string
}

// SetSandboxSources wires the snapshot and service lookups used by sandbox
// sources of type snapshot and service. Either may be nil, in which case that
// source type is unavailable.
func (m *Manager) SetSandboxSources(snap SnapshotSource, svc ServiceSource) {
	m.snapshotSource = snap
	m.serviceSource = svc
}

// SetMaxSandboxesTotal sets the host-wide cap on non-failed sandboxes,
// enforced inside the create INSERT. 0 disables the host cap.
func (m *Manager) SetMaxSandboxesTotal(n int) {
	if n < 0 {
		n = 0
	}
	m.maxSandboxesTotal = n
}

// SetSandboxDiskCheck replaces the free-disk check run before and after the
// image pull of each sandbox create, and by the sandbox reaper's disk
// watchdog (default: the deploy thresholds, 90% blocks, on /var/lib/ah and
// /var/lib/docker). It returns an error when usage is at the block
// threshold.
func (m *Manager) SetSandboxDiskCheck(fn func() error) {
	m.diskCheck = fn
}

// sandboxDiskCheck returns the configured disk check or the default.
func (m *Manager) sandboxDiskCheck() func() error {
	if m.diskCheck != nil {
		return m.diskCheck
	}
	return func() error { return diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90) }
}

// sandboxQuota is the tenant_quotas subset sandbox validation needs.
type sandboxQuota struct {
	maxTTL        int
	maxLifetime   int
	egressAllowed bool
	maxMemoryMB   int
	maxCPUMillis  int
	maxDiskMB     int
}

func (m *Manager) loadSandboxQuota(ctx context.Context, tenantID string) (*sandboxQuota, error) {
	q := &sandboxQuota{}
	var egress int
	var cpuCores float64
	err := m.db.QueryRowContext(ctx,
		`SELECT max_sandbox_ttl_seconds, max_sandbox_lifetime_seconds, sandbox_egress_allowed, max_memory_mb, max_cpu_cores,
		 max_sandbox_disk_mb
		 FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&q.maxTTL, &q.maxLifetime, &egress, &q.maxMemoryMB, &cpuCores, &q.maxDiskMB)
	if err != nil {
		return nil, fmt.Errorf("load sandbox quota: %w", err)
	}
	q.egressAllowed = egress == 1
	q.maxCPUMillis = int(cpuCores * 1000)
	if q.maxMemoryMB > sandboxMaxMemoryMB {
		q.maxMemoryMB = sandboxMaxMemoryMB
	}
	if q.maxCPUMillis > sandboxMaxCPUMillis {
		q.maxCPUMillis = sandboxMaxCPUMillis
	}
	return q, nil
}

// resolvedSource is a sandbox source turned into what create needs.
type resolvedSource struct {
	image      string
	runMode    string
	env        []string
	memoryMB   int
	cpuMillis  int
	templateID string // template_id column and warm-pool template ("" = no pool)
	sourceRef  string
	localImage bool
}

// resolveSource validates req.Source for tenantID and returns the image, run
// mode, env and resource defaults. Every resolved image goes through
// services.ValidateImage, so another tenant's local images, localhost and
// custom registries are rejected whatever the source.
func (m *Manager) resolveSource(ctx context.Context, tenantID string, req SandboxCreateRequest) (*resolvedSource, error) {
	src := req.Source
	r := &resolvedSource{memoryMB: sandboxDefaultMemoryMB, cpuMillis: sandboxDefaultCPUMillis}
	var envMap map[string]string

	switch src.Type {
	case SandboxSourceTemplate:
		if req.IncludeEnv {
			return nil, apierr.Validation("include_env is only valid for snapshot and service sources")
		}
		name := src.Template
		if name == "" {
			name = "default"
		}
		tmpl, err := m.GetTemplate(ctx, name)
		if err != nil {
			return nil, err
		}
		r.image = tmpl.BaseImage
		r.runMode = "idle"
		r.memoryMB, r.cpuMillis = tmpl.MemoryMB, tmpl.CPUMillis
		r.templateID = tmpl.ID
		r.sourceRef = tmpl.Name
	case SandboxSourceImage:
		if req.IncludeEnv {
			return nil, apierr.Validation("include_env is only valid for snapshot and service sources")
		}
		r.image = strings.TrimSpace(src.Image)
		r.runMode = "idle"
		r.sourceRef = r.image
		r.localImage = strings.HasPrefix(r.image, "127.0.0.1:5000/")
	case SandboxSourceSnapshot:
		if src.SnapshotID == "" {
			return nil, apierr.Validation("source.snapshot_id is required")
		}
		if m.snapshotSource == nil {
			return nil, apierr.Unavailable("snapshot sources are not available", nil)
		}
		img, err := m.snapshotSource.SnapshotImage(ctx, tenantID, src.SnapshotID)
		if err != nil {
			return nil, err
		}
		if req.IncludeEnv {
			if envMap, err = m.snapshotSource.SnapshotEnv(ctx, tenantID, src.SnapshotID); err != nil {
				return nil, err
			}
		}
		r.image, r.runMode, r.sourceRef, r.localImage = img, "app", src.SnapshotID, true
	case SandboxSourceService:
		if src.ServiceID == "" {
			return nil, apierr.Validation("source.service_id is required")
		}
		if m.serviceSource == nil {
			return nil, apierr.Unavailable("service sources are not available", nil)
		}
		img, env, err := m.serviceSource.ServiceImageAndEnv(ctx, tenantID, src.ServiceID, req.IncludeEnv)
		if err != nil {
			return nil, err
		}
		if img == "" {
			return nil, apierr.Conflict("service has no built image")
		}
		envMap = env
		r.image, r.runMode, r.sourceRef, r.localImage = img, "app", src.ServiceID, true
	case "":
		return nil, apierr.Validation("source.type is required (template, image, snapshot or service)")
	default:
		return nil, apierr.Validation(fmt.Sprintf("unknown source.type %q (want template, image, snapshot or service)", src.Type))
	}

	if err := services.ValidateImage(tenantID, r.image); err != nil {
		return nil, err
	}
	if len(envMap) > 0 {
		if err := services.ValidateEnvVars(envMap); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		r.env = make([]string, 0, len(keys))
		for _, k := range keys {
			r.env = append(r.env, k+"="+envMap[k])
		}
	}
	return r, nil
}

// CreateSandbox provisions a disposable sandbox. With egress "none" (the
// default) the sandbox is attached only to the tenant's internal network.
// Egress "internet" needs the operator's egress network and the tenant's
// sandbox_egress_allowed quota (see sandboxEgressGate); such a sandbox is
// attached only to the egress bridge.
func (m *Manager) CreateSandbox(ctx context.Context, tenantID string, req SandboxCreateRequest) (*Sandbox, error) {
	egress, err := parseSandboxEgress(req.Egress)
	if err != nil {
		return nil, err
	}
	if egress == SandboxEgressInternet && m.sandboxEgressNetwork == "" {
		return nil, errEgressUnavailable
	}
	switch req.Run {
	case "", "idle", "app":
	default:
		return nil, apierr.Validation(fmt.Sprintf("unknown run mode %q (want idle or app)", req.Run))
	}
	if req.Name != "" {
		if err := validateSandboxName(req.Name); err != nil {
			return nil, err
		}
	}

	q, err := m.loadSandboxQuota(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	egressNetwork, err := m.sandboxEgressGate(egress, q)
	if err != nil {
		return nil, err
	}

	ttl := sandboxDefaultTTL
	if req.TTLSeconds != nil {
		ttl = *req.TTLSeconds
	}
	maxTTL := q.maxTTL
	if q.maxLifetime < maxTTL {
		maxTTL = q.maxLifetime
	}
	if ttl < sandboxMinTTL || ttl > maxTTL {
		if req.TTLSeconds == nil && maxTTL >= sandboxMinTTL {
			ttl = maxTTL // the default exceeds a tighter tenant quota
		} else {
			return nil, apierr.Validation(fmt.Sprintf("ttl_seconds must be between %d and %d", sandboxMinTTL, maxTTL))
		}
	}

	r, err := m.resolveSource(ctx, tenantID, req)
	if err != nil {
		return nil, err
	}
	if req.Run != "" {
		r.runMode = req.Run
	}

	usePool := r.templateID != "" && r.runMode == "idle"
	memoryMB, cpuMillis := r.memoryMB, r.cpuMillis
	if req.MemoryMB != nil {
		if *req.MemoryMB < sandboxMinMemoryMB || *req.MemoryMB > q.maxMemoryMB {
			return nil, apierr.Validation(fmt.Sprintf("memory_mb must be between %d and %d", sandboxMinMemoryMB, q.maxMemoryMB))
		}
		usePool = usePool && *req.MemoryMB == memoryMB
		memoryMB = *req.MemoryMB
	} else if memoryMB > q.maxMemoryMB {
		memoryMB, usePool = q.maxMemoryMB, false
	}
	if req.CPUMillicores != nil {
		if *req.CPUMillicores < sandboxMinCPUMillis || *req.CPUMillicores > q.maxCPUMillis {
			return nil, apierr.Validation(fmt.Sprintf("cpu_millicores must be between %d and %d", sandboxMinCPUMillis, q.maxCPUMillis))
		}
		usePool = usePool && *req.CPUMillicores == cpuMillis
		cpuMillis = *req.CPUMillicores
	} else if cpuMillis > q.maxCPUMillis {
		cpuMillis, usePool = q.maxCPUMillis, false
	}
	if memoryMB < sandboxMinMemoryMB || cpuMillis < sandboxMinCPUMillis {
		return nil, apierr.Validation("tenant quota is too small for a sandbox")
	}
	diskMB, err := m.resolveSandboxDisk(req.DiskMB, memoryMB, q)
	if err != nil {
		return nil, err
	}

	check := m.sandboxDiskCheck()
	if err := check(); err != nil {
		log.Printf("environments: disk check refused sandbox create: %v", err)
		return nil, apierr.Unavailable("insufficient disk space; try again later", err)
	}

	name := req.Name
	if name == "" {
		suffix, err := generateID()
		if err != nil {
			return nil, err
		}
		name = "sbx-" + suffix[:8]
	}

	fresh := false
	if req.Source.Type == SandboxSourceImage && !r.localImage {
		fresh = m.trackPulledImage(ctx, r.image)
	}

	now := time.Now().Unix()
	maxExpiresAt := now + int64(q.maxLifetime)
	poolTmpl := ""
	if usePool {
		poolTmpl = r.templateID
	}
	c, err := m.create(ctx, tenantID, createSpec{
		kind:         KindSandbox,
		name:         name,
		templateID:   r.templateID,
		poolTmplID:   poolTmpl,
		image:        r.image,
		memoryMB:     memoryMB,
		cpuMillis:    cpuMillis,
		env:          r.env,
		useImageCmd:  r.runMode == "app",
		localImage:   r.localImage,
		leaseSec:     ttl,
		maxExpiresAt: maxExpiresAt,
		sourceType:   req.Source.Type,
		sourceRef:    r.sourceRef,
		runMode:      r.runMode,
		egress:       egress,

		runtime:          m.sandboxRuntime,
		workspaceTmpfsMB: diskMB,
		egressNetwork:    egressNetwork,
		prepareImage: func(ctx context.Context) error {
			return m.prepareSandboxImage(ctx, req.Source.Type, r, fresh, check)
		},
	})
	if err != nil {
		return nil, err
	}
	log.Printf("AUDIT: action=sandbox.create tenant=%s sandbox=%s source=%s:%s run=%s ttl=%d include_env=%t egress=%s disk_mb=%d",
		tenantID, c.id, req.Source.Type, r.sourceRef, r.runMode, ttl, req.IncludeEnv, egress, diskMB)

	return &Sandbox{
		ID:             c.id,
		Name:           name,
		Status:         "running",
		Source:         SandboxSourceRef{Type: req.Source.Type, Ref: r.sourceRef},
		Image:          r.image,
		Run:            r.runMode,
		Egress:         egress,
		MemoryMB:       memoryMB,
		CPUMillicores:  cpuMillis,
		DiskMB:         diskMB,
		TTLSeconds:     ttl,
		CreatedAt:      c.now,
		ExpiresAt:      c.leaseExpiresAt,
		MaxExpiresAt:   maxExpiresAt,
		LastActivityAt: c.now,
		tenantID:       tenantID,
		containerID:    c.containerID,
		volumeName:     c.volumeName,
	}, nil
}

// validateSandboxName applies the environment name rules with sandbox wording.
func validateSandboxName(name string) error {
	if len(name) > 63 {
		return apierr.Validation("sandbox name must be 63 characters or fewer")
	}
	if !namePattern.MatchString(name) {
		return apierr.Validation("sandbox name must start with a letter and contain only alphanumeric characters, hyphens, and underscores")
	}
	return nil
}

// sandboxColumns is the column list scanned by scanSandbox.
const sandboxColumns = `id, tenant_id, name, status, COALESCE(container_id, ''), COALESCE(volume_name, ''),
	 source_type, source_ref, image_ref, run_mode, egress, memory_mb, cpu_millicores, disk_mb,
	 lease_duration_seconds, COALESCE(lease_expires_at, 0), COALESCE(max_expires_at, 0),
	 COALESCE(last_activity_at, 0), created_at`

func scanSandbox(row interface{ Scan(...any) error }) (*Sandbox, error) {
	s := &Sandbox{}
	err := row.Scan(&s.ID, &s.tenantID, &s.Name, &s.Status, &s.containerID, &s.volumeName,
		&s.Source.Type, &s.Source.Ref, &s.Image, &s.Run, &s.Egress, &s.MemoryMB, &s.CPUMillicores, &s.DiskMB,
		&s.TTLSeconds, &s.ExpiresAt, &s.MaxExpiresAt, &s.LastActivityAt, &s.CreatedAt)
	return s, err
}

// GetSandbox returns a sandbox by ID, scoped to tenant. Environment rows
// return not found.
func (m *Manager) GetSandbox(ctx context.Context, tenantID, sandboxID string) (*Sandbox, error) {
	s, err := scanSandbox(m.db.QueryRowContext(ctx,
		`SELECT `+sandboxColumns+` FROM environments WHERE id = ? AND tenant_id = ? AND kind = 'sandbox'`,
		sandboxID, tenantID))
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("sandbox not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	return s, nil
}

// ListSandboxes returns a tenant's sandboxes, newest first, optionally
// filtered by status.
func (m *Manager) ListSandboxes(ctx context.Context, tenantID string, limit, offset int, status string) ([]*Sandbox, error) {
	// The API accepts up to 200 (maxPageLimit); honour that instead of
	// silently falling back to the default for 101-200.
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT ` + sandboxColumns + ` FROM environments WHERE tenant_id = ? AND kind = 'sandbox'`
	args := []any{tenantID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()
	result := make([]*Sandbox, 0)
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sandbox: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// DeleteSandbox claims the sandbox ('deleting') and tears it down. A create
// still in progress loses its final commit and removes what it made. If the
// container or volume cannot be removed the row stays 'deleting' and the
// sandbox reaper retries.
func (m *Manager) DeleteSandbox(ctx context.Context, tenantID, sandboxID string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE environments SET status = 'deleting', updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND kind = 'sandbox'`,
		time.Now().Unix(), sandboxID, tenantID)
	if err != nil {
		return fmt.Errorf("claim sandbox: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierr.NotFound("sandbox not found")
	}
	if err := m.teardownSandbox(ctx, tenantID, sandboxID); err != nil {
		return err
	}
	log.Printf("AUDIT: action=sandbox.delete tenant=%s sandbox=%s", tenantID, sandboxID)
	return nil
}

// teardownSandbox removes a claimed sandbox, detached from the caller so a
// cancelled request cannot stop it halfway.
func (m *Manager) teardownSandbox(ctx context.Context, tenantID, sandboxID string) error {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxDeleteTimeout)
	defer cancel()
	e, err := m.get(tctx, tenantID, sandboxID, KindSandbox)
	if err != nil {
		if errors.Is(err, apierr.ErrNotFound) {
			return nil // already gone
		}
		return err
	}
	return m.teardown(tctx, e, true)
}

// ExecSandbox runs a command in a running sandbox. Same contract as
// environment exec.
func (m *Manager) ExecSandbox(ctx context.Context, tenantID, sandboxID string, req ExecRequest) (*ExecResponse, error) {
	e, err := m.get(ctx, tenantID, sandboxID, KindSandbox)
	if err != nil {
		return nil, err
	}
	return m.execIn(ctx, e, KindSandbox, req)
}

// SandboxLogs streams the docker logs of the sandbox's main process. tail is
// clamped to 1..10000 (default 100).
func (m *Manager) SandboxLogs(ctx context.Context, tenantID, sandboxID string, follow bool, tail int) (io.ReadCloser, error) {
	s, err := m.GetSandbox(ctx, tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if s.containerID == "" {
		return nil, apierr.Conflict(fmt.Sprintf("sandbox has no container (status: %s)", s.Status))
	}
	return m.docker.LogsContainer(ctx, s.containerID, follow, ClampLogTail(tail))
}

// ClampLogTail bounds a requested log tail.
func ClampLogTail(tail int) int {
	if tail <= 0 {
		return sandboxDefaultLogTail
	}
	if tail > sandboxMaxLogTail {
		return sandboxMaxLogTail
	}
	return tail
}

// ExtendSandbox sets expires_at to now+ttl in one statement scoped by tenant,
// kind and status, so it cannot race the reaper's claim or extend a sandbox
// that is stopping. It never moves expires_at past max_expires_at.
func (m *Manager) ExtendSandbox(ctx context.Context, tenantID, sandboxID string, ttl int) (*Sandbox, error) {
	q, err := m.loadSandboxQuota(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if ttl < sandboxMinTTL || ttl > q.maxTTL {
		return nil, apierr.Validation(fmt.Sprintf("ttl_seconds must be between %d and %d", sandboxMinTTL, q.maxTTL))
	}
	now := time.Now().Unix()
	expires := now + int64(ttl)
	s, err := scanSandbox(m.db.QueryRowContext(ctx,
		`UPDATE environments SET lease_expires_at = ?, lease_duration_seconds = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND kind = 'sandbox' AND status = 'running'
		 AND lease_expires_at >= ? AND (max_expires_at IS NULL OR ? <= max_expires_at)
		 RETURNING `+sandboxColumns,
		expires, ttl, now, sandboxID, tenantID, now, expires))
	if err == nil {
		log.Printf("AUDIT: action=sandbox.extend tenant=%s sandbox=%s ttl=%d expires_at=%d", tenantID, sandboxID, ttl, expires)
		return s, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("extend sandbox: %w", err)
	}
	cur, gerr := m.GetSandbox(ctx, tenantID, sandboxID)
	if gerr != nil {
		return nil, gerr
	}
	if cur.Status != "running" || cur.ExpiresAt < now {
		return nil, apierr.Conflict(fmt.Sprintf("sandbox is not running (status: %s)", cur.Status))
	}
	left := cur.MaxExpiresAt - now
	if left < 0 {
		left = 0
	}
	return nil, apierr.Validation(fmt.Sprintf("ttl_seconds would pass the sandbox's maximum lifetime; at most %d seconds remain", left))
}

// trackPulledImage records a public image a sandbox create is about to pull,
// so GC can remove it once nothing uses it. Only an image absent locally is
// recorded (images already present, such as the operator's, are never GC'd
// this way); a ref recorded earlier has its timestamp refreshed so GC does
// not race this create. Best effort: failures only cost disk until pruned.
//
// It reports whether this create brings the image onto the host: absent
// locally and recorded in pulled_images. Only such an image is removed
// again when the create refuses it (too large, disk full).
func (m *Manager) trackPulledImage(ctx context.Context, ref string) bool {
	exists, existsErr := m.docker.ImageExists(ctx, ref)
	absent := existsErr == nil && !exists
	now := time.Now().Unix()
	res, err := m.db.ExecContext(ctx, `UPDATE pulled_images SET pulled_at = ? WHERE ref = ?`, now, ref)
	if err != nil {
		log.Printf("environments: track pulled image %s: %v", ref, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return absent
	}
	if !absent {
		return false
	}
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO pulled_images (ref, pulled_at) VALUES (?, ?) ON CONFLICT(ref) DO UPDATE SET pulled_at = excluded.pulled_at`,
		ref, now); err != nil {
		log.Printf("environments: track pulled image %s: %v", ref, err)
		return false
	}
	return true
}
