// Package builds coordinates build lifecycle between DB and builder.
package builds

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
)

const maxLogSize = 5 * 1024 * 1024 // 5MB max log per build
const tailLines = 500              // lines to preserve when log is truncated

// Build represents a build record.
type Build struct {
	ID           string `json:"id"`
	ServiceID    string `json:"service_id"`
	ServiceName  string `json:"service_name,omitempty"`
	TenantID     string `json:"tenant_id"`
	Status       string `json:"status"`
	SourceType   string `json:"source_type"`
	SourceURL    string `json:"source_url,omitempty"`
	SourceRef    string `json:"source_ref"`
	Image        string `json:"image,omitempty"`
	NixpacksPlan string `json:"nixpacks_plan,omitempty"`
	Log          string `json:"-"`
	StartedAt    *int64 `json:"started_at,omitempty"`
	FinishedAt   *int64 `json:"finished_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// StartBuildRequest holds parameters for starting a build.
type StartBuildRequest struct {
	SourceType string `json:"source_type"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref"`
}

// DeployFunc is called when a build succeeds to deploy the resulting image.
// The buildID parameter links the deployment to the originating build record.
type DeployFunc func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error

type buildExecutor interface {
	Build(ctx context.Context, req builder.BuildRequest, logCb func(string)) error
	CancelBuild(buildID string) error
}

// logState tracks per-build log accumulation and tail buffer.
type logState struct {
	size      int             // current log size in bytes
	truncated bool            // whether the log has been truncated
	tail      []string        // ring buffer of last tailLines lines
	tailIdx   int             // next write position in tail ring buffer
	tailCount int             // total lines written to tail (for omission count)
	pending   strings.Builder // lines accepted but not yet written to the DB
	flushMu   sync.Mutex      // serializes DB flushes so chunks stay in order
}

const (
	flushBytes    = 64 * 1024   // flush buffered log lines once this many bytes are pending
	flushInterval = time.Second // and at least this often while a build runs
)

// Manager coordinates build lifecycle.
type Manager struct {
	db         *sql.DB
	builder    buildExecutor
	deployFn   DeployFunc
	logMu      sync.Mutex
	buildQueue chan struct{}                 // bounded queue for build goroutines
	logSubs    map[string][]chan string      // buildID -> subscribers
	logStates  map[string]*logState          // buildID -> log accumulation state
	cancels    map[string]context.CancelFunc // buildID -> cancel for the running build (guarded by logMu)
}

// NewManager creates a build manager.
func NewManager(db *sql.DB, b buildExecutor, deployFn DeployFunc) *Manager {
	m := &Manager{
		db:         db,
		builder:    b,
		deployFn:   deployFn,
		buildQueue: make(chan struct{}, 10), // max 10 queued builds
		logSubs:    make(map[string][]chan string),
		logStates:  make(map[string]*logState),
		cancels:    make(map[string]context.CancelFunc),
	}
	// Mark any stale builds from previous process as failed
	m.reconcileStaleBuilds()
	return m
}

// reconcileStaleBuilds marks any builds stuck in pending/running as failed on startup.
func (m *Manager) reconcileStaleBuilds() {
	now := time.Now().Unix()
	result, err := m.db.Exec(
		`UPDATE builds SET status = 'failed', finished_at = ?, log = log || ? WHERE status IN ('pending', 'running')`,
		now, "[ah] Build failed: process restarted\n",
	)
	if err != nil {
		log.Printf("builds: reconcile stale builds error: %v", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("builds: marked %d stale builds as failed on startup", n)
	}
}

// cleanupStaleBuildDirs removes build directories not referenced by active builds.
func (m *Manager) cleanupStaleBuildDirs(workDir string) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Check if this dir corresponds to an active build
		var count int
		m.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE id = ? AND status IN ('pending', 'running')`, e.Name()).Scan(&count)
		if count == 0 {
			path := workDir + "/" + e.Name()
			if err := os.RemoveAll(path); err != nil {
				log.Printf("builds: failed to cleanup stale dir %s: %v", path, err)
			} else {
				log.Printf("builds: cleaned up stale build dir %s", e.Name())
			}
		}
	}
}

// ImageTag generates a deterministic image tag for local registry.
// Uses full IDs to prevent collisions between tenants.
func ImageTag(tenantID, serviceID, buildID string) string {
	return fmt.Sprintf("127.0.0.1:5000/ah/%s-%s:%s", tenantID, serviceID, buildID)
}

// StartBuild creates a build record and starts the build asynchronously.
func (m *Manager) StartBuild(ctx context.Context, tenantID, serviceID string, req StartBuildRequest) (*Build, error) {
	// Check disk space before starting build
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, fmt.Errorf("disk check: %w", err)
	}

	if req.SourceType != "git" {
		return nil, apierr.Validation(fmt.Sprintf("unsupported source_type: %s (only 'git' is supported)", req.SourceType))
	}
	if req.SourceURL == "" {
		return nil, apierr.Validation("source_url is required for git builds")
	}
	if err := validateGitURL(req.SourceURL); err != nil {
		return nil, err
	}

	ref := req.SourceRef
	if ref == "" {
		ref = "main"
	}
	if err := validateSourceRef(ref); err != nil {
		return nil, err
	}

	// Verify service exists and belongs to tenant
	var svcExists int
	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&svcExists)
	if err != nil || svcExists == 0 {
		return nil, apierr.NotFound("service not found")
	}

	buildID, err := generateID()
	if err != nil {
		return nil, err
	}

	imageTag := ImageTag(tenantID, serviceID, buildID)
	now := time.Now().Unix()

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?)`,
		buildID, serviceID, tenantID, req.SourceType, req.SourceURL, ref, imageTag, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert build: %w", err)
	}

	build := &Build{
		ID:         buildID,
		ServiceID:  serviceID,
		TenantID:   tenantID,
		Status:     "pending",
		SourceType: req.SourceType,
		SourceURL:  req.SourceURL,
		SourceRef:  ref,
		Image:      imageTag,
		CreatedAt:  now,
	}

	// Bounded build queue: reject if too many builds queued
	select {
	case m.buildQueue <- struct{}{}:
	default:
		// Queue is full, fail the build synchronously
		m.db.ExecContext(ctx,
			`UPDATE builds SET status = 'failed', finished_at = ? WHERE id = ?`,
			now, buildID,
		)
		return nil, apierr.Conflict("build queue full; try again later")
	}

	go m.runBuild(buildID, tenantID, serviceID, builder.BuildRequest{
		BuildID:    buildID,
		ServiceID:  serviceID,
		TenantID:   tenantID,
		SourceType: req.SourceType,
		SourceURL:  req.SourceURL,
		SourceRef:  ref,
		ImageTag:   imageTag,
	})

	return build, nil
}

func (m *Manager) runBuild(buildID, tenantID, serviceID string, req builder.BuildRequest) {
	defer func() { <-m.buildQueue }() // release queue slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	m.logMu.Lock()
	m.cancels[buildID] = cancel
	m.logMu.Unlock()

	// cbMu + finished stop late callbacks (the builder's output goroutines can outlive
	// Build) from recreating log state after cleanup below.
	var cbMu sync.Mutex
	finished := false
	stopFlusher := make(chan struct{})
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.flushPending(context.Background(), buildID)
			case <-stopFlusher:
				return
			}
		}
	}()
	// NOTE: flushTailBuffer is called explicitly before each final status write
	// (succeeded/failed) so that the DB log is fully flushed before any caller
	// polling on build status can observe the terminal state.
	defer func() {
		close(stopFlusher)
		cbMu.Lock()
		finished = true
		cbMu.Unlock()
		m.logMu.Lock()
		delete(m.logStates, buildID)
		delete(m.cancels, buildID)
		m.logMu.Unlock()
	}()

	now := time.Now().Unix()
	result, dbErr := m.db.ExecContext(ctx,
		`UPDATE builds SET status = 'running', started_at = ? WHERE id = ? AND status = 'pending'`,
		now, buildID,
	)
	if dbErr != nil {
		log.Printf("builds: failed to set build %s to running: %v", buildID, dbErr)
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		log.Printf("builds: build %s no longer pending (likely cancelled), aborting", buildID)
		return
	}

	logCb := func(line string) {
		cbMu.Lock()
		defer cbMu.Unlock()
		if finished {
			return
		}
		m.appendLog(context.Background(), buildID, line)
		m.notifyLogSubs(buildID, line)
	}

	err := m.builder.Build(ctx, req, logCb)

	// Use a fresh context for final DB writes so timeout doesn't prevent status update
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer finalCancel()

	finishedAt := time.Now().Unix()
	if err != nil {
		logCb("[ah] BUILD FAILED: " + err.Error())
		log.Printf("build %s failed: %v", buildID, err)
		// Flush tail buffer before writing final status so that any caller
		// polling on status=failed will see a complete log.
		m.flushTailBuffer(finalCtx, buildID)
		if _, dbErr := m.db.ExecContext(finalCtx,
			`UPDATE builds SET status = 'failed', finished_at = ? WHERE id = ? AND status = 'running'`,
			finishedAt, buildID,
		); dbErr != nil {
			log.Printf("builds: failed to mark build %s as failed: %v", buildID, dbErr)
		}
		m.closeLogSubs(buildID)
		return
	}

	// A cancel may have landed while the builder was finishing; do not deploy then.
	var status string
	if qErr := m.db.QueryRowContext(finalCtx, `SELECT status FROM builds WHERE id = ?`, buildID).Scan(&status); qErr != nil || status != "running" || ctx.Err() != nil {
		log.Printf("build %s no longer running (status %q), skipping deploy", buildID, status)
		m.flushTailBuffer(finalCtx, buildID)
		m.closeLogSubs(buildID)
		return
	}

	// Deploy the built image
	logCb("[ah] Build complete")
	if m.deployFn != nil {
		logCb("[ah] Deploying built image...")
		deployCtx, deployCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if deployErr := m.deployFn(deployCtx, tenantID, serviceID, req.ImageTag, buildID); deployErr != nil {
			log.Printf("build %s deploy failed: %v", buildID, deployErr)
			logCb("[ah] Deploy failed: " + deployErr.Error())
			// Flush tail buffer before writing final status so that any caller
			// polling on status=failed will see a complete log.
			m.flushTailBuffer(finalCtx, buildID)
			if _, dbErr := m.db.ExecContext(finalCtx,
				`UPDATE builds SET status = 'failed', finished_at = ? WHERE id = ? AND status = 'running'`,
				finishedAt, buildID,
			); dbErr != nil {
				log.Printf("builds: failed to mark build %s as failed after deploy error: %v", buildID, dbErr)
			}
			deployCancel()
			m.closeLogSubs(buildID)
			return
		} else {
			logCb("[ah] Deploy succeeded")
		}
		deployCancel()
	}
	// Flush tail buffer before writing final status so that any caller
	// polling on status=succeeded will see a complete log.
	m.flushTailBuffer(finalCtx, buildID)
	if _, dbErr := m.db.ExecContext(finalCtx,
		`UPDATE builds SET status = 'succeeded', finished_at = ? WHERE id = ? AND status = 'running'`,
		finishedAt, buildID,
	); dbErr != nil {
		log.Printf("builds: failed to mark build %s as succeeded: %v", buildID, dbErr)
	}
	m.closeLogSubs(buildID)
}

// appendLog accepts a log line. Lines are buffered and written to the DB in batches
// (flushBytes, or every flushInterval via runBuild's flusher) because appending to a
// log column of up to 5MB once per line is quadratic.
func (m *Manager) appendLog(ctx context.Context, buildID, line string) {
	m.logMu.Lock()
	ls := m.logStates[buildID]
	if ls == nil {
		ls = &logState{tail: make([]string, tailLines)}
		m.logStates[buildID] = ls
	}

	lineBytes := len(line) + 1 // +1 for newline

	if ls.truncated || ls.size+lineBytes > maxLogSize {
		// Log exceeded cap — only buffer in the ring buffer
		ls.truncated = true
		ls.tail[ls.tailIdx%tailLines] = line
		ls.tailIdx++
		ls.tailCount++
		m.logMu.Unlock()
		return
	}

	ls.size += lineBytes
	ls.pending.WriteString(line)
	ls.pending.WriteByte('\n')
	full := ls.pending.Len() >= flushBytes
	m.logMu.Unlock()

	if full {
		m.flushPending(ctx, buildID)
	}
}

// flushPending writes any buffered lines for the build to the DB, in order.
func (m *Manager) flushPending(ctx context.Context, buildID string) {
	m.logMu.Lock()
	ls := m.logStates[buildID]
	m.logMu.Unlock()
	if ls == nil {
		return
	}
	ls.flushMu.Lock()
	defer ls.flushMu.Unlock()
	m.logMu.Lock()
	chunk := ls.pending.String()
	ls.pending.Reset()
	m.logMu.Unlock()
	if chunk == "" {
		return
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE builds SET log = log || ? WHERE id = ?`, chunk, buildID); err != nil {
		log.Printf("builds: failed to append log for %s: %v", buildID, err)
	}
}

// appendLogDirect writes one line straight to the DB. It is for messages written
// outside runBuild (cancel notices), so it never creates in-memory log state.
func (m *Manager) appendLogDirect(ctx context.Context, buildID, line string) {
	m.flushPending(ctx, buildID) // keep ordering with buffered lines
	if _, err := m.db.ExecContext(ctx, `UPDATE builds SET log = log || ? || char(10) WHERE id = ?`, line, buildID); err != nil {
		log.Printf("builds: failed to append log for %s: %v", buildID, err)
	}
}

// flushTailBuffer replaces the DB log with a truncation notice + tail lines
// if the build log was truncated during accumulation.
func (m *Manager) flushTailBuffer(ctx context.Context, buildID string) {
	m.flushPending(ctx, buildID)
	m.logMu.Lock()
	ls := m.logStates[buildID]
	if ls == nil || !ls.truncated {
		m.logMu.Unlock()
		return
	}

	// Collect tail lines in order
	lines := collectTailLines(ls)
	omitted := ls.tailCount
	m.logMu.Unlock()

	header := fmt.Sprintf("[truncated: %d lines omitted, 5MB limit reached]\n", omitted)
	tail := strings.Join(lines, "\n") + "\n"

	// Replace the entire log with: original prefix (up to cap) is discarded,
	// replaced by truncation notice + last N lines.
	_, err := m.db.ExecContext(ctx,
		`UPDATE builds SET log = ? WHERE id = ?`,
		header+tail, buildID,
	)
	if err != nil {
		log.Printf("builds: failed to flush tail buffer for %s: %v", buildID, err)
	}
}

// collectTailLines extracts the tail ring buffer contents in chronological order.
func collectTailLines(ls *logState) []string {
	count := ls.tailCount
	if count > tailLines {
		count = tailLines
	}
	lines := make([]string, 0, count)
	start := ls.tailIdx - count
	if start < 0 {
		start = 0
	}
	for i := start; i < ls.tailIdx; i++ {
		lines = append(lines, ls.tail[i%tailLines])
	}
	return lines
}

// notifyLogSubs does non-blocking sends under logMu so a concurrent closeLogSubs
// cannot close a channel mid-send.
func (m *Manager) notifyLogSubs(buildID, line string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	for _, ch := range m.logSubs[buildID] {
		select {
		case ch <- line:
		default:
		}
	}
}

func (m *Manager) closeLogSubs(buildID string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	for _, ch := range m.logSubs[buildID] {
		close(ch)
	}
	delete(m.logSubs, buildID)
}

func (m *Manager) subscribeLogs(buildID string) chan string {
	ch := make(chan string, 100)
	m.logMu.Lock()
	m.logSubs[buildID] = append(m.logSubs[buildID], ch)
	m.logMu.Unlock()
	return ch
}

// unsubscribeLogs removes ch if closeLogSubs has not already done so.
func (m *Manager) unsubscribeLogs(buildID string, ch chan string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	subs := m.logSubs[buildID]
	for i, c := range subs {
		if c == ch {
			subs = append(subs[:i:i], subs[i+1:]...)
			break
		}
	}
	if len(subs) == 0 {
		delete(m.logSubs, buildID)
	} else {
		m.logSubs[buildID] = subs
	}
}

// GetBuild returns a build by ID, scoped to tenant.
func (m *Manager) GetBuild(ctx context.Context, tenantID, buildID string) (*Build, error) {
	b := &Build{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, service_id, tenant_id, status, source_type, source_url, source_ref, image, log, started_at, finished_at, created_at
		 FROM builds WHERE id = ? AND tenant_id = ?`,
		buildID, tenantID,
	).Scan(&b.ID, &b.ServiceID, &b.TenantID, &b.Status, &b.SourceType, &b.SourceURL, &b.SourceRef, &b.Image, &b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("build not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get build: %w", err)
	}
	return b, nil
}

// ListBuilds returns builds for a service, scoped to tenant.
func (m *Manager) ListBuilds(ctx context.Context, tenantID, serviceID string) ([]*Build, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT b.id, b.service_id, COALESCE(s.name, ''), b.tenant_id, b.status, b.source_type, b.source_url, b.source_ref,
		        b.image, b.started_at, b.finished_at, b.created_at
		 FROM builds b
		 LEFT JOIN services s ON s.id = b.service_id
		 WHERE b.tenant_id = ? AND b.service_id = ?
		 ORDER BY b.created_at DESC
		 LIMIT 50`,
		tenantID, serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list builds: %w", err)
	}
	defer rows.Close()

	var result []*Build
	for rows.Next() {
		b := &Build{}
		if err := rows.Scan(&b.ID, &b.ServiceID, &b.ServiceName, &b.TenantID, &b.Status, &b.SourceType, &b.SourceURL, &b.SourceRef, &b.Image, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan build: %w", err)
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// ListTenantBuilds returns recent builds across all services for a tenant.
func (m *Manager) ListTenantBuilds(ctx context.Context, tenantID string, limit int) ([]*Build, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT b.id, b.service_id, COALESCE(s.name, ''), b.tenant_id, b.status, b.source_type, b.source_url, b.source_ref,
		        b.image, b.started_at, b.finished_at, b.created_at
		 FROM builds b
		 LEFT JOIN services s ON s.id = b.service_id
		 WHERE b.tenant_id = ?
		 ORDER BY b.created_at DESC
		 LIMIT ?`,
		tenantID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list tenant builds: %w", err)
	}
	defer rows.Close()

	var result []*Build
	for rows.Next() {
		b := &Build{}
		if err := rows.Scan(&b.ID, &b.ServiceID, &b.ServiceName, &b.TenantID, &b.Status, &b.SourceType, &b.SourceURL, &b.SourceRef, &b.Image, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant build: %w", err)
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// GetBuildLogs returns the full build log.
func (m *Manager) GetBuildLogs(ctx context.Context, tenantID, buildID string) (string, error) {
	var logText string
	err := m.db.QueryRowContext(ctx,
		`SELECT log FROM builds WHERE id = ? AND tenant_id = ?`,
		buildID, tenantID,
	).Scan(&logText)
	if err == sql.ErrNoRows {
		return "", apierr.NotFound("build not found")
	}
	if err != nil {
		return "", fmt.Errorf("get build logs: %w", err)
	}
	return logText, nil
}

// StreamBuildLogs writes the existing log then streams new lines.
func (m *Manager) StreamBuildLogs(ctx context.Context, tenantID, buildID string, w io.Writer) error {
	// Subscribe before reading so a build that finishes in between still closes our channel.
	ch := m.subscribeLogs(buildID)
	defer m.unsubscribeLogs(buildID, ch)

	m.flushPending(ctx, buildID) // make buffered lines visible in the DB read below
	build, err := m.GetBuild(ctx, tenantID, buildID)
	if err != nil {
		return err
	}

	if build.Log != "" {
		if _, err := io.WriteString(w, build.Log); err != nil {
			return err
		}
	}

	if build.Status != "pending" && build.Status != "running" {
		return nil
	}

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return err
			}
			if f, ok := w.(interface{ Flush() }); ok {
				f.Flush()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// cancelActive moves a pending/running build to newStatus and stops its work.
// It reports false (and changes nothing) if the build already finished.
func (m *Manager) cancelActive(ctx context.Context, buildID, newStatus, msg string) bool {
	res, err := m.db.ExecContext(ctx,
		`UPDATE builds SET status = ?, finished_at = ? WHERE id = ? AND status IN ('pending', 'running')`,
		newStatus, time.Now().Unix(), buildID,
	)
	if err != nil {
		log.Printf("builds: cancel build %s: %v", buildID, err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	// Status is already terminal, so runBuild will not deploy. Now stop the work:
	// cancel its context (covers waiting on the build slot and gaps between phases)
	// and the systemd unit (may legitimately be untracked at this moment).
	m.logMu.Lock()
	cancel := m.cancels[buildID]
	m.logMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := m.builder.CancelBuild(buildID); err != nil {
		log.Printf("builds: stop build %s unit: %v", buildID, err)
	}
	m.appendLogDirect(ctx, buildID, msg)
	m.closeLogSubs(buildID)
	return true
}

// CancelBuild cancels a running build.
func (m *Manager) CancelBuild(ctx context.Context, tenantID, buildID string) error {
	build, err := m.GetBuild(ctx, tenantID, buildID)
	if err != nil {
		return err
	}
	if build.Status != "pending" && build.Status != "running" {
		return apierr.Conflict(fmt.Sprintf("build is not in progress (status: %s)", build.Status))
	}
	if !m.cancelActive(ctx, buildID, "failed", "[ah] Build cancelled by user") {
		return apierr.Conflict("build is no longer in progress")
	}
	return nil
}

// CancelAllForTenant cancels all pending and running builds for a tenant.
// Used during tenant suspension to free build slots and prevent orphaned builds.
func (m *Manager) CancelAllForTenant(ctx context.Context, tenantID string) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id FROM builds WHERE tenant_id = ? AND status IN ('pending', 'running')`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("query active builds for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()

	var buildIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan build id: %w", err)
		}
		buildIDs = append(buildIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active builds: %w", err)
	}

	for _, buildID := range buildIDs {
		m.cancelActive(ctx, buildID, "cancelled", "[ah] Build cancelled: tenant suspended")
	}

	if len(buildIDs) > 0 {
		log.Printf("builds: cancelled %d active builds for suspended tenant %s", len(buildIDs), tenantID)
	}
	return nil
}

// allowedGitHosts is the set of trusted git hosting providers.
// This prevents DNS rebinding attacks by only allowing known-good hostnames.
var allowedGitHosts = map[string]bool{
	"github.com":    true,
	"gitlab.com":    true,
	"bitbucket.org": true,
	"sr.ht":         true,
	"codeberg.org":  true,
}

// validRefPattern matches safe git ref names (branches, tags, SHAs).
var validRefPattern = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

func validateGitURL(rawURL string) error {
	if len(rawURL) > 2048 {
		return apierr.Validation("source_url too long (max 2048)")
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return apierr.Validation("only HTTPS git URLs are allowed")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return apierr.Validation(fmt.Sprintf("invalid git URL: %v", err))
	}

	if u.User != nil {
		return apierr.Validation("credentials in URL are not allowed")
	}

	host := u.Hostname()
	if host == "" {
		return apierr.Validation("empty hostname in URL")
	}

	// Only allow known trusted git hosting providers (prevents DNS rebinding)
	if !allowedGitHosts[strings.ToLower(host)] {
		return apierr.Validation(fmt.Sprintf("git host %q is not in the allowed list; supported: github.com, gitlab.com, bitbucket.org, sr.ht, codeberg.org", host))
	}

	return nil
}

func validateSourceRef(ref string) error {
	if len(ref) > 256 {
		return apierr.Validation("source_ref too long (max 256)")
	}
	if !validRefPattern.MatchString(ref) {
		return apierr.Validation("source_ref contains invalid characters")
	}
	return nil
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
