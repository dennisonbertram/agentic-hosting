// Package kanbans manages per-tenant Vikunja kanban board provisioning.
package kanbans

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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

// Kanban represents a provisioned Vikunja kanban board instance.
type Kanban struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Status      string          `json:"status"`
	ContainerID string          `json:"-"`
	Host        string          `json:"host,omitempty"`
	Port        int             `json:"port,omitempty"`
	URL         string          `json:"url,omitempty"`
	Credentials *KanbanCredentials `json:"credentials,omitempty"` // only on create
	VolumeName  string          `json:"-"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
}

// KanbanCredentials holds the admin credentials returned on create.
type KanbanCredentials struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	JWT          string `json:"jwt,omitempty"`
	SetupSuccess bool   `json:"setup_success"`
}

// Manager manages kanban board lifecycle.
type Manager struct {
	db                 *sql.DB
	docker             docker.Client
	masterKey          []byte
	mu                 sync.Mutex // protects port allocation
	healthCheckTimeout time.Duration
	baseURL            string // default "http://127.0.0.1"
}

// NewManager creates a kanban manager.
func NewManager(db *sql.DB, dockerClient docker.Client, masterKey []byte) *Manager {
	if dockerClient == nil {
		panic("kanbans: NewManager requires non-nil docker client")
	}
	mgr := &Manager{
		db:                 db,
		docker:             dockerClient,
		masterKey:          masterKey,
		healthCheckTimeout: 60 * time.Second,
		baseURL:            "http://127.0.0.1",
	}
	mgr.ReconcileStale()
	return mgr
}

// SetHealthCheckTimeout overrides the default health check timeout (for testing).
func (m *Manager) SetHealthCheckTimeout(d time.Duration) {
	m.healthCheckTimeout = d
}

// SetBaseURL overrides the default base URL (for testing).
func (m *Manager) SetBaseURL(url string) {
	m.baseURL = url
}

// ReconcileStale marks kanbans stuck in "provisioning" as "failed" and
// attempts to clean up their Docker resources. Called on startup.
func (m *Manager) ReconcileStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM kanbans WHERE status = 'provisioning'`)
	if err != nil {
		log.Printf("kanbans: reconcile query failed: %v", err)
		return
	}
	defer rows.Close()

	var stale []struct{ id, containerID, volumeName string }
	for rows.Next() {
		var s struct{ id, containerID, volumeName string }
		if err := rows.Scan(&s.id, &s.containerID, &s.volumeName); err != nil {
			log.Printf("kanbans: reconcile scan failed: %v", err)
			continue
		}
		stale = append(stale, s)
	}

	for _, s := range stale {
		log.Printf("kanbans: reconciling stale kanban %s", s.id)
		if s.containerID != "" {
			_ = m.docker.StopContainer(ctx, s.containerID)
			_ = m.docker.RemoveContainer(ctx, s.containerID)
		}
		if s.volumeName != "" {
			_ = m.docker.RemoveVolume(ctx, s.volumeName)
		}
		m.updateStatus(ctx, s.id, "failed")
	}
	if len(stale) > 0 {
		log.Printf("kanbans: reconciled %d stale kanbans", len(stale))
	}
}

// Create provisions a new Vikunja kanban board for a tenant.
// It returns immediately with status "provisioning"; a background goroutine
// handles health checks, Vikunja setup, and status transitions to "ready"
// or "failed". Callers should poll GET /v1/kanban for the final status.
func (m *Manager) Create(ctx context.Context, tenantID string) (*Kanban, error) {
	// Validate tenantID is DNS-label safe (used in subdomain and email).
	// Tenant IDs are hex-encoded (generateID produces lowercase hex), but
	// defend against any future format changes.
	if !isDNSLabelSafe(tenantID) {
		return nil, apierr.Validation("tenant ID is not valid for DNS subdomain use")
	}

	// Check disk space before provisioning
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, fmt.Errorf("disk check: %w", err)
	}

	// Check if tenant already has a kanban
	var existingCount int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kanbans WHERE tenant_id = ? AND status != 'failed'`, tenantID,
	).Scan(&existingCount); err != nil {
		return nil, fmt.Errorf("check existing kanban: %w", err)
	}
	if existingCount > 0 {
		return nil, apierr.Conflict("tenant already has a kanban board")
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// Generate JWT secret and admin password
	jwtSecret, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate jwt secret: %w", err)
	}
	adminPassword, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate admin password: %w", err)
	}

	volumeName := fmt.Sprintf("ah-kanban-%s", id)
	tenantPrefix := tenantID
	if len(tenantPrefix) > 8 {
		tenantPrefix = tenantPrefix[:8]
	}
	idPrefix := id
	if len(idPrefix) > 16 {
		idPrefix = idPrefix[:16]
	}
	containerName := fmt.Sprintf("ah-kanban-%s-%s", tenantPrefix, idPrefix)
	publicURL := fmt.Sprintf("%s.kanban.agentic.hosting", tenantID)

	// Find free port and insert atomically — retry only on port UNIQUE violation
	var port int
	now := time.Now().Unix()
	const maxPortRetries = 5
	for attempt := 0; attempt < maxPortRetries; attempt++ {
		port, err = m.findFreePort(ctx)
		if err != nil {
			return nil, fmt.Errorf("find free port: %w", err)
		}

		// Insert DB record — UNIQUE index on port prevents race conditions
		_, err = m.db.ExecContext(ctx,
			`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
			 VALUES (?, ?, 'provisioning', '127.0.0.1', ?, ?, ?, ?, ?)`,
			id, tenantID, port, volumeName, publicURL, now, now,
		)
		if err != nil {
			errMsg := err.Error()
			// Only retry on port uniqueness conflicts, not tenant_id conflicts
			if strings.Contains(errMsg, "UNIQUE constraint failed") {
				if strings.Contains(errMsg, "kanbans.tenant_id") {
					return nil, apierr.Conflict("tenant already has a kanban board")
				}
				continue // port collision — retry with different port
			}
			return nil, fmt.Errorf("insert kanban: %w", err)
		}
		break // success
	}
	if err != nil {
		return nil, fmt.Errorf("insert kanban after %d retries: %w", maxPortRetries, err)
	}

	// Create Docker volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Run Vikunja container
	containerID, err := m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          containerName,
		Image:         "vikunja/vikunja:0.24.6",
		HostPort:      port,
		ContainerPort: 3456,
		Env: map[string]string{
			"VIKUNJA_SERVICE_JWTSECRET":                       jwtSecret,
			"VIKUNJA_SERVICE_FRONTENDURL":                     fmt.Sprintf("https://%s", publicURL),
			"VIKUNJA_DATABASE_TYPE":                           "sqlite",
			"VIKUNJA_DATABASE_PATH":                           "/app/vikunja/files/vikunja.db",
			// Registration must be enabled for initial admin user creation.
			// Container is only accessible on loopback (127.0.0.1 port binding)
			// so external registration is not possible.
			"VIKUNJA_SERVICE_ENABLEREGISTRATION":              "true",
			"VIKUNJA_DEFAULTSETTINGS_DISCOVERABLE_BY_NAME":   "false",
			"VIKUNJA_DEFAULTSETTINGS_DISCOVERABLE_BY_EMAIL":  "false",
			"VIKUNJA_MAILER_ENABLED":                          "false",
		},
		VolumeName: volumeName,
		MountPath:  "/app/vikunja/files",
		Labels: map[string]string{
			"ah.managed": "true",
			"ah.type":    "kanban",
		},
	})
	if err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Update container ID — check rows affected to detect concurrent Delete
	result, updateErr := m.db.ExecContext(ctx,
		`UPDATE kanbans SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, time.Now().Unix(), id,
	)
	if updateErr != nil || result == nil {
		log.Printf("kanbans: failed to update container_id for %s, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("kanban record deleted during provisioning")
	}
	if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
		log.Printf("kanbans: record %s was deleted during provisioning, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("kanban record deleted during provisioning")
	}

	// Launch background provisioning: health check, Vikunja setup, credential
	// storage, and final status transition. The API returns immediately with
	// status "provisioning" so the caller is never blocked on a 60-second
	// health check.
	go m.provision(id, tenantID, containerID, volumeName, publicURL, port, adminPassword)

	return &Kanban{
		ID:        id,
		TenantID:  tenantID,
		Status:    "provisioning",
		Host:      "127.0.0.1",
		Port:      port,
		URL:       publicURL,
		VolumeName: volumeName,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// provision runs in a background goroutine to complete kanban setup after the
// container has been started. It handles the health check, Vikunja user/project
// setup, credential encryption, and final status update. On any failure it
// marks the kanban as "failed" and cleans up Docker resources.
func (m *Manager) provision(id, tenantID, containerID, volumeName, publicURL string, port int, adminPassword string) {
	// Use a background context since the original request context is gone.
	ctx, cancel := context.WithTimeout(context.Background(), m.healthCheckTimeout+30*time.Second)
	defer cancel()

	cleanup := func() {
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
	}

	// Wait for health check (HTTP GET to /api/v1/info)
	healthy := m.waitForHTTP(ctx, port, m.healthCheckTimeout)

	if !healthy {
		log.Printf("kanbans: health check failed for %s after %s", id, m.healthCheckTimeout)
		m.updateStatus(ctx, id, "failed")
		cleanup()
		return
	}

	// Setup Vikunja (create admin user, project, buckets) — non-fatal on failure
	setupToken, setupErr := m.setupVikunja(port, tenantID, adminPassword)
	if setupErr != nil {
		log.Printf("kanbans: setup failed for %s (non-fatal): %v", id, setupErr)
	} else {
		_ = setupToken // JWT stored via admin token below; setup success logged
	}

	// Encrypt admin password for storage — failure is fatal (irrecoverable credentials)
	tokenEnc, err := crypto.Encrypt([]byte(adminPassword), m.masterKey)
	if err != nil {
		log.Printf("kanbans: failed to encrypt admin token for %s, cleaning up: %v", id, err)
		m.updateStatus(ctx, id, "failed")
		cleanup()
		return
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE kanbans SET admin_token_encrypted = ?, url = ?, updated_at = ? WHERE id = ?`,
		tokenEnc, publicURL, time.Now().Unix(), id,
	); err != nil {
		log.Printf("kanbans: failed to store admin token for %s, cleaning up: %v", id, err)
		m.updateStatus(ctx, id, "failed")
		cleanup()
		return
	}

	if !m.updateStatus(ctx, id, "ready") {
		cleanup()
		return
	}

	log.Printf("kanbans: provisioning complete for %s (tenant %s)", id, tenantID)
}

// Get returns the kanban board for a tenant.
func (m *Manager) Get(ctx context.Context, tenantID string) (*Kanban, error) {
	k := &Kanban{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, status, container_id, host, port, url,
		 volume_name, created_at, updated_at
		 FROM kanbans WHERE tenant_id = ?`, tenantID,
	).Scan(&k.ID, &k.TenantID, &k.Status, &k.ContainerID, &k.Host, &k.Port, &k.URL,
		&k.VolumeName, &k.CreatedAt, &k.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierr.NotFound("kanban board not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get kanban: %w", err)
	}
	return k, nil
}

// GetAdminToken returns the decrypted admin token.
func (m *Manager) GetAdminToken(ctx context.Context, tenantID string) (string, error) {
	var tokenEnc sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT admin_token_encrypted FROM kanbans WHERE tenant_id = ?`, tenantID,
	).Scan(&tokenEnc)
	if err == sql.ErrNoRows {
		return "", apierr.NotFound("kanban board not found")
	}
	if err != nil {
		return "", fmt.Errorf("get admin token: %w", err)
	}
	if !tokenEnc.Valid || tokenEnc.String == "" {
		return "", apierr.NotFound("admin token not available")
	}
	plaintext, err := crypto.Decrypt(tokenEnc.String, m.masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt admin token: %w", err)
	}
	return string(plaintext), nil
}

// Delete destroys a kanban board: stops container, removes volume, deletes record.
func (m *Manager) Delete(ctx context.Context, tenantID string) error {
	k, err := m.Get(ctx, tenantID)
	if err != nil {
		return err
	}

	// Stop and remove container
	if k.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, k.ContainerID); err != nil {
			log.Printf("kanbans: stop container %s: %v", k.ContainerID, err)
		}
		if err := m.docker.RemoveContainer(ctx, k.ContainerID); err != nil {
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("kanbans: remove container %s failed, keeping record: %v", k.ContainerID, err)
				return fmt.Errorf("failed to remove kanban container")
			}
		}
	}

	// Remove volume
	if k.VolumeName != "" {
		if err := m.docker.RemoveVolume(ctx, k.VolumeName); err != nil {
			if !strings.Contains(err.Error(), "no such volume") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("kanbans: remove volume %s failed, keeping record: %v", k.VolumeName, err)
				return fmt.Errorf("failed to remove kanban volume")
			}
		}
	}

	// Delete record only after Docker cleanup succeeded
	_, err = m.db.ExecContext(ctx, `DELETE FROM kanbans WHERE id = ? AND tenant_id = ?`, k.ID, tenantID)
	if err != nil {
		return fmt.Errorf("delete kanban record: %w", err)
	}

	return nil
}

// StopForTenant stops the kanban container for a tenant without deleting the
// DB record, so it can be restarted if the tenant is ever reactivated. Errors
// are logged but not returned — suspension should be best-effort.
func (m *Manager) StopForTenant(ctx context.Context, tenantID string) {
	k, err := m.Get(ctx, tenantID)
	if err != nil {
		// Not found is expected when the tenant has no kanban board.
		if strings.Contains(err.Error(), "not found") {
			return
		}
		log.Printf("kanbans: StopForTenant get failed for tenant %s: %v", tenantID, err)
		return
	}

	if k.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, k.ContainerID); err != nil {
			log.Printf("kanbans: StopForTenant stop container %s: %v", k.ContainerID, err)
		}
		if err := m.docker.RemoveContainer(ctx, k.ContainerID); err != nil {
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("kanbans: StopForTenant remove container %s: %v", k.ContainerID, err)
			}
		}
	}

	m.updateStatus(ctx, k.ID, "stopped")
	log.Printf("kanbans: StopForTenant stopped kanban board for tenant %s", tenantID)
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) bool {
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := m.db.ExecContext(freshCtx,
		`UPDATE kanbans SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("kanbans: failed to update status for %s: %v", id, err)
		return false
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("kanbans: record %s was deleted, status update skipped", id)
		return false
	}
	return true
}

func (m *Manager) findFreePort(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	const minPort, maxPort = 7100, 7500

	// Check which ports are already allocated in DB
	rows, err := m.db.QueryContext(ctx, `SELECT port FROM kanbans WHERE port IS NOT NULL AND status NOT IN ('failed')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	usedPorts := make(map[int]bool)
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, fmt.Errorf("scan port: %w", err)
		}
		usedPorts[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate ports: %w", err)
	}

	// Find first free port
	for port := minPort; port <= maxPort; port++ {
		if usedPorts[port] {
			continue
		}
		// Also check if port is actually available on the host
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", minPort, maxPort)
}

func (m *Manager) waitForHTTP(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("%s:%d/api/v1/info", m.baseURL, port)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

// setupVikunja creates admin user, gets API token, creates default project and buckets.
// Returns the API token on success.
func (m *Manager) setupVikunja(port int, tenantID, adminPassword string) (string, error) {
	base := fmt.Sprintf("%s:%d/api/v1", m.baseURL, port)
	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Register admin user
	regBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"email":    fmt.Sprintf("admin@%s.kanban.agentic.hosting", tenantID),
		"password": adminPassword,
	})
	resp, err := client.Post(base+"/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		return "", fmt.Errorf("register admin: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("register admin: status %d", resp.StatusCode)
	}

	// 2. Login to get JWT
	loginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": adminPassword,
	})
	resp, err = client.Post(base+"/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("login: status %d", resp.StatusCode)
	}
	var loginResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	token, ok := loginResp["token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no token in login response")
	}

	// 3. Create default project
	projBody, _ := json.Marshal(map[string]interface{}{
		"title": fmt.Sprintf("%s Kanban", tenantID),
	})
	req, _ := http.NewRequest("POST", base+"/projects", bytes.NewReader(projBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		return token, fmt.Errorf("create project: %w", err)
	}
	projBodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return token, fmt.Errorf("create project: status %d", resp.StatusCode)
	}
	var projResp map[string]interface{}
	if err := json.Unmarshal(projBodyBytes, &projResp); err != nil {
		return token, fmt.Errorf("decode project response: %w", err)
	}
	projectID, _ := projResp["id"].(float64)
	if projectID == 0 {
		return token, fmt.Errorf("no project ID in response")
	}

	// 4. Create default buckets
	buckets := []struct {
		Title    string
		Position int
	}{
		{"Backlog", 0},
		{"In Progress", 1},
		{"In Review", 2},
		{"Done", 3},
	}
	bucketsURL := fmt.Sprintf("%s/projects/%d/buckets", base, int(projectID))
	for _, b := range buckets {
		bucketBody, _ := json.Marshal(map[string]interface{}{
			"title":    b.Title,
			"position": b.Position,
		})
		req, _ := http.NewRequest("POST", bucketsURL, bytes.NewReader(bucketBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("kanbans: create bucket %q failed: %v", b.Title, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("kanbans: create bucket %q: status %d", b.Title, resp.StatusCode)
		}
	}

	return token, nil
}

// dnsLabelRe matches a valid DNS label: lowercase alphanumeric, hyphens allowed
// (not at start/end), max 63 chars. Tenant IDs are hex so this is always true
// in practice, but we validate defensively.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func isDNSLabelSafe(s string) bool {
	return len(s) > 0 && len(s) <= 63 && dnsLabelRe.MatchString(s)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("random hex: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
