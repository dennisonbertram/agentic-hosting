// Package services coordinates DB records with Docker containers.
package services

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/docker/docker/errdefs"
)

// Service represents a deployed service.
type Service struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	Name          string `json:"name"`
	DNSLabel      string `json:"dns_label,omitempty"`
	Status        string `json:"status"`
	Image         string `json:"image"`
	ContainerID   string `json:"container_id,omitempty"`
	Port          int    `json:"port"`
	URL           string `json:"url,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	CrashCount    int    `json:"crash_count"`
	CircuitOpen   bool   `json:"circuit_open"`
	LastCrashedAt int64  `json:"last_crashed_at,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a new service.
type CreateRequest struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Port  int               `json:"port"`
	Env   map[string]string `json:"env"`
}

// maxConcurrentDeploys limits simultaneous deploy operations globally.
const maxConcurrentDeploys = 5

// maxQueuedDeploys limits how many deploys can be waiting for a slot.
// If the queue is full, new deploy requests are rejected with backpressure.
const maxQueuedDeploys = 20

// dockerHubImagePattern accepts Docker Hub image references without a registry prefix.
var dockerHubImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)?(?::[a-zA-Z0-9._-]+)?$`)

// localRegistryImagePattern accepts the platform-managed loopback registry and
// requires at least two path segments after the registry prefix.
var localRegistryImagePattern = regexp.MustCompile(`^127\.0\.0\.1:5000/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[a-zA-Z0-9._-]+)?$`)

// envKeyPattern validates environment variable key names.
var envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

// maxEnvValueLen is the maximum length of an environment variable value.
const maxEnvValueLen = 32768 // 32KB

// deniedEnvKeys are environment variable names that cannot be set by tenants.
var deniedEnvKeys = map[string]bool{
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"PATH":            true,
}

// isNotFoundError returns true if the error is a typed Docker "not found" error
// (container/image definitively does not exist). Transient errors return false;
// matching on message text would misfire on IDs or messages containing "404".
func isNotFoundError(err error) bool {
	return err != nil && errdefs.IsNotFound(err)
}

// resolvePort returns the port the container listens on: the service port
// (default 8000), overridden by a valid PORT env var. Deploy, Restart and the
// Traefik route written by UpdateName must all agree on this value.
func resolvePort(svcPort int, envVars map[string]string) int {
	port := svcPort
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		var parsed int
		if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}
	return port
}

// isAlreadyStoppedError returns true if the error indicates the container is already
// stopped / not running. This is a non-fatal condition for Stop-before-recreate flows.
func isAlreadyStoppedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "container already stopped")
}

// dnsLabelRe matches a valid DNS label: lowercase alphanumeric, hyphens allowed
// (not at start/end), max 63 chars.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// isDNSLabelSafe returns true if s is a valid DNS label.
func isDNSLabelSafe(s string) bool {
	return len(s) > 0 && len(s) <= 63 && dnsLabelRe.MatchString(s)
}

// normalizeDNSName lowercases name, replaces every non-alphanumeric character
// with a hyphen and trims leading/trailing hyphens. It does not truncate.
func normalizeDNSName(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// ValidateDNSName checks that a service name can produce a valid DNS label.
// Returns nil if OK, or an apierr.ValidationError describing the problem.
func ValidateDNSName(name string) error {
	label := normalizeDNSName(name)
	if label == "" {
		return apierr.Validation("service name must contain at least one alphanumeric character for DNS routing")
	}
	if len(label) > 63 {
		return apierr.Validation("service name too long for DNS routing (max 63 chars)")
	}
	return nil
}

// toDNSLabel derives a DNS-safe label from a service name.
// Lowercases, replaces non-alphanumeric with hyphens, trims hyphens, truncates to 63 chars.
func toDNSLabel(name string) string {
	s := normalizeDNSName(name)
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.TrimRight(s, "-")
}

// dnsLabelFor validates name and derives the subdomain label a service gets
// under the configured base domain. It returns "" when no base domain is set:
// localhost URLs are UUID-based, and claiming a globally unique label that
// cannot be routed would only squat it.
func (m *Manager) dnsLabelFor(name string) (string, error) {
	if m.baseDomain == "" {
		return "", nil
	}
	if err := ValidateDNSName(name); err != nil {
		return "", err
	}
	dnsLabel := toDNSLabel(name)
	if !isDNSLabelSafe(dnsLabel) {
		dnsLabel = "" // fallback to UUID-based URL
	}
	if reservedDNSLabels[dnsLabel] || strings.HasPrefix(dnsLabel, "env-") { // env- is reserved for environment preview hosts
		return "", apierr.Validation(fmt.Sprintf("service name %q is reserved and cannot be used as a subdomain", dnsLabel))
	}
	return dnsLabel, nil
}

// dnsLabelConflict maps a UNIQUE(dns_label) violation to a Conflict error and
// returns nil for any other error.
func dnsLabelConflict(err error, dnsLabel string) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "dns_label") {
		return apierr.Conflict("subdomain already taken: " + dnsLabel + " — choose a different service name")
	}
	return nil
}

// baseDomainRe validates a base domain: labels of lowercase alphanumeric/hyphens
// separated by single dots, no leading/trailing dot or hyphen, no consecutive dots.
var baseDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// reservedDNSLabels is a denylist of subdomains tenants cannot claim under the
// shared base domain. These names are either platform-reserved or are commonly
// expected to be platform endpoints.
var reservedDNSLabels = map[string]bool{
	"api": true, "admin": true, "dashboard": true, "traefik": true,
	"www": true, "mail": true, "smtp": true, "health": true,
	"status": true, "metrics": true, "grafana": true, "prometheus": true,
	"registry": true, "auth": true, "login": true, "oauth": true,
}

// publicURL returns the public URL for a service.
// baseDomain is validated before use; falls back to localhost if invalid.
func publicURL(serviceID, dnsLabel, baseDomain string) string {
	if baseDomain != "" && dnsLabel != "" && baseDomainRe.MatchString(baseDomain) {
		return fmt.Sprintf("https://%s.%s", dnsLabel, baseDomain)
	}
	return fmt.Sprintf("http://%s.localhost", serviceID)
}

// writeTraefikRoute writes a Traefik dynamic config file for a service.
// The file is read by Traefik's file provider and hot-reloaded.
//
// Two modes:
//   - Production (baseDomain set): routes {dnsLabel}.{baseDomain} via HTTPS/letsencrypt.
//   - Localhost  (baseDomain empty): routes {serviceID}.localhost via HTTP (web entrypoint).
//     This makes dev-mode services reachable at http://{serviceID}.localhost.
//
// Returns nil (no-op) when traefikConfigDir is empty.
func (m *Manager) writeTraefikRoute(serviceID, tenantID, dnsLabel, baseDomain string, port int) error {
	if m.traefikConfigDir == "" {
		return nil
	}

	containerName := docker.ServiceIngressAlias(tenantID, serviceID)
	portStr := strconv.Itoa(port)

	var content string
	if baseDomain != "" {
		// Production mode either terminates TLS locally (default) or uses the plain
		// HTTP entrypoint when a trusted upstream proxy terminates public TLS.
		if dnsLabel == "" {
			return nil
		}
		if !isDNSLabelSafe(dnsLabel) || !baseDomainRe.MatchString(baseDomain) {
			return nil
		}
		host := fmt.Sprintf("%s.%s", dnsLabel, baseDomain)
		if m.externalTLSTerminated {
			content = fmt.Sprintf(`http:
  routers:
    svc-%s:
      rule: "Host(`+"`%s`"+`)"
      entryPoints:
        - web
      middlewares:
        - svc-%s-public-https
      service: svc-%s
  middlewares:
    svc-%s-public-https:
      headers:
        customRequestHeaders:
          X-Forwarded-Proto: "https"
  services:
    svc-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%s"
`, serviceID, host, serviceID, serviceID, serviceID, serviceID, containerName, portStr)
		} else {
			content = fmt.Sprintf(`http:
  routers:
    svc-%s-http:
      rule: "Host(`+"`%s`"+`)"
      entryPoints:
        - web
      middlewares:
        - svc-%s-redirect-https
      service: svc-%s
    svc-%s:
      rule: "Host(`+"`%s`"+`)"
      entryPoints:
        - websecure
      service: svc-%s
      tls:
        certResolver: letsencrypt
  middlewares:
    svc-%s-redirect-https:
      redirectScheme:
        scheme: https
        permanent: true
  services:
    svc-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%s"
`, serviceID, host, serviceID, serviceID, serviceID, host, serviceID, serviceID, serviceID, containerName, portStr)
		}
	} else {
		// Localhost mode: HTTP-only, {serviceID}.localhost
		content = fmt.Sprintf(`http:
  routers:
    svc-%s:
      rule: "Host(`+"`%s.localhost`"+`)"
      entryPoints:
        - web
      service: svc-%s
  services:
    svc-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%s"
`, serviceID, serviceID, serviceID, serviceID, containerName, portStr)
	}

	// Write atomically: write to a temp file then rename to avoid Traefik
	// reading a partially-written config during a hot reload.
	path := filepath.Join(m.traefikConfigDir, serviceID+".yml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0640); err != nil {
		return fmt.Errorf("write traefik route tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename traefik route %s: %w", path, err)
	}
	return nil
}

// deleteTraefikRoute removes the Traefik dynamic config file for a service.
func (m *Manager) deleteTraefikRoute(serviceID string) error {
	if m.traefikConfigDir == "" {
		return nil
	}
	path := filepath.Join(m.traefikConfigDir, serviceID+".yml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete traefik route %s: %w", path, err)
	}
	return nil
}

// Manager coordinates service lifecycle between DB and Docker.
type Manager struct {
	db                    *sql.DB
	docker                docker.Client
	masterKey             []byte
	baseDomain            string
	externalTLSTerminated bool
	traefikConfigDir      string             // Traefik file provider directory; empty disables file-based routing
	deploySem             chan struct{}      // bounded deploy concurrency
	deployQueue           chan struct{}      // bounded queue for waiting deploys
	deployStore           *deployments.Store // deployment history tracking (nil-safe: failures are logged, not propagated)

	// svcLocks is a 256-slot striped mutex array that serialises per-service destructive
	// lifecycle operations (Deploy, Restart, ResetCircuitBreaker). Using a fixed-size array
	// avoids unbounded map growth while providing reasonable parallelism across services.
	// The slot is chosen by hashing the serviceID modulo 256.
	svcLocks [256]sync.Mutex
}

// SetExternalTLSTerminated configures routes for deployments where a trusted
// upstream proxy terminates public TLS before traffic reaches Traefik. Enabling
// it assumes public requests reach these routes only through that configured
// proxy; the route middleware then sets X-Forwarded-Proto to https.
func (m *Manager) SetExternalTLSTerminated(enabled bool) {
	m.externalTLSTerminated = enabled
}

// NewManager creates a service manager.
func NewManager(db *sql.DB, docker docker.Client, masterKey []byte, baseDomain, traefikConfigDir string, deployStore *deployments.Store) *Manager {
	if docker == nil {
		panic("ah: NewManager requires a non-nil Docker client")
	}
	return &Manager{
		db:               db,
		docker:           docker,
		masterKey:        masterKey,
		baseDomain:       baseDomain,
		traefikConfigDir: traefikConfigDir,
		deploySem:        make(chan struct{}, maxConcurrentDeploys),
		deployQueue:      make(chan struct{}, maxQueuedDeploys),
		deployStore:      deployStore,
	}
}

// recordDeployment creates a deployment record. Errors are logged but never returned
// because deployment tracking is observability, not critical path.
func (m *Manager) recordDeployment(ctx context.Context, d *deployments.Deployment) string {
	if m.deployStore == nil {
		return ""
	}
	if err := m.deployStore.Create(ctx, d); err != nil {
		log.Printf("services: failed to record deployment for service %s: %v", d.ServiceID, err)
		return ""
	}
	return d.ID
}

// updateDeploymentStatus updates a deployment record's status. Errors are logged but never
// returned because deployment tracking is observability, not critical path.
func (m *Manager) updateDeploymentStatus(ctx context.Context, deployID, status string, opts ...deployments.UpdateOption) {
	if m.deployStore == nil || deployID == "" {
		return
	}
	if err := m.deployStore.UpdateStatus(ctx, deployID, status, opts...); err != nil {
		log.Printf("services: failed to update deployment %s to %s: %v", deployID, status, err)
	}
}

// lockService acquires a striped mutex for the given serviceID and returns an unlock function.
// Uses a fixed 256-slot array hashed by serviceID to bound memory usage regardless of
// how many service IDs are presented by callers. Prevents concurrent destructive lifecycle
// operations (Deploy, Restart, ResetCircuitBreaker) from interleaving Docker and DB state.
func (m *Manager) lockService(serviceID string) func() {
	// djb2-style hash of serviceID bytes, mod 256.
	var h uint8
	for i := 0; i < len(serviceID); i++ {
		h = h*31 + serviceID[i]
	}
	mu := &m.svcLocks[h]
	mu.Lock()
	return mu.Unlock
}

// acquireDeploySlot enters the bounded deploy queue, failing fast with
// errDeployQueueFull (503 + Retry-After) when it is full (backpressure), then
// waits for one of the maxConcurrentDeploys slots. The returned func releases
// both. The only other error is ctx's. Deploy maps queue-full to its
// historical 409; Restart, DeployImage and ResetCircuitBreaker return the 503.
//
// LOCK ORDER: deployQueue → deploySem → lockService. Every container-creating
// path (Deploy, DeployImage, Restart, ResetCircuitBreaker) must acquire them in
// this order to prevent deadlocks.
func (m *Manager) acquireDeploySlot(ctx context.Context) (release func(), err error) {
	select {
	case m.deployQueue <- struct{}{}:
	default:
		return nil, errDeployQueueFull()
	}
	select {
	case m.deploySem <- struct{}{}:
		return func() { <-m.deploySem; <-m.deployQueue }, nil
	case <-ctx.Done():
		<-m.deployQueue
		return nil, ctx.Err()
	}
}

// failDeployment marks the service failed with msg and closes its deployment
// record as failed. The writes ignore ctx cancellation so a deploy that failed
// because its deadline passed is not left looking "deploying" forever.
func (m *Manager) failDeployment(ctx context.Context, tenantID, serviceID, deployID, msg string) {
	ctx = context.WithoutCancel(ctx)
	m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", msg)
	m.updateDeploymentStatus(ctx, deployID, deployments.StatusFailed, deployments.WithError(msg), deployments.WithCompletedAt(time.Now().Unix()))
}

// recordInstantDeployment records a lifecycle event (start/stop) that completes immediately.
func (m *Manager) recordInstantDeployment(ctx context.Context, svc *Service, status string) {
	now := time.Now().Unix()
	m.recordDeployment(ctx, &deployments.Deployment{
		ServiceID:   svc.ID,
		TenantID:    svc.TenantID,
		Image:       svc.Image,
		Status:      status,
		Trigger:     deployments.TriggerManual,
		StartedAt:   now,
		CompletedAt: &now,
	})
}

// removeContainerByName stops and removes the service container by its
// deterministic name, catching split-brain orphans whose ID the DB lost. It
// fails closed: an error is returned unless the container is confirmed gone.
func (m *Manager) removeContainerByName(ctx context.Context, name string) error {
	cleanErr := m.docker.StopAndRemoveByName(ctx, name)
	if cleanErr == nil || isNotFoundError(cleanErr) {
		return nil
	}
	log.Printf("services: cleanup by name %s failed: %v — verifying absence", name, cleanErr)
	_, verErr := m.docker.InspectContainer(ctx, name)
	if verErr == nil {
		return fmt.Errorf("container still exists after cleanup attempt (%s): %v — retry or redeploy", name, cleanErr)
	}
	if !isNotFoundError(verErr) {
		return fmt.Errorf("cannot confirm container absence (inspect error=%v, remove error=%v) — retry or redeploy", verErr, cleanErr)
	}
	return nil
}

// cidShort returns up to the first 12 characters of a container ID for logging.
// Guards against shorter-than-12-char IDs from corrupted state or future Docker versions.
func cidShort(containerID string) string {
	if len(containerID) <= 12 {
		return containerID
	}
	return containerID[:12]
}

// checkTenantActive verifies the tenant is not suspended/deleted.
// Returns an error if tenant is not in active state.
func (m *Manager) checkTenantActive(ctx context.Context, tenantID string) error {
	var status string
	err := m.db.QueryRowContext(ctx,
		`SELECT status FROM tenants WHERE id = ?`, tenantID,
	).Scan(&status)
	if err != nil {
		return apierr.Forbidden("tenant not found")
	}
	if status != "active" {
		return apierr.Forbidden(fmt.Sprintf("tenant is %s", status))
	}
	return nil
}

// ValidateImage checks that an image reference is allowed.
func ValidateImage(tenantID, img string) error {
	if img == "" {
		return apierr.Validation("image is required")
	}
	if len(img) > 256 {
		return apierr.Validation("image reference too long")
	}

	// Docker treats "localhost" as a registry host, so it is never allowed.
	if first, _, _ := strings.Cut(img, "/"); first == "localhost" || strings.HasPrefix(first, "localhost:") {
		return apierr.Validation("custom registries not allowed")
	}
	if localRegistryImagePattern.MatchString(img) {
		// Only the caller's own build output or snapshot images may be used.
		if strings.HasPrefix(img, "127.0.0.1:5000/ah/"+tenantID+"-") ||
			strings.HasPrefix(img, "127.0.0.1:5000/snapshots/"+tenantID+":") {
			return nil
		}
		return apierr.Validation("local registry image does not belong to this tenant")
	}

	if slashIdx := strings.IndexByte(img, '/'); slashIdx > 0 {
		prefix := img[:slashIdx]
		if strings.ContainsAny(prefix, ".:") {
			return apierr.Validation("custom registries not allowed; only Docker Hub or the local loopback registry are allowed")
		}
	}
	if !dockerHubImagePattern.MatchString(img) {
		return apierr.Validation("invalid image format")
	}
	return nil
}

// ValidateEnvVars checks env var keys and values for format and safety.
func ValidateEnvVars(vars map[string]string) error {
	for k, v := range vars {
		if !envKeyPattern.MatchString(k) {
			return apierr.Validation(fmt.Sprintf("invalid env var key %q: must match [A-Za-z_][A-Za-z0-9_]{0,127}", k))
		}
		if deniedEnvKeys[strings.ToUpper(k)] {
			return apierr.Validation(fmt.Sprintf("env var %q is not allowed", k))
		}
		if len(v) > maxEnvValueLen {
			return apierr.Validation(fmt.Sprintf("env var %q value too long (max %d bytes)", k, maxEnvValueLen))
		}
		if strings.ContainsAny(v, "\x00") {
			return apierr.Validation(fmt.Sprintf("env var %q value contains null bytes", k))
		}
	}
	return nil
}

// Create inserts a new service record (status=created, not yet running).
// Enforces tenant quota (max_services).
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Service, error) {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return nil, err
	}
	if err := ValidateImage(tenantID, req.Image); err != nil {
		return nil, err
	}
	// Only enforce DNS-safe name when a base domain is configured — when baseDomain
	// is empty all services use UUID-based localhost URLs and the name is cosmetic.
	dnsLabel, err := m.dnsLabelFor(req.Name)
	if err != nil {
		return nil, err
	}
	if err := ValidateEnvVars(req.Env); err != nil {
		return nil, err
	}

	// Enforce tenant quota
	var maxServices int
	err = m.db.QueryRowContext(ctx,
		`SELECT max_services FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxServices)
	if err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	port := req.Port
	if port == 0 {
		port = 8000 // default when omitted
	}
	if port < 1 || port > 65535 {
		return nil, apierr.Validation("port must be between 1 and 65535")
	}

	url := publicURL(id, dnsLabel, m.baseDomain)

	// Use a transaction so service insert + env vars are atomic.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Count and insert in one statement so concurrent creates cannot both pass
	// the quota check.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
		 SELECT ?, ?, ?, ?, 'created', ?, ?, '', ?, ?, ?
		 WHERE (SELECT COUNT(*) FROM services WHERE tenant_id = ?) < ?`,
		id, tenantID, req.Name, dnsLabel, req.Image, port, url, now, now, tenantID, maxServices,
	)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, apierr.QuotaExceeded(fmt.Sprintf("quota exceeded: maximum %d services reached", maxServices))
		}
	}
	if err != nil {
		if cErr := dnsLabelConflict(err, dnsLabel); cErr != nil {
			return nil, cErr
		}
		return nil, fmt.Errorf("insert service: %w", err)
	}

	if err := checkEnvVarQuota(ctx, tx, tenantID, id, req.Env); err != nil {
		return nil, err
	}
	if err := m.upsertEnvVarsTx(ctx, tx, id, req.Env, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit service create: %w", err)
	}

	return &Service{
		ID:        id,
		TenantID:  tenantID,
		Name:      req.Name,
		DNSLabel:  dnsLabel,
		Status:    "created",
		Image:     req.Image,
		Port:      port,
		URL:       url,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Deploy pulls the image, reads env vars, creates and starts the container.
// Uses a bounded semaphore with a bounded queue for backpressure.
func (m *Manager) Deploy(ctx context.Context, tenantID, serviceID string) error {
	if m.docker == nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "Docker client not configured")
		return fmt.Errorf("docker client not configured")
	}
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", err.Error())
		return err
	}
	release, err := m.acquireDeploySlot(ctx)
	if err != nil {
		if errors.Is(err, apierr.ErrUnavailable) { // queue full, not a cancelled caller
			m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "deploy queue full; try again later")
			return apierr.Conflict("deploy queue full; try again later") // 409 kept for Deploy (existing API contract)
		}
		return err
	}
	defer release()

	// Serialise with Restart/ResetCircuitBreaker to prevent interleaved Docker+DB mutations.
	unlock := m.lockService(serviceID)
	defer unlock()

	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.CircuitOpen {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "circuit breaker is open")
		return apierr.Conflict("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}

	// Check disk space before deploy
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", err.Error())
		return errDiskLow(err)
	}

	m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")

	// Record deployment event.
	now := time.Now().Unix()
	deployID := m.recordDeployment(ctx, &deployments.Deployment{
		ServiceID: serviceID,
		TenantID:  tenantID,
		Image:     svc.Image,
		Status:    deployments.StatusDeploying,
		Trigger:   deployments.TriggerManual,
		StartedAt: now,
	})

	// Ensure per-tenant network exists for isolation
	if _, err := m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID)); err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("network setup failed: %v", err))
		return apierr.DeployFailed(fmt.Errorf("ensure tenant network: %w", err))
	}

	// Always try to pull so mutable tags get refreshed. Snapshot images exist only in
	// the local store, so a failed pull is tolerated when the image is already local.
	if err := m.docker.PullImage(ctx, svc.Image); err != nil {
		if exists, _ := m.docker.ImageExists(ctx, svc.Image); !exists {
			m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("image pull failed: %v", err))
			return apierr.DeployFailed(fmt.Errorf("pull image: %w", err))
		}
		log.Printf("WARNING: pull of %s failed (%v); using local image for %s", svc.Image, err, serviceID)
	}

	// Re-read the row after the slow image pull for the authoritative container_id.
	svc, err = m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, "service deleted or unreadable during deploy")
		return fmt.Errorf("service deleted or circuit opened during deploy")
	}

	// Remove existing container before creating new one to prevent
	// "name already in use" errors on redeploy.
	if err := m.removeOldContainer(ctx, svc); err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("remove old container failed: %v", err))
		return apierr.DeployFailed(fmt.Errorf("remove old container: %w", err))
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("env vars load failed: %v", err))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := resolvePort(svc.Port, envVars)

	// Re-check the tenant after the (possibly slow) pull: a suspension that
	// landed meanwhile must not get a fresh container. StopAllForTenant has
	// already set the row's status, so only the deployment row is failed here.
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		m.updateDeploymentStatus(context.WithoutCancel(ctx), deployID, deployments.StatusFailed, deployments.WithError("tenant not active"), deployments.WithCompletedAt(time.Now().Unix()))
		return err
	}

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, svc.Image, port, envVars, nil, m.getResourceLimits(ctx, tenantID))
	if err != nil {
		m.failDeployment(ctx, tenantID, serviceID, deployID, fmt.Sprintf("container start failed: %v", err))
		return apierr.DeployFailed(fmt.Errorf("run container: %w", err))
	}
	if err := m.activateContainer(ctx, tenantID, svc, containerID, deployID, envVars); err != nil {
		return err
	}

	// Write Traefik file-provider route (non-fatal on error)
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s: %v", serviceID, err)
	}

	return nil
}

// removeOldContainer stops and removes svc's current container ahead of a
// redeploy, falling back to removal by deterministic name when removal by ID
// fails. On success container_id is cleared (compare-and-swap) so a failure
// later in the deploy does not leave the row pointing at a deleted container.
func (m *Manager) removeOldContainer(ctx context.Context, svc *Service) error {
	if svc.ContainerID == "" {
		return nil
	}
	log.Printf("services: removing existing container %s before redeploy of %s", cidShort(svc.ContainerID), svc.ID)
	if err := m.docker.StopContainer(ctx, svc.ContainerID); err != nil && !isNotFoundError(err) && !isAlreadyStoppedError(err) {
		log.Printf("services: stop error during redeploy teardown for %s: %v (proceeding)", svc.ID, err)
	}
	if err := m.docker.RemoveContainer(ctx, svc.ContainerID); err != nil && !isNotFoundError(err) {
		log.Printf("services: remove by id failed for %s: %v; falling back to name", svc.ID, err)
		if err := m.removeContainerByName(ctx, docker.ServiceContainerName(svc.TenantID, svc.ID)); err != nil {
			return err
		}
	}
	_, _ = m.db.ExecContext(ctx,
		`UPDATE services SET container_id = '', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
		time.Now().Unix(), svc.ID, svc.TenantID, svc.ContainerID,
	)
	return nil
}

// activeTenantGuard is appended to the final "status = 'running'" UPDATE of
// every deploy path (activateContainer; takes one extra tenantID arg). A tenant
// suspended while a deploy was in flight makes the update affect 0 rows, and
// the caller then removes the new container instead of leaving it running.
const activeTenantGuard = ` AND EXISTS (SELECT 1 FROM tenants WHERE id = ? AND status = 'active')`

// errDeployInterrupted is returned when the final state write of a deploy is
// rejected because the service changed underneath it.
const errDeployInterrupted = "service deleted, circuit opened or tenant suspended during deploy"

// errInterrupted is the sentinel activateContainer wraps when its final write
// matched no row. The service row then belongs to whoever changed it (Delete,
// the circuit breaker or StopAllForTenant), so callers must not overwrite it.
var errInterrupted = errors.New(errDeployInterrupted)

// discardContainer stops and removes a container this manager just created
// but cannot keep. It runs on a fresh bounded context so a cancelled or
// expired caller context cannot leave the container running and untracked.
func (m *Manager) discardContainer(ctx context.Context, serviceID, containerID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	if err := m.docker.StopContainer(cctx, containerID); err != nil && !isNotFoundError(err) && !isAlreadyStoppedError(err) {
		log.Printf("WARNING: stop of discarded container %s for service %s failed: %v", cidShort(containerID), serviceID, err)
	}
	if err := m.docker.RemoveContainer(cctx, containerID); err != nil && !isNotFoundError(err) {
		log.Printf("WARNING: remove of discarded container %s for service %s failed: %v (GC will retry)", cidShort(containerID), serviceID, err)
	}
}

// discardTimeout bounds best-effort cleanup Docker calls that run on a
// context detached from the caller's.
const discardTimeout = 30 * time.Second

// activateContainer finishes a deploy once containerID is running: it attaches
// the service's database networks and records the container as the service's
// live one. On any failure the new container is discarded, the deployment
// record is closed as failed, and an error is returned. All failure writes run
// on a context detached from ctx: when ctx's deadline is what failed the
// deploy, writes on it would fail too and leave the rows 'deploying'.
func (m *Manager) activateContainer(ctx context.Context, tenantID string, svc *Service, containerID, deployID string, envVars map[string]string) error {
	if err := m.attachDatabaseNetworks(ctx, tenantID, containerID, envVars); err != nil {
		m.discardContainer(ctx, svc.ID, containerID)
		m.failDeployment(ctx, tenantID, svc.ID, deployID, fmt.Sprintf("database network attach failed: %v", err))
		return apierr.DeployFailed(fmt.Errorf("attach database networks: %w", err))
	}
	res, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = 'running', container_id = ?, url = ?, last_error = '', last_restart_count = NULL, updated_at = ? WHERE id = ? AND tenant_id = ? AND circuit_open = 0`+activeTenantGuard,
		containerID, publicURL(svc.ID, svc.DNSLabel, m.baseDomain), time.Now().Unix(), svc.ID, tenantID, tenantID,
	)
	if err != nil {
		// Container is running but DB update failed; clean up to avoid an orphan.
		m.discardContainer(ctx, svc.ID, containerID)
		m.failDeployment(ctx, tenantID, svc.ID, deployID, fmt.Sprintf("db update failed: %v", err))
		return fmt.Errorf("update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Service was deleted, circuit opened or tenant suspended while we were
		// deploying; whoever did that owns the service row now.
		log.Printf("WARNING: service %s was deleted (or circuit opened, or tenant suspended) during deploy; removing orphan container %s", svc.ID, containerID)
		m.discardContainer(ctx, svc.ID, containerID)
		m.updateDeploymentStatus(context.WithoutCancel(ctx), deployID, deployments.StatusFailed, deployments.WithError(errDeployInterrupted), deployments.WithCompletedAt(time.Now().Unix()))
		return errInterrupted
	}
	m.updateDeploymentStatus(ctx, deployID, deployments.StatusRunning, deployments.WithContainerID(containerID), deployments.WithCompletedAt(time.Now().Unix()))
	return nil
}

// getResourceLimits reads per-service resource limits from tenant quotas.
func (m *Manager) getResourceLimits(ctx context.Context, tenantID string) *docker.ResourceLimits {
	var maxMemMB int
	var maxCPUCores float64
	err := m.db.QueryRowContext(ctx,
		`SELECT max_memory_mb, max_cpu_cores FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxMemMB, &maxCPUCores)
	if err != nil {
		log.Printf("services: failed to load resource limits for tenant %s: %v (using defaults)", tenantID, err)
		return nil // use defaults
	}
	limits := &docker.ResourceLimits{}
	if maxMemMB > 0 {
		limits.MemoryMB = int64(maxMemMB)
	}
	if maxCPUCores > 0 {
		limits.CPUCores = maxCPUCores
	}
	return limits
}

// Stop stops a running service container.
func (m *Manager) Stop(ctx context.Context, tenantID, serviceID string) error {
	// Verify existence cheaply before holding a lock stripe.
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}

	// Lock before re-reading state to prevent races with Restart/Deploy rotating container_id.
	unlock := m.lockService(serviceID)
	defer unlock()

	// Re-read under lock to get authoritative container_id.
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.ContainerID == "" {
		return apierr.Conflict("service has no container")
	}

	switch err := m.docker.StopContainer(ctx, svc.ContainerID); {
	case err == nil || isAlreadyStoppedError(err):
		m.updateStatusScoped(ctx, tenantID, serviceID, "stopped")
	case isNotFoundError(err):
		// Container no longer exists; clear the stale container_id with CAS
		// and report stopped (self-heal, same pattern as Start).
		_, _ = m.db.ExecContext(ctx,
			`UPDATE services SET container_id = '', status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
			time.Now().Unix(), serviceID, tenantID, svc.ContainerID,
		)
	default:
		return fmt.Errorf("stop container: %w", err)
	}
	m.recordInstantDeployment(ctx, svc, deployments.StatusStopped)
	return nil
}

// Start starts a stopped service container.
// If the container no longer exists in Docker (e.g., after a circuit breaker reset),
// it clears the stale container_id and returns a clear error directing the user to redeploy.
func (m *Manager) Start(ctx context.Context, tenantID, serviceID string) error {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}

	// Verify existence cheaply before holding a lock stripe.
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}

	// Lock before re-reading state to prevent races with Restart/Deploy rotating container_id.
	unlock := m.lockService(serviceID)
	defer unlock()

	// Re-read under lock to get authoritative container_id.
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.CircuitOpen {
		return apierr.Conflict("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}
	if svc.ContainerID == "" {
		return apierr.Conflict("service has no container — use POST /redeploy to deploy it")
	}

	if err := m.docker.StartContainer(ctx, svc.ContainerID); err != nil {
		if isNotFoundError(err) {
			// Container no longer exists; clear the stale container_id using a compare-and-swap
			// update (WHERE container_id = ?) so we only clear if nobody else has updated it.
			_, _ = m.db.ExecContext(ctx,
				`UPDATE services SET container_id = '', status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
				time.Now().Unix(), serviceID, tenantID, svc.ContainerID,
			)
			return apierr.Conflict("container no longer exists — use POST /redeploy to deploy it")
		}
		return fmt.Errorf("start container: %w", err)
	}
	// The container is running again; restore its ingress, database networks and
	// route, or stop it so it never runs unreachable.
	if err := m.restoreStartedContainer(ctx, svc); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
		_ = m.docker.StopContainer(stopCtx, svc.ContainerID)
		cancel()
		return err
	}
	m.updateStatusScoped(ctx, tenantID, serviceID, "running")
	m.recordInstantDeployment(ctx, svc, deployments.StatusRunning)
	return nil
}

// restoreStartedContainer reconnects a restarted container to its service
// ingress network and database networks and rewrites its Traefik route.
func (m *Manager) restoreStartedContainer(ctx context.Context, svc *Service) error {
	containerInfo, err := m.docker.InspectContainer(ctx, svc.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect started container: %w", err)
	}
	envVars, err := m.getEnvVars(ctx, svc.ID)
	if err != nil {
		return fmt.Errorf("load env vars for route restore: %w", err)
	}
	port := containerInfo.EffectivePort
	if port < 1 || port > 65535 {
		port = resolvePort(svc.Port, envVars)
	}
	ingressID, err := m.docker.EnsureIngressNetwork(ctx, docker.ServiceIngressNetworkName(svc.TenantID, svc.ID))
	if err == nil {
		err = m.docker.ConnectNetwork(ctx, ingressID, "paas-traefik")
	}
	if err == nil {
		err = m.docker.ConnectNetworkAlias(ctx, ingressID, svc.ContainerID, docker.ServiceIngressAlias(svc.TenantID, svc.ID))
	}
	if err != nil {
		return fmt.Errorf("restore service ingress network: %w", err)
	}
	if err := m.attachDatabaseNetworks(ctx, svc.TenantID, svc.ContainerID, envVars); err != nil {
		return fmt.Errorf("restore database networks: %w", err)
	}
	if err := m.writeTraefikRoute(svc.ID, svc.TenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		return fmt.Errorf("restore service route: %w", err)
	}
	return nil
}

// Restart recreates the service container so that updated env vars take effect.
// Docker container env is immutable after creation; a stop+start does NOT apply
// env var changes. This method stops the old container, removes it, then runs a
// fresh container using the current image and env vars from the DB.
func (m *Manager) Restart(ctx context.Context, tenantID, serviceID string) (retErr error) {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}

	// Run disk-space preflight (same as Deploy) before acquiring any semaphore.
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return errDiskLow(err)
	}

	// Verify existence and ownership cheaply before acquiring global backpressure.
	pre, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	// No container (failed first deploy, failed restart, circuit reset): there is
	// nothing to restart, so run the full Deploy path (pull, create, route).
	// Deploy takes its own queue/semaphore/lock, so this must happen before ours.
	// Deploy runs on a detached, bounded context (as the async create path does):
	// callers pass a request context with a short HTTP timeout, and an image
	// pull or the failure-status writes must not die with it, or the service
	// and deployment rows are left stuck in 'deploying'.
	if pre.ContainerID == "" && !pre.CircuitOpen {
		deployCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartDeployTimeout)
		defer cancel()
		return m.Deploy(deployCtx, tenantID, serviceID)
	}

	// Restart creates a container and must participate in the same backpressure.
	release, err := m.acquireDeploySlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Per-service lock acquired AFTER global backpressure (consistent with Deploy order).
	unlock := m.lockService(serviceID)
	defer unlock()

	// Re-read service state *under the lock* to get authoritative post-lock values
	// (ContainerID, CircuitOpen, Image, Port, DNSLabel). Avoids TOCTOU with Deploy/Reset.
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.CircuitOpen {
		return apierr.Conflict("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}
	if svc.ContainerID == "" {
		return apierr.Conflict("service has no container — use POST /redeploy to deploy it")
	}

	// Record deployment event.
	restartNow := time.Now().Unix()
	deployID := m.recordDeployment(ctx, &deployments.Deployment{
		ServiceID: serviceID,
		TenantID:  tenantID,
		Image:     svc.Image,
		Status:    deployments.StatusDeploying,
		Trigger:   deployments.TriggerRestart,
		StartedAt: restartNow,
	})
	// Every error return below leaves the deployment row 'failed', not 'deploying'.
	defer func() {
		if retErr != nil {
			bg := context.WithoutCancel(ctx)
			m.updateDeploymentStatus(bg, deployID, deployments.StatusFailed, deployments.WithError(retErr.Error()), deployments.WithCompletedAt(time.Now().Unix()))
			// Failed before the old container was dropped: hand the row back to the reconciler.
			_, _ = m.db.ExecContext(bg,
				`UPDATE services SET status = ?, last_error = ? WHERE id = ? AND tenant_id = ? AND container_id = ? AND status = 'restarting'`,
				svc.Status, retErr.Error(), serviceID, tenantID, svc.ContainerID)
		}
	}()

	// Use a detached context for destructive operations so client disconnects do not
	// interrupt critical cleanup and DB state transitions mid-way.
	// context.WithoutCancel preserves request-scoped values while dropping client cancellation.
	opCtx, opCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer opCancel()

	// Take the row out of the reconciler's watch (it only scans running/crashed) BEFORE
	// touching the container, keeping container_id so a failure here stays recoverable.
	// Otherwise a tick between the stop and this update records a phantom crash.
	casRes, casErr := m.db.ExecContext(opCtx,
		`UPDATE services SET status = 'restarting', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ? AND circuit_open = 0`,
		time.Now().Unix(), serviceID, tenantID, svc.ContainerID)
	if casErr != nil {
		return fmt.Errorf("mark service restarting: %w", casErr)
	}
	if n, _ := casRes.RowsAffected(); n == 0 {
		return apierr.Conflict("service changed while restarting (circuit opened or container replaced); retry")
	}

	// Stop and remove the existing container.
	log.Printf("services: restarting service %s — stopping container %s", serviceID, cidShort(svc.ContainerID))
	if err := m.docker.StopContainer(opCtx, svc.ContainerID); err != nil {
		if isNotFoundError(err) {
			log.Printf("services: container %s already gone during restart of %s", cidShort(svc.ContainerID), serviceID)
		} else if isAlreadyStoppedError(err) {
			// Container is already stopped — safe to proceed to remove + recreate.
			log.Printf("services: container %s already stopped during restart of %s", cidShort(svc.ContainerID), serviceID)
		} else {
			return apierr.DeployFailed(fmt.Errorf("stop container: %w", err))
		}
	}
	if err := m.docker.RemoveContainer(opCtx, svc.ContainerID); err != nil {
		if !isNotFoundError(err) {
			return apierr.DeployFailed(fmt.Errorf("remove container: %w", err))
		}
	}
	// Fallback cleanup by deterministic container name in case DB had a stale container_id.
	// If the container cannot be confirmed gone, abort to avoid orphaning a running container.
	if err := m.removeContainerByName(opCtx, docker.ServiceContainerName(tenantID, serviceID)); err != nil {
		return apierr.DeployFailed(err)
	}

	// Clear container_id in DB immediately after removing the old container so that if
	// RunContainer fails below, the DB does not point at a non-existent container.
	// Status is set to 'restarting' to indicate an in-progress recreation.
	// Abort if the DB update fails — the old container is already removed, so we must
	// record the interim state before proceeding to avoid split-brain.
	dbRes, dbErr := m.db.ExecContext(opCtx,
		`UPDATE services SET container_id = '', status = 'restarting', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		time.Now().Unix(), serviceID, tenantID,
	)
	if dbErr != nil {
		// Old container is gone; delete Traefik route to prevent stale routing,
		// then mark failed with explanation.
		if traefikErr := m.deleteTraefikRoute(serviceID); traefikErr != nil {
			log.Printf("WARNING: failed to delete traefik route for service %s after DB clear failure: %v", serviceID, traefikErr)
		}
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("DB update failed mid-restart: %v", dbErr))
		return fmt.Errorf("clear container_id during restart: %w", dbErr)
	}
	if n, _ := dbRes.RowsAffected(); n == 0 {
		// Service was deleted while we were tearing down; nothing more to do.
		return fmt.Errorf("service deleted during restart teardown")
	}

	// Re-check tenant is still active before creating a new container.
	// The detached context means the original request may no longer be live, but
	// an admin could have suspended the tenant since we started.
	if err := m.checkTenantActive(opCtx, tenantID); err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", "tenant suspended during restart")
		return err
	}

	// Load current env vars from DB so the new container picks up any changes.
	envVars, err := m.getEnvVars(opCtx, serviceID)
	if err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("env vars load failed: %v", err))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := resolvePort(svc.Port, envVars)

	// Ensure per-tenant network exists (mirrors Deploy behaviour; absence could fall
	// back to default networking and break tenant isolation).
	if _, err := m.docker.EnsureNetwork(opCtx, docker.TenantNetworkName(tenantID)); err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("network setup failed: %v", err))
		return apierr.DeployFailed(fmt.Errorf("ensure tenant network: %w", err))
	}

	// The old container is gone, so if no new one comes up its route must go too.
	dropRoute := func() {
		if traefikErr := m.deleteTraefikRoute(serviceID); traefikErr != nil {
			log.Printf("WARNING: failed to delete traefik route for service %s after restart failure: %v", serviceID, traefikErr)
		}
	}

	// Run a fresh container with the current image and current env vars.
	// Skip image pull — the image is already local (same as redeploy without pull).
	containerID, err := m.docker.RunContainer(opCtx, tenantID, serviceID, svc.Image, port, envVars, nil, m.getResourceLimits(opCtx, tenantID))
	if err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("container restart failed: %v", err))
		dropRoute()
		return apierr.DeployFailed(fmt.Errorf("run container: %w", err))
	}
	if err := m.activateContainer(opCtx, tenantID, svc, containerID, deployID, envVars); err != nil {
		dropRoute()
		if errors.Is(err, errInterrupted) {
			// The row belongs to whoever deleted it, opened its circuit or
			// suspended its tenant; do not overwrite their status.
			return fmt.Errorf("service deleted, circuit opened or tenant suspended during restart")
		}
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("container restart failed: %v", err))
		return err
	}

	// Re-write Traefik route (non-fatal; port may have changed via PORT env var).
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s after restart: %v", serviceID, err)
	}

	return nil
}

// containersGone reports whether none of the given containers (IDs or names; empty skipped) exist.
// An inspect error other than not-found counts as "cannot confirm", i.e. not gone.
func (m *Manager) containersGone(ctx context.Context, refs ...string) bool {
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, err := m.docker.InspectContainer(ctx, ref); err == nil || !isNotFoundError(err) {
			return false
		}
	}
	return true
}

// Delete stops and removes the container, then deletes the DB record.
func (m *Manager) Delete(ctx context.Context, tenantID, serviceID string) error {
	// Verify existence cheaply before holding a lock stripe.
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}

	unlock := m.lockService(serviceID)
	defer unlock()

	// Re-read under the lock: a Deploy/Restart that held it may have replaced the
	// container, and the pre-lock container_id would then miss the live one.
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	removeFailed := false
	if svc.ContainerID != "" {
		if stopErr := m.docker.StopContainer(ctx, svc.ContainerID); stopErr != nil {
			log.Printf("WARNING: failed to stop container %s for service %s: %v", svc.ContainerID, serviceID, stopErr)
		}
		if rmErr := m.docker.RemoveContainer(ctx, svc.ContainerID); rmErr != nil {
			removeFailed = !isNotFoundError(rmErr)
			log.Printf("WARNING: failed to remove container %s for service %s: %v (orphan container may remain)", svc.ContainerID, serviceID, rmErr)
		}
	}
	// Also try cleanup by deterministic container name to catch split-brain orphans
	// where a container exists but DB doesn't have its ID.
	expectedName := docker.ServiceContainerName(tenantID, serviceID)
	cleanupErr := m.docker.StopAndRemoveByName(ctx, expectedName)
	if cleanupErr != nil {
		log.Printf("services: cleanup by name %s: %v", expectedName, cleanupErr)
	}
	// If any removal failed for a reason other than "not found", confirm the container is
	// really gone. Deleting the row while an unless-stopped container survives would
	// leave it running with nothing tracking it.
	if (removeFailed || (cleanupErr != nil && !isNotFoundError(cleanupErr))) && !m.containersGone(ctx, svc.ContainerID, expectedName) {
		return fmt.Errorf("container still exists after cleanup — retry delete")
	}

	// Remove Traefik file-provider route (non-fatal on error)
	if err := m.deleteTraefikRoute(serviceID); err != nil {
		log.Printf("WARNING: failed to delete traefik route for service %s: %v", serviceID, err)
	}
	if err := m.cleanupServiceIngressNetwork(ctx, tenantID, serviceID, svc.ContainerID); err != nil {
		log.Printf("WARNING: failed to clean ingress network for service %s: %v", serviceID, err)
	}

	// snapshots.service_id has no ON DELETE action, so snapshots must go first.
	// Untag their images best-effort; the rows and the service go in one transaction.
	var snapRefs []string
	if rows, qErr := m.db.QueryContext(ctx, `SELECT image_ref FROM snapshots WHERE service_id = ? AND tenant_id = ?`, serviceID, tenantID); qErr == nil {
		for rows.Next() {
			var ref string
			if rows.Scan(&ref) == nil && ref != "" {
				snapRefs = append(snapRefs, ref)
			}
		}
		rows.Close()
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete service: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE service_id = ? AND tenant_id = ?`, serviceID, tenantID); err != nil {
		return fmt.Errorf("delete service snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = ? AND tenant_id = ?`, serviceID, tenantID); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete service: %w", err)
	}
	for _, ref := range snapRefs {
		// A service restored from one of these snapshots runs from its tag,
		// which exists only locally; keep it (GC reclaims it once unused).
		var inUse int
		if err := m.db.QueryRowContext(ctx, `SELECT 1 FROM services WHERE image = ? LIMIT 1`, ref).Scan(&inUse); !errors.Is(err, sql.ErrNoRows) {
			log.Printf("services: keeping snapshot image %s for deleted service %s (in use or check failed: %v)", ref, serviceID, err)
			continue
		}
		if err := m.docker.RemoveImage(ctx, ref); err != nil {
			log.Printf("WARNING: failed to remove snapshot image %s for deleted service %s: %v", ref, serviceID, err)
		}
	}
	return nil
}

// StopAllForTenant stops and removes all containers belonging to a tenant.
// Also cleans up any split-brain orphan containers by label.
func (m *Manager) StopAllForTenant(ctx context.Context, tenantID string) {
	// First, clean up any containers with this tenant's label (catches split-brain orphans)
	if labelContainers, err := m.docker.ListContainersByLabel(ctx, "ah.tenant", tenantID); err == nil {
		for _, cid := range labelContainers {
			_ = m.docker.StopContainer(ctx, cid)
			_ = m.docker.RemoveContainer(ctx, cid)
		}
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id FROM services WHERE tenant_id = ?`, tenantID,
	)
	if err != nil {
		log.Printf("services: failed to list services for tenant %s: %v", tenantID, err)
		return
	}

	// Collect all service records before closing the rows iterator.
	// We must close the rows before calling ExecContext so that the single
	// open connection (SetMaxOpenConns(1) in tests) is not held while we
	// attempt to acquire it again, which would deadlock.
	type svcRecord struct {
		id          string
		containerID sql.NullString
	}
	var svcs []svcRecord
	for rows.Next() {
		var rec svcRecord
		if err := rows.Scan(&rec.id, &rec.containerID); err != nil {
			continue
		}
		svcs = append(svcs, rec)
	}
	rows.Close()

	for _, rec := range svcs {
		svcID := rec.id
		containerID := rec.containerID
		if containerID.Valid && containerID.String != "" {
			// "Not found" from stop/remove is treated as success: the container is
			// already gone (likely cleaned up by the label-based pre-pass above).
			stopOk := true
			if stopErr := m.docker.StopContainer(ctx, containerID.String); stopErr != nil {
				if !isNotFoundError(stopErr) {
					log.Printf("WARNING: failed to stop container %s for tenant %s: %v", containerID.String, tenantID, stopErr)
					stopOk = false
				}
			}
			if rmErr := m.docker.RemoveContainer(ctx, containerID.String); rmErr != nil {
				if !isNotFoundError(rmErr) {
					log.Printf("WARNING: failed to remove container %s for tenant %s: %v (orphan may remain)", containerID.String, tenantID, rmErr)
					stopOk = false
				}
			}
			if stopOk {
				// Clear container_id using a CAS update (WHERE container_id = ?) so we
				// do not clobber a new container_id written by a concurrent Deploy/Restart.
				now := time.Now().Unix()
				_, dbErr := m.db.ExecContext(ctx,
					`UPDATE services SET container_id = '', status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
					now, svcID, tenantID, containerID.String,
				)
				if dbErr != nil {
					log.Printf("WARNING: failed to clear container_id for service %s: %v", svcID, dbErr)
					m.updateStatusScoped(ctx, tenantID, svcID, "stopped")
				}
				if err := m.cleanupServiceIngressNetwork(ctx, tenantID, svcID, containerID.String); err != nil {
					log.Printf("services: clean ingress network after suspending service %s: %v", svcID, err)
				}
			} else {
				m.updateStatusWithErrorScoped(ctx, tenantID, svcID, "failed", "container cleanup failed during tenant suspension")
			}
		} else {
			m.updateStatusScoped(ctx, tenantID, svcID, "stopped")
		}
	}

	// Keep the shared tenant bridge: stopped environments may still reference
	// its network ID. GC removes it once no persisted workload owns it.
}

func (m *Manager) cleanupServiceIngressNetwork(ctx context.Context, tenantID, serviceID, containerID string) error {
	name := docker.ServiceIngressNetworkName(tenantID, serviceID)
	networks, err := m.docker.NetworkList(ctx)
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	var networkID string
	for _, network := range networks {
		if network.Name == name {
			networkID = network.ID
			break
		}
	}
	if networkID == "" {
		return nil
	}
	for _, endpoint := range []string{containerID, "paas-traefik"} {
		if endpoint == "" {
			continue
		}
		if err := m.docker.NetworkDisconnect(ctx, networkID, endpoint); err != nil && !isNotFoundError(err) {
			return fmt.Errorf("disconnect %s from ingress network: %w", endpoint, err)
		}
	}
	if err := m.docker.RemoveNetwork(ctx, networkID); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("remove ingress network: %w", err)
	}
	return nil
}

// Logs returns a reader for the service container logs.
func (m *Manager) Logs(ctx context.Context, tenantID, serviceID string, follow bool, tail int) (io.ReadCloser, error) {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.ContainerID == "" {
		return nil, apierr.Conflict("service has no container")
	}
	return m.docker.LogsContainer(ctx, svc.ContainerID, follow, tail)
}

// List returns all services for a tenant.
func (m *Manager) List(ctx context.Context, tenantID string) ([]*Service, error) {
	return m.ListPaginated(ctx, tenantID, 100, 0)
}

// ValidStatuses is the set of recognised service status values.
var ValidStatuses = map[string]bool{
	"created":   true,
	"running":   true,
	"stopped":   true,
	"deploying": true,
	"failed":    true,
	"error":     true,
	"crashed":   true,
}

// ListPaginated returns services for a tenant with limit and offset.
// An optional statusFilter restricts results to services whose status
// matches one of the given values. Pass nil or an empty slice to list all.
func (m *Manager) ListPaginated(ctx context.Context, tenantID string, limit, offset int, statusFilter ...string) ([]*Service, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	// Build the query dynamically to include an optional status IN clause.
	query := `SELECT ` + serviceColumns + ` FROM services WHERE tenant_id = ?`
	args := []any{tenantID}

	if len(statusFilter) > 0 {
		placeholders := make([]string, len(statusFilter))
		for i, s := range statusFilter {
			placeholders[i] = "?"
			args = append(args, s)
		}
		query += " AND status IN (" + strings.Join(placeholders, ",") + ")"
	}

	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var svcs []*Service
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		// Fall back to computed URL for backward compatibility (pre-migration rows).
		if s.URL == "" {
			s.URL = publicURL(s.ID, s.DNSLabel, m.baseDomain)
		}
		svcs = append(svcs, s)
	}
	return svcs, rows.Err()
}

// Get returns a single service by ID, scoped to tenant.
func (m *Manager) Get(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}

	if svc.ContainerID != "" {
		info, err := m.docker.InspectContainer(ctx, svc.ContainerID)
		if err == nil {
			svc.Status = info.Status
		}
	}
	// Fall back to computed URL for backward compatibility (pre-migration rows with empty url).
	if svc.URL == "" {
		svc.URL = publicURL(svc.ID, svc.DNSLabel, m.baseDomain)
	}
	return svc, nil
}

// UpdateName renames a service, updates its DNS label and Traefik route,
// and returns the updated service record. The name must be 1-128 characters
// and produce a valid DNS label when baseDomain is configured.
func (m *Manager) UpdateName(ctx context.Context, tenantID, serviceID, name string) (*Service, error) {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return nil, err
	}
	// Serialise with Deploy/Restart so the route written below matches the live container.
	unlock := m.lockService(serviceID)
	defer unlock()
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}

	// Validate and derive the new DNS label (only when baseDomain is configured).
	dnsLabel, err := m.dnsLabelFor(name)
	if err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	url := publicURL(svc.ID, dnsLabel, m.baseDomain)

	_, err = m.db.ExecContext(ctx,
		`UPDATE services SET name = ?, dns_label = ?, url = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		name, dnsLabel, url, now, serviceID, tenantID,
	)
	if err != nil {
		if cErr := dnsLabelConflict(err, dnsLabel); cErr != nil {
			return nil, cErr
		}
		return nil, fmt.Errorf("update service name: %w", err)
	}

	// Update Traefik route with the new DNS label.
	envVars, envErr := m.getEnvVars(ctx, serviceID)
	if envErr != nil {
		log.Printf("WARNING: failed to load env for port resolution on rename of %s: %v", serviceID, envErr)
	}
	if err := m.writeTraefikRoute(serviceID, tenantID, dnsLabel, m.baseDomain, resolvePort(svc.Port, envVars)); err != nil {
		log.Printf("WARNING: failed to update traefik route for service %s after rename: %v", serviceID, err)
	}

	log.Printf("AUDIT: tenant=%s renamed service=%s to %q (dns_label=%s)", tenantID, serviceID, name, dnsLabel)

	svc.Name = name
	svc.DNSLabel = dnsLabel
	svc.URL = url
	svc.UpdatedAt = now
	return svc, nil
}

// SetEnv sets or updates environment variables for a service.
func (m *Manager) SetEnv(ctx context.Context, tenantID, serviceID string, vars map[string]string) error {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}
	if err := ValidateEnvVars(vars); err != nil {
		return err
	}
	return m.setEnvVars(ctx, tenantID, serviceID, vars)
}

// GetEnv returns env var keys for a service. If reveal is true, returns decrypted values.
// Audit logs reveal operations for security monitoring.
func (m *Manager) GetEnv(ctx context.Context, tenantID, serviceID string, reveal bool) (map[string]string, error) {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return nil, err
	}
	if reveal {
		log.Printf("AUDIT: tenant=%s revealed env vars for service=%s", tenantID, serviceID)
		return m.getEnvVars(ctx, serviceID)
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT key FROM service_env WHERE service_id = ? ORDER BY key`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list env keys: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		result[key] = "********"
	}
	return result, rows.Err()
}

// DeleteEnv removes a single environment variable.
func (m *Manager) DeleteEnv(ctx context.Context, tenantID, serviceID, key string) error {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM service_env WHERE service_id = ? AND key = ?`,
		serviceID, key,
	)
	if err != nil {
		return fmt.Errorf("delete env var: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierr.NotFound("env var not found")
	}
	log.Printf("AUDIT: tenant=%s deleted env var %q for service=%s", tenantID, key, serviceID)
	return nil
}

// getOwned loads a service and verifies tenant ownership.
func (m *Manager) getOwned(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	s, err := scanService(m.db.QueryRowContext(ctx,
		`SELECT `+serviceColumns+` FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	))
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("service not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get service: %w", err)
	}
	return s, nil
}

// serviceColumns is the column list scanService expects, in order.
const serviceColumns = `id, tenant_id, name, dns_label, status, image, port, container_id, url, last_error, crash_count, circuit_open, last_crashed_at, created_at, updated_at`

// scanService scans one serviceColumns row from a *sql.Row or *sql.Rows.
func scanService(row interface{ Scan(...any) error }) (*Service, error) {
	s := &Service{}
	var containerID sql.NullString
	var circuitOpen int
	var lastCrashedAt sql.NullInt64
	if err := row.Scan(&s.ID, &s.TenantID, &s.Name, &s.DNSLabel, &s.Status, &s.Image, &s.Port, &containerID, &s.URL, &s.LastError, &s.CrashCount, &circuitOpen, &lastCrashedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.ContainerID = containerID.String
	s.CircuitOpen = circuitOpen != 0
	s.LastCrashedAt = lastCrashedAt.Int64
	return s, nil
}

func (m *Manager) updateStatusScoped(ctx context.Context, tenantID, serviceID, status string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		status, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status for service %s to %s: %v", serviceID, status, err)
	}
}

func (m *Manager) updateStatusWithErrorScoped(ctx context.Context, tenantID, serviceID, status, lastError string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		status, lastError, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status/error for service %s to %s: %v", serviceID, status, err)
	}
}

// ResetCircuitBreaker resets the circuit breaker for a service, allowing it to restart.
func (m *Manager) ResetCircuitBreaker(ctx context.Context, tenantID, serviceID string) error {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}

	// Verify existence and ownership cheaply before acquiring backpressure.
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}

	// Reset involves Docker stop/remove/inspect and must participate in global backpressure.
	release, err := m.acquireDeploySlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Per-service lock acquired AFTER global backpressure (consistent with Deploy order).
	unlock := m.lockService(serviceID)
	defer unlock()

	// Re-read service state *under the lock* to get authoritative post-lock values.
	// Avoids TOCTOU with concurrent Deploy or Restart changing ContainerID/status.
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	// Use a detached context so client disconnects do not interrupt critical cleanup.
	// context.WithoutCancel preserves request-scoped values while dropping client cancellation.
	opCtx, opCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer opCancel()

	// If a container exists, stop and remove it before resetting; the breaker
	// state is only cleared once the container is confirmed gone.
	if svc.ContainerID != "" {
		if err := m.removeContainerForReset(opCtx, svc.ContainerID); err != nil {
			return err
		}
	}

	// Fallback cleanup by deterministic container name to catch split-brain orphans
	// where DB has a stale/empty/wrong container_id (mirrors Delete() approach).
	// Fails closed unless the container is confirmed absent.
	if err := m.removeContainerByName(opCtx, docker.ServiceContainerName(tenantID, serviceID)); err != nil {
		return err
	}

	// Remove Traefik route so no traffic is forwarded to a non-existent container.
	if err := m.deleteTraefikRoute(serviceID); err != nil {
		log.Printf("WARNING: failed to delete traefik route for service %s during reset: %v", serviceID, err)
	}

	// Clear container_id so subsequent Start() calls do not try to start a
	// non-existent container. The container was stopped and removed above.
	res, err := m.db.ExecContext(opCtx,
		`UPDATE services SET crash_count = 0, circuit_open = 0, crash_window_start = NULL, status = 'stopped',
		 container_id = '', last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		time.Now().Unix(), serviceID, tenantID)
	if err != nil {
		return fmt.Errorf("reset circuit breaker: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierr.NotFound("service not found or already deleted")
	}
	log.Printf("services: circuit breaker reset for %s — container_id cleared, redeploy required", serviceID)
	return nil
}

// removeContainerForReset stops and removes containerID. "Not found" at any step
// counts as success; otherwise it fails closed whenever the container may still
// be running or cannot be confirmed removed.
func (m *Manager) removeContainerForReset(ctx context.Context, containerID string) error {
	if stopErr := m.docker.StopContainer(ctx, containerID); stopErr != nil {
		if isNotFoundError(stopErr) {
			log.Printf("services: container %s already removed during reset", cidShort(containerID))
			return nil
		}
		// Stop failed for a non-404 reason — check the container state.
		info, inspectErr := m.docker.InspectContainer(ctx, containerID)
		switch {
		case isNotFoundError(inspectErr):
			log.Printf("services: container %s disappeared during reset", cidShort(containerID))
			return nil
		case inspectErr != nil:
			return fmt.Errorf("cannot verify container state after stop failure: %v (stop error: %v)", inspectErr, stopErr)
		case info.Status == "running" || info.Status == "restarting":
			return fmt.Errorf("failed to stop container before reset: %w", stopErr)
		}
		// Container exists but is not running — safe to remove.
	}
	rmErr := m.docker.RemoveContainer(ctx, containerID)
	if rmErr == nil || isNotFoundError(rmErr) {
		return nil
	}
	_, inspErr := m.docker.InspectContainer(ctx, containerID)
	if inspErr == nil {
		return fmt.Errorf("failed to remove container during reset: %w — redeploy the service and try again", rmErr)
	}
	if !isNotFoundError(inspErr) {
		return fmt.Errorf("cannot verify container removal: %v (remove error: %v)", inspErr, rmErr)
	}
	return nil
}

func (m *Manager) setEnvVars(ctx context.Context, tenantID, serviceID string, vars map[string]string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := checkEnvVarQuota(ctx, tx, tenantID, serviceID, vars); err != nil {
		return err
	}

	if err := m.upsertEnvVarsTx(ctx, tx, serviceID, vars, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// upsertEnvVarsTx encrypts and upserts vars for serviceID inside tx.
func (m *Manager) upsertEnvVarsTx(ctx context.Context, tx *sql.Tx, serviceID string, vars map[string]string, now int64) error {
	for k, v := range vars {
		encrypted, err := crypto.Encrypt([]byte(v), m.masterKey)
		if err != nil {
			return fmt.Errorf("encrypt env var %s: %w", k, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(service_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = excluded.updated_at`,
			serviceID, k, encrypted, now, now,
		)
		if err != nil {
			return fmt.Errorf("upsert env var %s: %w", k, err)
		}
	}
	return nil
}

// checkEnvVarQuota enforces tenant_quotas.max_env_vars_per_service: the
// service's existing keys plus the new distinct keys in vars must not exceed
// the limit. Upserts of existing keys do not count. A tenant with no quota row
// is not limited here. Runs inside the caller's transaction so the count and
// the inserts are consistent.
func checkEnvVarQuota(ctx context.Context, tx *sql.Tx, tenantID, serviceID string, vars map[string]string) error {
	if len(vars) == 0 {
		return nil
	}
	var maxVars int
	err := tx.QueryRowContext(ctx,
		`SELECT max_env_vars_per_service FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxVars)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check env var quota: %w", err)
	}

	existing := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT key FROM service_env WHERE service_id = ?`, serviceID)
	if err != nil {
		return fmt.Errorf("count env vars: %w", err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		existing[k] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	total := len(existing)
	for k := range vars {
		if !existing[k] {
			total++
		}
	}
	if total > maxVars {
		return apierr.QuotaExceeded(fmt.Sprintf("quota exceeded: maximum %d env vars per service (would have %d)", maxVars, total))
	}
	return nil
}

func (m *Manager) getEnvVars(ctx context.Context, serviceID string) (map[string]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT key, value_encrypted FROM service_env WHERE service_id = ?`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query env vars: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key, encrypted string
		if err := rows.Scan(&key, &encrypted); err != nil {
			return nil, err
		}
		plaintext, err := crypto.Decrypt(encrypted, m.masterKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt env var %s: %w", key, err)
		}
		result[key] = string(plaintext)
	}
	return result, rows.Err()
}

// errDeployQueueFull is the retryable backpressure error returned when the
// global deploy queue is full (503 + Retry-After rather than 500).
func errDeployQueueFull() error {
	return apierr.Unavailable("deploy queue full; try again later", nil)
}

// errDiskLow wraps a diskcheck failure as a retryable 503.
func errDiskLow(cause error) error {
	log.Printf("services: disk check refused deploy: %v", cause)
	return apierr.Unavailable("insufficient disk space; try again later", cause)
}

// restartDeployTimeout bounds the full Deploy that Restart runs for a service
// with no container. Matches the async create-path deploy timeout.
const restartDeployTimeout = 10 * time.Minute

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
