// Package ahclient provides a typed HTTP client for the agentic-hosting API.
// It has no dependencies on internal server packages — types are defined independently.
package ahclient

// ---- Error type ----

// APIError represents a non-2xx response from the API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return "ahclient: API error " + itoa(e.StatusCode) + ": " + e.Message
}

// itoa converts an integer to string without importing strconv at the package level.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ---- Service types ----

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

// CreateServiceRequest holds parameters for creating a service.
type CreateServiceRequest struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Port  int               `json:"port,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
}

// UpdateServiceRequest holds parameters for renaming a service.
type UpdateServiceRequest struct {
	Name string `json:"name"`
}

// ---- Deployment types ----

// Deployment represents a single deployment event for a service.
type Deployment struct {
	ID           string `json:"id"`
	ServiceID    string `json:"service_id"`
	TenantID     string `json:"tenant_id"`
	BuildID      string `json:"build_id,omitempty"`
	Image        string `json:"image"`
	Status       string `json:"status"`
	Trigger      string `json:"trigger"`
	ContainerID  string `json:"container_id,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	StartedAt    int64  `json:"started_at"`
	CompletedAt  *int64 `json:"completed_at,omitempty"`
	CancelledAt  *int64 `json:"cancelled_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// ---- Environment types ----

// Environment represents a sandboxed dev environment.
type Environment struct {
	ID                   string `json:"id"`
	TenantID             string `json:"tenant_id"`
	Name                 string `json:"name"`
	TemplateID           string `json:"template_id"`
	Status               string `json:"status"`
	ContainerID          string `json:"container_id,omitempty"`
	LeaseExpiresAt       *int64 `json:"lease_expires_at,omitempty"`
	LeaseDurationSeconds int    `json:"lease_duration_seconds"`
	LastActivityAt       *int64 `json:"last_activity_at,omitempty"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

// EnvironmentTemplate describes a pre-configured environment type.
type EnvironmentTemplate struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseImage    string `json:"base_image"`
	Description  string `json:"description"`
	MemoryMB     int    `json:"memory_mb"`
	CPUMillis    int    `json:"cpu_millicores"`
	DiskMB       int    `json:"disk_mb"`
	EgressPolicy string `json:"egress_policy"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// CreateEnvironmentRequest holds parameters for creating an environment.
type CreateEnvironmentRequest struct {
	Name                 string `json:"name"`
	TemplateID           string `json:"template_id,omitempty"`
	LeaseDurationSeconds *int   `json:"lease_duration_seconds,omitempty"`
}

// ExecResult holds the result of a command execution in an environment.
type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	DurationMs int64  `json:"duration_ms"`
}

// Preview represents a preview route exposing an environment port via Traefik.
type Preview struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environment_id"`
	TenantID      string `json:"tenant_id"`
	Name          string `json:"name"`
	Port          int    `json:"port"`
	DNSLabel      string `json:"dns_label"`
	URL           string `json:"url,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// CreatePreviewRequest holds parameters for creating a preview route.
type CreatePreviewRequest struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// SyncRequest holds parameters for syncing code into an environment.
type SyncRequest struct {
	GitURL string `json:"git_url"`
	GitRef string `json:"git_ref,omitempty"`
}

// ---- Database types ----

// Database represents a provisioned database.
type Database struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	Host             string `json:"host,omitempty"`
	Port             int    `json:"port,omitempty"`
	DBName           string `json:"db_name,omitempty"`
	Username         string `json:"username,omitempty"`
	ConnectionString string `json:"connection_string,omitempty"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

// CreateDatabaseRequest holds parameters for creating a database.
type CreateDatabaseRequest struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ConnectionStringResponse holds a database connection string.
type ConnectionStringResponse struct {
	ConnectionString string `json:"connection_string"`
}

// ---- Build types ----

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
	StartedAt    *int64 `json:"started_at,omitempty"`
	FinishedAt   *int64 `json:"finished_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// StartBuildRequest holds parameters for starting a build.
type StartBuildRequest struct {
	SourceType string `json:"source_type"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref,omitempty"`
}

// StartBuildResponse is returned by the start build endpoint.
type StartBuildResponse struct {
	BuildID string `json:"build_id"`
	Status  string `json:"status"`
	Image   string `json:"image"`
}

// ---- Snapshot types ----

// Snapshot represents a point-in-time capture of a service.
type Snapshot struct {
	ID             string            `json:"id"`
	TenantID       string            `json:"tenant_id"`
	ServiceID      string            `json:"service_id"`
	Name           string            `json:"name"`
	Description    string            `json:"description,omitempty"`
	ImageRef       string            `json:"image_ref"`
	ResourceConfig string            `json:"resource_config,omitempty"`
	Port           int               `json:"port"`
	EnvVars        map[string]string `json:"env_vars,omitempty"`
	CreatedAt      int64             `json:"created_at"`
}

// CreateSnapshotRequest holds parameters for creating a snapshot.
type CreateSnapshotRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ---- Tenant types ----

// RegisterRequest holds parameters for registering a tenant.
type RegisterRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// RegisterResponse is returned after tenant registration.
type RegisterResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
}

// Tenant holds tenant information.
type Tenant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// UsageBucket holds used/max counts for a resource.
type UsageBucket struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

// TenantUsage holds tenant resource usage.
type TenantUsage struct {
	Services  UsageBucket `json:"services"`
	Databases UsageBucket `json:"databases"`
	APIKeys   UsageBucket `json:"api_keys"`
	MemoryMB  int         `json:"memory_mb"`
	CPUCores  float64     `json:"cpu_cores"`
	DiskGB    int         `json:"disk_gb"`
	RateLimit int         `json:"rate_limit"`
}

// UpdateTenantRequest holds parameters for updating a tenant.
type UpdateTenantRequest struct {
	Name *string `json:"name,omitempty"`
}

// ReactivateResponse is returned after reactivating a tenant.
type ReactivateResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
	Status   string `json:"status"`
}

// ---- API Key types ----

// CreateKeyRequest holds parameters for creating an API key.
type CreateKeyRequest struct {
	Name      string `json:"name"`
	ExpiresIn *int64 `json:"expires_in,omitempty"`
}

// CreateKeyResponse is returned after key creation.
type CreateKeyResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	Prefix  string `json:"prefix"`
	Expires *int64 `json:"expires_at,omitempty"`
}

// APIKey holds API key metadata (no secret value).
type APIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
}

// KeyRecoverRequest holds parameters for recovering an API key.
type KeyRecoverRequest struct {
	Email          string `json:"email"`
	BootstrapToken string `json:"bootstrap_token"`
}

// KeyRecoverResponse is returned after key recovery.
type KeyRecoverResponse struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	Name         string `json:"name"`
	CreatedAt    int64  `json:"created_at"`
	Warning      string `json:"warning,omitempty"`
	RevokedKeyID string `json:"revoked_key_id,omitempty"`
}

// ---- Health types ----

// HealthResponse is the minimal health check response.
type HealthResponse struct {
	Status string `json:"status"`
}

// DockerInfo holds Docker availability info.
type DockerInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

// GVisorInfo holds gVisor availability info.
type GVisorInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

// DiskInfo holds disk usage info.
type DiskInfo struct {
	TotalGB     float64 `json:"total_gb"`
	FreeGB      float64 `json:"free_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// DockerStorageInfo holds Docker object-level storage sizes.
type DockerStorageInfo struct {
	ImagesSizeBytes     int64   `json:"images_size_bytes"`
	ContainersSizeBytes int64   `json:"containers_size_bytes"`
	VolumesSizeBytes    int64   `json:"volumes_size_bytes"`
	BuildCacheSizeBytes int64   `json:"build_cache_size_bytes"`
	TotalSizeGB         float64 `json:"total_size_gb"`
}

// DetailedHealthResponse holds full system health info.
type DetailedHealthResponse struct {
	Status          string             `json:"status"`
	Docker          DockerInfo         `json:"docker"`
	GVisor          GVisorInfo         `json:"gvisor"`
	Disk            DiskInfo           `json:"disk"`
	DockerDisk      DiskInfo           `json:"docker_disk"`
	DockerStorage   *DockerStorageInfo `json:"docker_storage,omitempty"`
	TraefikNetworks *int               `json:"traefik_networks,omitempty"`
}

// ---- Activity types ----

// ActivityEvent holds a single activity event.
type ActivityEvent struct {
	ID           string `json:"id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name,omitempty"`
	Action       string `json:"action"`
	Status       string `json:"status,omitempty"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"created_at"`
	ServiceID    string `json:"service_id,omitempty"`
}

// ActivityFilter holds optional query parameters for filtering activity events.
type ActivityFilter struct {
	ResourceType string
	Action       string
	ServiceID    string
	Since        int64
	Limit        int
	Offset       int
}

// ---- Status response ----

// StatusResponse is used for simple status responses.
type StatusResponse struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}
