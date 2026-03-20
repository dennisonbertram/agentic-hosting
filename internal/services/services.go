// Package services coordinates DB records with Docker containers.
package services

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
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
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
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
var localRegistryImagePattern = regexp.MustCompile(`^(?:127\.0\.0\.1|localhost):5000/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[a-zA-Z0-9._-]+)?$`)

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

// isNotFoundError returns true if the error indicates a container definitively
// does not exist (Docker 404). Transient errors return false.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "404")
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

// ValidateDNSName checks that a service name can produce a valid DNS label.
// Returns nil if OK, or an apierr.ValidationError describing the problem.
func ValidateDNSName(name string) error {
	// Replicate normalization without the truncation step to detect length issues.
	s := strings.ToLower(name)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	label := strings.Trim(b.String(), "-")
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
	s := strings.ToLower(name)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	s = strings.Trim(b.String(), "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.TrimRight(s, "-")
	return s
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

// traefikLabels returns an empty map. Routing is now handled by the Traefik file
// provider (writeTraefikRoute/deleteTraefikRoute) instead of Docker labels.
// Container-level traefik.enable=false is set in buildServiceContainerConfig to
// prevent malicious image labels from being read by Traefik's Docker provider.
func traefikLabels(serviceID, dnsLabel, baseDomain string, port int) map[string]string {
	return map[string]string{}
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

	containerName := fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
	portStr := strconv.Itoa(port)

	var content string
	if baseDomain != "" {
		// Production mode: HTTPS with Let's Encrypt
		if dnsLabel == "" {
			return nil
		}
		if !isDNSLabelSafe(dnsLabel) || !baseDomainRe.MatchString(baseDomain) {
			return nil
		}
		host := fmt.Sprintf("%s.%s", dnsLabel, baseDomain)
		content = fmt.Sprintf(`http:
  routers:
    svc-%s:
      rule: "Host(`+"`%s`"+`)"
      entryPoints:
        - websecure
      service: svc-%s
      tls:
        certResolver: letsencrypt
  services:
    svc-%s:
      loadBalancer:
        servers:
          - url: "http://%s:%s"
`, serviceID, host, serviceID, serviceID, containerName, portStr)
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
	db               *sql.DB
	docker           docker.Client
	masterKey        []byte
	baseDomain       string
	traefikConfigDir string       // Traefik file provider directory; empty disables file-based routing
	deploySem        chan struct{} // bounded deploy concurrency
	deployQueue      chan struct{} // bounded queue for waiting deploys

	// svcLocks is a 256-slot striped mutex array that serialises per-service destructive
	// lifecycle operations (Deploy, Restart, ResetCircuitBreaker). Using a fixed-size array
	// avoids unbounded map growth while providing reasonable parallelism across services.
	// The slot is chosen by hashing the serviceID modulo 256.
	svcLocks [256]sync.Mutex
}

// NewManager creates a service manager.
func NewManager(db *sql.DB, docker docker.Client, masterKey []byte, baseDomain, traefikConfigDir string) *Manager {
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
func ValidateImage(img string) error {
	if img == "" {
		return apierr.Validation("image is required")
	}
	if len(img) > 256 {
		return apierr.Validation("image reference too long")
	}

	if localRegistryImagePattern.MatchString(img) {
		return nil
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
	if err := ValidateImage(req.Image); err != nil {
		return nil, err
	}
	// Only enforce DNS-safe name when a base domain is configured — when baseDomain
	// is empty all services use UUID-based localhost URLs and the name is cosmetic.
	if m.baseDomain != "" {
		if err := ValidateDNSName(req.Name); err != nil {
			return nil, err
		}
	}

	// Validate env vars if provided
	if len(req.Env) > 0 {
		if err := ValidateEnvVars(req.Env); err != nil {
			return nil, err
		}
	}

	// Enforce tenant quota
	var maxServices int
	err := m.db.QueryRowContext(ctx,
		`SELECT max_services FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxServices)
	if err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}

	var currentCount int
	err = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM services WHERE tenant_id = ?`, tenantID,
	).Scan(&currentCount)
	if err != nil {
		return nil, fmt.Errorf("count services: %w", err)
	}
	if currentCount >= maxServices {
		return nil, apierr.QuotaExceeded(fmt.Sprintf("service limit reached (max %d)", maxServices))
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	port := req.Port
	if port <= 0 {
		port = 8000
	}
	if port < 1 || port > 65535 {
		return nil, apierr.Validation("port must be between 1 and 65535")
	}

	// Derive DNS label from service name only when baseDomain is configured.
	// When baseDomain is empty (localhost fallback), skip dns_label to avoid
	// squatting globally-unique subdomains that can't actually be routed.
	var dnsLabel string
	if m.baseDomain != "" {
		dnsLabel = toDNSLabel(req.Name)
		if !isDNSLabelSafe(dnsLabel) {
			dnsLabel = "" // fallback to UUID-based URL
		}
		if reservedDNSLabels[dnsLabel] {
			return nil, apierr.Validation(fmt.Sprintf("service name %q is reserved and cannot be used as a subdomain", dnsLabel))
		}
	}

	url := publicURL(id, dnsLabel, m.baseDomain)

	// Use a transaction so service insert + env vars are atomic.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'created', ?, ?, '', ?, ?, ?)`,
		id, tenantID, req.Name, dnsLabel, req.Image, port, url, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "dns_label") {
			return nil, apierr.Conflict("subdomain already taken: " + dnsLabel + " — choose a different service name")
		}
		return nil, fmt.Errorf("insert service: %w", err)
	}

	if len(req.Env) > 0 {
		for k, v := range req.Env {
			encrypted, encErr := crypto.Encrypt([]byte(v), m.masterKey)
			if encErr != nil {
				return nil, fmt.Errorf("encrypt env var %s: %w", k, encErr)
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(service_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = excluded.updated_at`,
				id, k, encrypted, now, now,
			)
			if err != nil {
				return nil, fmt.Errorf("insert env var %s: %w", k, err)
			}
		}
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
	// Try to enter the deploy queue; reject immediately if full (backpressure)
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "deploy queue full; try again later")
		return fmt.Errorf("deploy queue full; try again later")
	}

	// Acquire deploy slot (bounded concurrency)
	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// Serialise with Restart/ResetCircuitBreaker to prevent interleaved Docker+DB mutations.
	unlock := m.lockService(serviceID)
	defer unlock()

	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.CircuitOpen {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "circuit breaker is open")
		return fmt.Errorf("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}

	// Check disk space before deploy
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", err.Error())
		return fmt.Errorf("disk check: %w", err)
	}

	m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")

	// Ensure per-tenant network exists for isolation
	_, err = m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID))
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("network setup failed: %v", err))
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	if err := m.docker.PullImage(ctx, svc.Image); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("image pull failed: %v", err))
		return fmt.Errorf("pull image: %w", err)
	}

	// Re-verify service still exists after the slow image pull.
	// The user may have deleted the service while we were pulling.
	svc, err = m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return fmt.Errorf("service deleted during deploy")
	}

	// Remove existing container before creating new one to prevent
	// "name already in use" errors on redeploy.
	if svc.ContainerID != "" {
		log.Printf("services: removing existing container %s before redeploy of %s", cidShort(svc.ContainerID), serviceID)
		if stopErr := m.docker.StopContainer(ctx, svc.ContainerID); stopErr != nil {
			if !isNotFoundError(stopErr) && !isAlreadyStoppedError(stopErr) {
				log.Printf("services: stop error during redeploy teardown for %s: %v (proceeding)", serviceID, stopErr)
			}
		}
		if rmErr := m.docker.RemoveContainer(ctx, svc.ContainerID); rmErr != nil {
			if !isNotFoundError(rmErr) {
				// Remove failed for non-404 reason; verify the container is actually gone.
				if _, inspErr := m.docker.InspectContainer(ctx, svc.ContainerID); inspErr == nil {
					// Container still exists; if RunContainer tries to use the same name it will fail.
					// Log the situation but proceed — RunContainer will fail with "name in use" if needed.
					log.Printf("WARNING: failed to remove container %s during redeploy of %s: %v — container may still exist", cidShort(svc.ContainerID), serviceID, rmErr)
				}
			}
		}
		// Clear container_id in DB now so that if RunContainer fails below, the DB
		// does not point at a container that no longer exists.
		_, _ = m.db.ExecContext(ctx,
			`UPDATE services SET container_id = '', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
			time.Now().Unix(), serviceID, tenantID, svc.ContainerID,
		)
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("env vars load failed: %v", err))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := svc.Port
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		var parsed int
		if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}

	// Load resource limits from tenant quotas
	limits := m.getResourceLimits(ctx, tenantID)

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, svc.Image, port, envVars, traefikLabels(serviceID, svc.DNSLabel, m.baseDomain, port), limits)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("container start failed: %v", err))
		return fmt.Errorf("run container: %w", err)
	}

	now := time.Now().Unix()
	url := publicURL(serviceID, svc.DNSLabel, m.baseDomain)
	res, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = 'running', container_id = ?, url = ?, last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		containerID, url, now, serviceID, tenantID,
	)
	if err != nil {
		// Container is running but DB update failed; try to clean up
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		return fmt.Errorf("update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Service was deleted while we were deploying; clean up the container
		log.Printf("WARNING: service %s was deleted during deploy; removing orphan container %s", serviceID, containerID)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		return fmt.Errorf("service deleted during deploy")
	}

	// Write Traefik file-provider route (non-fatal on error)
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s: %v", serviceID, err)
	}

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

	if err := m.docker.StopContainer(ctx, svc.ContainerID); err != nil {
		if isNotFoundError(err) {
			// Container no longer exists; clear the stale container_id with CAS
			// and report stopped (self-heal, same pattern as Start).
			_, _ = m.db.ExecContext(ctx,
				`UPDATE services SET container_id = '', status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
				time.Now().Unix(), serviceID, tenantID, svc.ContainerID,
			)
			return nil
		}
		if isAlreadyStoppedError(err) {
			// Container is already stopped — treat as success.
			m.updateStatus(ctx, serviceID, "stopped")
			return nil
		}
		return fmt.Errorf("stop container: %w", err)
	}
	m.updateStatus(ctx, serviceID, "stopped")
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
		return apierr.Conflict("service has no container — deploy the service to start it")
	}

	if err := m.docker.StartContainer(ctx, svc.ContainerID); err != nil {
		if isNotFoundError(err) {
			// Container no longer exists; clear the stale container_id using a compare-and-swap
			// update (WHERE container_id = ?) so we only clear if nobody else has updated it.
			_, _ = m.db.ExecContext(ctx,
				`UPDATE services SET container_id = '', status = 'stopped', updated_at = ? WHERE id = ? AND tenant_id = ? AND container_id = ?`,
				time.Now().Unix(), serviceID, tenantID, svc.ContainerID,
			)
			return apierr.Conflict("container no longer exists — deploy the service to start it")
		}
		return fmt.Errorf("start container: %w", err)
	}
	m.updateStatus(ctx, serviceID, "running")
	return nil
}

// Restart recreates the service container so that updated env vars take effect.
// Docker container env is immutable after creation; a stop+start does NOT apply
// env var changes. This method stops the old container, removes it, then runs a
// fresh container using the current image and env vars from the DB.
func (m *Manager) Restart(ctx context.Context, tenantID, serviceID string) error {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}

	// Run disk-space preflight (same as Deploy) before acquiring any semaphore.
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return fmt.Errorf("disk check: %w", err)
	}

	// Verify existence and ownership cheaply before acquiring global backpressure.
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}

	// LOCK ORDER: deployQueue → deploySem → lockService (same as Deploy).
	// All three must be acquired in this order everywhere to prevent deadlocks.
	// Restart creates a container and must participate in the same backpressure.
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		return fmt.Errorf("deploy queue full; try again later")
	}
	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return ctx.Err()
	}

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
		return apierr.Conflict("service has no container — deploy the service first")
	}

	// Use a detached context for destructive operations so client disconnects do not
	// interrupt critical cleanup and DB state transitions mid-way.
	// context.WithoutCancel preserves request-scoped values while dropping client cancellation.
	opCtx, opCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer opCancel()

	// Stop and remove the existing container.
	log.Printf("services: restarting service %s — stopping container %s", serviceID, cidShort(svc.ContainerID))
	if err := m.docker.StopContainer(opCtx, svc.ContainerID); err != nil {
		if isNotFoundError(err) {
			log.Printf("services: container %s already gone during restart of %s", cidShort(svc.ContainerID), serviceID)
		} else if isAlreadyStoppedError(err) {
			// Container is already stopped — safe to proceed to remove + recreate.
			log.Printf("services: container %s already stopped during restart of %s", cidShort(svc.ContainerID), serviceID)
		} else {
			return fmt.Errorf("stop container: %w", err)
		}
	}
	if err := m.docker.RemoveContainer(opCtx, svc.ContainerID); err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	// Fallback cleanup by deterministic container name in case DB had a stale container_id.
	// On non-"not found" errors, verify the container is actually gone before proceeding.
	// If it still exists, abort to avoid orphaning a running container.
	expectedName := fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
	if cleanErr := m.docker.StopAndRemoveByName(opCtx, expectedName); cleanErr != nil {
		if !isNotFoundError(cleanErr) {
			log.Printf("services: restart cleanup by name %s failed: %v — verifying absence", expectedName, cleanErr)
			_, verErr := m.docker.InspectContainer(opCtx, expectedName)
			if verErr == nil {
				// Container still exists; abort to avoid orphaning a running container.
				return fmt.Errorf("container still exists after cleanup attempt (%s): %v — retry restart or redeploy", expectedName, cleanErr)
			}
			if !isNotFoundError(verErr) {
				// Inspect failed for a non-404 reason — cannot confirm absence; fail closed.
				return fmt.Errorf("cannot confirm container absence (inspect error=%v, remove error=%v) — retry restart or redeploy", verErr, cleanErr)
			}
			// isNotFoundError(verErr): container is definitely gone; safe to proceed.
		}
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

	port := svc.Port
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		var parsed int
		if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}

	// Ensure per-tenant network exists (mirrors Deploy behaviour; absence could fall
	// back to default networking and break tenant isolation).
	if _, err := m.docker.EnsureNetwork(opCtx, docker.TenantNetworkName(tenantID)); err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("network setup failed: %v", err))
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	// Load resource limits from tenant quotas.
	limits := m.getResourceLimits(opCtx, tenantID)

	// Run a fresh container with the current image and current env vars.
	// Skip image pull — the image is already local (same as redeploy without pull).
	containerID, err := m.docker.RunContainer(opCtx, tenantID, serviceID, svc.Image, port, envVars, traefikLabels(serviceID, svc.DNSLabel, m.baseDomain, port), limits)
	if err != nil {
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("container restart failed: %v", err))
		// Remove Traefik route since the old container is gone and new one failed to start.
		if traefikErr := m.deleteTraefikRoute(serviceID); traefikErr != nil {
			log.Printf("WARNING: failed to delete traefik route for service %s after restart failure: %v", serviceID, traefikErr)
		}
		return fmt.Errorf("run container: %w", err)
	}

	dbUpdRes, dbUpdErr := m.db.ExecContext(opCtx,
		`UPDATE services SET status = 'running', container_id = ?, last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		containerID, time.Now().Unix(), serviceID, tenantID,
	)
	if dbUpdErr != nil {
		// Container is running but DB update failed; clean up to avoid orphan.
		m.updateStatusWithErrorScoped(opCtx, tenantID, serviceID, "failed", fmt.Sprintf("DB update failed after restart: %v", dbUpdErr))
		_ = m.docker.StopContainer(opCtx, containerID)
		_ = m.docker.RemoveContainer(opCtx, containerID)
		return fmt.Errorf("update service after restart: %w", dbUpdErr)
	}
	if n, _ := dbUpdRes.RowsAffected(); n == 0 {
		log.Printf("WARNING: service %s was deleted during restart; removing orphan container %s", serviceID, containerID)
		_ = m.docker.StopContainer(opCtx, containerID)
		_ = m.docker.RemoveContainer(opCtx, containerID)
		return fmt.Errorf("service deleted during restart")
	}

	// Re-write Traefik route (non-fatal; port may have changed via PORT env var).
	if err := m.writeTraefikRoute(serviceID, tenantID, svc.DNSLabel, m.baseDomain, port); err != nil {
		log.Printf("WARNING: failed to write traefik route for service %s after restart: %v", serviceID, err)
	}

	return nil
}

// Delete stops and removes the container, then deletes the DB record.
func (m *Manager) Delete(ctx context.Context, tenantID, serviceID string) error {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	unlock := m.lockService(serviceID)
	defer unlock()

	if svc.ContainerID != "" {
		if stopErr := m.docker.StopContainer(ctx, svc.ContainerID); stopErr != nil {
			log.Printf("WARNING: failed to stop container %s for service %s: %v", svc.ContainerID, serviceID, stopErr)
		}
		if rmErr := m.docker.RemoveContainer(ctx, svc.ContainerID); rmErr != nil {
			log.Printf("WARNING: failed to remove container %s for service %s: %v (orphan container may remain)", svc.ContainerID, serviceID, rmErr)
		}
	}
	// Also try cleanup by deterministic container name to catch split-brain orphans
	// where a container exists but DB doesn't have its ID.
	expectedName := fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
	if cleanupErr := m.docker.StopAndRemoveByName(ctx, expectedName); cleanupErr != nil {
		// Not an error — container may not exist by this name
		log.Printf("services: cleanup by name %s: %v", expectedName, cleanupErr)
	}

	// Remove Traefik file-provider route (non-fatal on error)
	if err := m.deleteTraefikRoute(serviceID); err != nil {
		log.Printf("WARNING: failed to delete traefik route for service %s: %v", serviceID, err)
	}

	_, err = m.db.ExecContext(ctx, `DELETE FROM services WHERE id = ? AND tenant_id = ?`, serviceID, tenantID)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
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
	defer rows.Close()

	for rows.Next() {
		var svcID string
		var containerID sql.NullString
		if err := rows.Scan(&svcID, &containerID); err != nil {
			continue
		}
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
					m.updateStatus(ctx, svcID, "stopped")
				}
			} else {
				m.updateStatusWithError(ctx, svcID, "failed", "container cleanup failed during tenant suspension")
			}
		} else {
			m.updateStatus(ctx, svcID, "stopped")
		}
	}
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

// ListPaginated returns services for a tenant with limit and offset.
func (m *Manager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*Service, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, dns_label, status, image, port, container_id, url, last_error, crash_count, circuit_open, last_crashed_at, created_at, updated_at
		 FROM services WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var svcs []*Service
	for rows.Next() {
		s := &Service{}
		var containerID sql.NullString
		var lastCrashedAt sql.NullInt64
		var circuitOpen int
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &s.DNSLabel, &s.Status, &s.Image, &s.Port, &containerID, &s.URL, &s.LastError, &s.CrashCount, &circuitOpen, &lastCrashedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		if containerID.Valid {
			s.ContainerID = containerID.String
		}
		s.CircuitOpen = circuitOpen != 0
		if lastCrashedAt.Valid {
			s.LastCrashedAt = lastCrashedAt.Int64
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

// SetEnv sets or updates environment variables for a service.
func (m *Manager) SetEnv(ctx context.Context, tenantID, serviceID string, vars map[string]string) error {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}
	if err := ValidateEnvVars(vars); err != nil {
		return err
	}
	return m.setEnvVars(ctx, serviceID, vars)
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
	s := &Service{}
	var containerID sql.NullString
	var circuitOpenInt int
	var lastCrashedAtNull sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, dns_label, status, image, port, container_id, url, last_error, crash_count, circuit_open, last_crashed_at, created_at, updated_at
		 FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&s.ID, &s.TenantID, &s.Name, &s.DNSLabel, &s.Status, &s.Image, &s.Port, &containerID, &s.URL, &s.LastError, &s.CrashCount, &circuitOpenInt, &lastCrashedAtNull, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("service not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get service: %w", err)
	}
	if containerID.Valid {
		s.ContainerID = containerID.String
	}
	s.CircuitOpen = circuitOpenInt != 0
	if lastCrashedAtNull.Valid {
		s.LastCrashedAt = lastCrashedAtNull.Int64
	}
	return s, nil
}

func (m *Manager) updateStatus(ctx context.Context, serviceID, status string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), serviceID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status for service %s to %s: %v", serviceID, status, err)
	}
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

func (m *Manager) updateStatusWithError(ctx context.Context, serviceID, status, lastError string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, lastError, time.Now().Unix(), serviceID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status/error for service %s to %s: %v", serviceID, status, err)
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

	// LOCK ORDER: deployQueue → deploySem → lockService (same as Deploy/Restart).
	// Reset involves Docker stop/remove/inspect and must participate in global backpressure.
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		return fmt.Errorf("deploy queue full; try again later")
	}
	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return ctx.Err()
	}

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

	// If container exists, stop it before resetting.
	// Treat "not found" as success (container already gone is a valid terminal state).
	containerGone := svc.ContainerID == ""
	if !containerGone {
		if stopErr := m.docker.StopContainer(opCtx, svc.ContainerID); stopErr != nil {
			if isNotFoundError(stopErr) {
				// Container is already gone — safe to proceed
				log.Printf("services: container %s already removed during reset", cidShort(svc.ContainerID))
				containerGone = true
			} else {
				// Stop failed for non-404 reason — check container state
				info, inspectErr := m.docker.InspectContainer(opCtx, svc.ContainerID)
				if inspectErr != nil {
					if isNotFoundError(inspectErr) {
						// Container gone between stop and inspect — safe
						log.Printf("services: container %s disappeared during reset", cidShort(svc.ContainerID))
						containerGone = true
					} else {
						// Can't verify container state — fail closed
						return fmt.Errorf("cannot verify container state after stop failure: %v (stop error: %v)", inspectErr, stopErr)
					}
				} else if info.Status == "running" || info.Status == "restarting" {
					return fmt.Errorf("failed to stop container before reset: %w", stopErr)
				}
				// Container exists but not running — safe to proceed
			}
		}
		if !containerGone {
			// Remove the container; fail closed if removal fails and we cannot confirm it's gone.
			if rmErr := m.docker.RemoveContainer(opCtx, svc.ContainerID); rmErr != nil {
				if isNotFoundError(rmErr) {
					containerGone = true
				} else {
					// Verify the container is actually gone before clearing DB state.
					_, inspErr := m.docker.InspectContainer(opCtx, svc.ContainerID)
					if inspErr == nil {
						// Container still exists; do not clear breaker state.
						return fmt.Errorf("failed to remove container during reset: %w — redeploy the service and try again", rmErr)
					}
					// Inspect returned an error; treat as gone if it's a 404.
					if !isNotFoundError(inspErr) {
						return fmt.Errorf("cannot verify container removal: %v (remove error: %v)", inspErr, rmErr)
					}
					containerGone = true
				}
			} else {
				containerGone = true
			}
		}
	}

	// Fallback cleanup by deterministic container name to catch split-brain orphans
	// where DB has a stale/empty/wrong container_id (mirrors Delete() approach).
	// When we started with container_id == "" (containerGone was already true), we rely
	// solely on this name-based cleanup; treat non-"not found" errors as non-fatal but
	// verify actual absence via Inspect to avoid clearing DB state while a container runs.
	expectedName := fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
	if cleanupErr := m.docker.StopAndRemoveByName(opCtx, expectedName); cleanupErr != nil {
		if !isNotFoundError(cleanupErr) {
			log.Printf("services: reset cleanup by name %s failed: %v — verifying absence", expectedName, cleanupErr)
			// Verify the container is actually gone before clearing DB state.
			// Fail closed unless Inspect confirms 404 (container definitively gone).
			_, verErr := m.docker.InspectContainer(opCtx, expectedName)
			if verErr == nil {
				// Container still exists; fail closed.
				return fmt.Errorf("container still exists after name-based removal attempt (%s): %v — retry or redeploy", expectedName, cleanupErr)
			}
			if !isNotFoundError(verErr) {
				// Inspect failed for a non-404 reason — cannot confirm absence; fail closed.
				return fmt.Errorf("cannot confirm container absence (inspect error=%v, remove error=%v) — retry or redeploy", verErr, cleanupErr)
			}
			// isNotFoundError(verErr): container is definitely gone; proceed.
		}
	}

	if !containerGone {
		// Should not reach here given the checks above, but guard defensively.
		return fmt.Errorf("could not confirm container removal; aborting circuit breaker reset")
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

func (m *Manager) setEnvVars(ctx context.Context, serviceID string, vars map[string]string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
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
	return tx.Commit()
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

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
