package ahclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client is a typed HTTP client for the agentic-hosting API.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewClient creates a new Client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientNoTimeout creates a client without a request timeout.
// Use this for streaming endpoints (logs, build output).
func NewClientNoTimeout(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{},
	}
}

// ---- internal helpers ----

// doJSON sends a JSON request and decodes the response body into dest.
// For 204 No Content, dest may be nil.
func (c *Client) doJSON(method, path string, body any, dest any, extraHeaders map[string]string) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ahclient: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("ahclient: build request: %w", err)
	}

	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ahclient: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.readError(resp)
	}

	if dest == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("ahclient: decode response: %w", err)
	}
	return nil
}

// doStream sends a GET request and returns the response body as an io.ReadCloser.
// Callers are responsible for closing the returned reader.
func (c *Client) doStream(method, path string, extraHeaders map[string]string) (io.ReadCloser, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("ahclient: build request: %w", err)
	}

	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	// Use a client without timeout for streaming
	cl := c.HTTPClient
	if cl.Timeout != 0 {
		cl = &http.Client{Transport: cl.Transport}
	}

	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ahclient: do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, c.readError(resp)
	}

	return resp.Body, nil
}

// readError reads an API error response body and returns an *APIError.
func (c *Client) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp struct {
		Error string `json:"error"`
	}
	msg := string(body)
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		msg = errResp.Error
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg}
}

// ---- System endpoints ----

// Health checks system health (unauthenticated).
func (c *Client) Health() (*HealthResponse, error) {
	var resp HealthResponse
	if err := c.doJSON(http.MethodGet, "/v1/system/health", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// HealthDetailed returns detailed system health (authenticated).
// Pass fresh=true to bypass the server-side cache.
func (c *Client) HealthDetailed(fresh bool) (*DetailedHealthResponse, error) {
	path := "/v1/system/health/detailed"
	if fresh {
		path += "?fresh=true"
	}
	var resp DetailedHealthResponse
	if err := c.doJSON(http.MethodGet, path, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- Tenant endpoints ----

// Register creates a new tenant. Requires a bootstrap token.
func (c *Client) Register(name, email, bootstrapToken string) (*RegisterResponse, error) {
	headers := map[string]string{"X-Bootstrap-Token": bootstrapToken}
	body := RegisterRequest{Name: name, Email: email}
	var resp RegisterResponse
	if err := c.doJSON(http.MethodPost, "/v1/tenants/register", body, &resp, headers); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTenant returns the current tenant's details.
func (c *Client) GetTenant() (*Tenant, error) {
	var resp Tenant
	if err := c.doJSON(http.MethodGet, "/v1/tenant", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpdateTenant updates the current tenant.
func (c *Client) UpdateTenant(req UpdateTenantRequest) (*Tenant, error) {
	var resp Tenant
	if err := c.doJSON(http.MethodPatch, "/v1/tenant", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTenantUsage returns resource usage for the current tenant.
func (c *Client) GetTenantUsage() (*TenantUsage, error) {
	var resp TenantUsage
	if err := c.doJSON(http.MethodGet, "/v1/tenant/usage", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteTenant suspends the current tenant account.
func (c *Client) DeleteTenant() error {
	return c.doJSON(http.MethodDelete, "/v1/tenant", nil, nil, nil)
}

// ReactivateTenant reactivates a suspended tenant. Requires a bootstrap token.
func (c *Client) ReactivateTenant(tenantID, bootstrapToken string) (*ReactivateResponse, error) {
	headers := map[string]string{"X-Bootstrap-Token": bootstrapToken}
	var resp ReactivateResponse
	if err := c.doJSON(http.MethodPost, "/v1/tenants/"+tenantID+"/reactivate", nil, &resp, headers); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- API key endpoints ----

// ListKeys returns all non-revoked API keys for the current tenant.
func (c *Client) ListKeys() ([]APIKey, error) {
	var resp []APIKey
	if err := c.doJSON(http.MethodGet, "/v1/auth/keys", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateKey creates a new API key.
func (c *Client) CreateKey(req CreateKeyRequest) (*CreateKeyResponse, error) {
	var resp CreateKeyResponse
	if err := c.doJSON(http.MethodPost, "/v1/auth/keys", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RevokeKey revokes an API key by ID.
func (c *Client) RevokeKey(keyID string) error {
	return c.doJSON(http.MethodDelete, "/v1/auth/keys/"+keyID, nil, nil, nil)
}

// RecoverKey generates a new API key using the bootstrap token and email.
func (c *Client) RecoverKey(email, bootstrapToken string) (*KeyRecoverResponse, error) {
	body := KeyRecoverRequest{Email: email, BootstrapToken: bootstrapToken}
	var resp KeyRecoverResponse
	if err := c.doJSON(http.MethodPost, "/v1/auth/recover", body, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- Service endpoints ----

// ListServices returns all services for the current tenant.
func (c *Client) ListServices() ([]Service, error) {
	var resp []Service
	if err := c.doJSON(http.MethodGet, "/v1/services", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetService returns a service by ID.
func (c *Client) GetService(serviceID string) (*Service, error) {
	var resp Service
	if err := c.doJSON(http.MethodGet, "/v1/services/"+serviceID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateService creates a new service.
func (c *Client) CreateService(req CreateServiceRequest) (*Service, error) {
	var resp Service
	if err := c.doJSON(http.MethodPost, "/v1/services", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpdateService renames a service.
func (c *Client) UpdateService(serviceID, name string) (*Service, error) {
	body := UpdateServiceRequest{Name: name}
	var resp Service
	if err := c.doJSON(http.MethodPatch, "/v1/services/"+serviceID, body, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteService deletes a service.
func (c *Client) DeleteService(serviceID string) error {
	return c.doJSON(http.MethodDelete, "/v1/services/"+serviceID, nil, nil, nil)
}

// StartService starts a stopped service.
func (c *Client) StartService(serviceID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/start", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopService stops a running service.
func (c *Client) StopService(serviceID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/stop", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RestartService restarts a service.
func (c *Client) RestartService(serviceID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/restart", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RedeployService redeploys a service (stops and restarts with current image and env).
func (c *Client) RedeployService(serviceID string) (*Service, error) {
	var resp Service
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/redeploy", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ResetService resets the circuit breaker for a service.
func (c *Client) ResetService(serviceID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/reset", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetServiceEnv returns env vars for a service.
// Pass reveal=true to return values (audit-logged server-side).
func (c *Client) GetServiceEnv(serviceID string, reveal bool) (map[string]string, error) {
	path := "/v1/services/" + serviceID + "/env"
	if reveal {
		path += "?reveal=true"
	}
	var resp map[string]string
	if err := c.doJSON(http.MethodGet, path, nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// SetServiceEnv sets env vars for a service.
func (c *Client) SetServiceEnv(serviceID string, vars map[string]string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/env", vars, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteServiceEnv deletes an env var from a service.
func (c *Client) DeleteServiceEnv(serviceID, key string) error {
	return c.doJSON(http.MethodDelete, "/v1/services/"+serviceID+"/env/"+key, nil, nil, nil)
}

// GetServiceLogs returns service logs as a streaming reader.
// If follow is true, the connection stays open for live tailing.
func (c *Client) GetServiceLogs(serviceID string, follow bool, tail int) (io.ReadCloser, error) {
	params := url.Values{}
	params.Set("tail", strconv.Itoa(tail))
	if follow {
		params.Set("follow", "true")
	}
	path := "/v1/services/" + serviceID + "/logs?" + params.Encode()
	return c.doStream(http.MethodGet, path, nil)
}

// ListDeployments returns the deployment history for a service.
func (c *Client) ListDeployments(serviceID string) ([]Deployment, error) {
	var resp []Deployment
	if err := c.doJSON(http.MethodGet, "/v1/services/"+serviceID+"/deployments", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// CancelDeployment cancels an in-progress or queued deployment.
func (c *Client) CancelDeployment(serviceID, deploymentID string) (*Deployment, error) {
	var resp Deployment
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/deployments/"+deploymentID+"/cancel", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- Snapshot endpoints ----

// ListSnapshots returns all snapshots for the current tenant.
func (c *Client) ListSnapshots() ([]Snapshot, error) {
	var resp []Snapshot
	if err := c.doJSON(http.MethodGet, "/v1/snapshots", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetSnapshot returns a snapshot by ID.
// Pass reveal=true to include env var values (audit-logged server-side).
func (c *Client) GetSnapshot(snapshotID string, reveal bool) (*Snapshot, error) {
	path := "/v1/snapshots/" + snapshotID
	if reveal {
		path += "?reveal=true"
	}
	var resp Snapshot
	if err := c.doJSON(http.MethodGet, path, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateSnapshot creates a snapshot of a service.
func (c *Client) CreateSnapshot(serviceID string, req CreateSnapshotRequest) (*Snapshot, error) {
	var resp Snapshot
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/snapshots", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteSnapshot deletes a snapshot.
func (c *Client) DeleteSnapshot(snapshotID string) error {
	return c.doJSON(http.MethodDelete, "/v1/snapshots/"+snapshotID, nil, nil, nil)
}

// ---- Build endpoints ----

// ListBuilds returns all builds for the current tenant.
func (c *Client) ListBuilds() ([]Build, error) {
	var resp []Build
	if err := c.doJSON(http.MethodGet, "/v1/builds", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// ListBuildsForService returns all builds for a specific service.
func (c *Client) ListBuildsForService(serviceID string) ([]Build, error) {
	var resp []Build
	if err := c.doJSON(http.MethodGet, "/v1/services/"+serviceID+"/builds", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetBuild returns a build by ID.
func (c *Client) GetBuild(serviceID, buildID string) (*Build, error) {
	var resp Build
	if err := c.doJSON(http.MethodGet, "/v1/services/"+serviceID+"/builds/"+buildID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetBuildLogs returns build logs as a streaming reader.
// If follow is true, the connection stays open until the build completes.
func (c *Client) GetBuildLogs(serviceID, buildID string, follow bool) (io.ReadCloser, error) {
	path := "/v1/services/" + serviceID + "/builds/" + buildID + "/logs"
	if follow {
		path += "?follow=true"
	}
	return c.doStream(http.MethodGet, path, nil)
}

// StartBuild starts a new build for a service.
func (c *Client) StartBuild(serviceID string, req StartBuildRequest) (*StartBuildResponse, error) {
	var resp StartBuildResponse
	if err := c.doJSON(http.MethodPost, "/v1/services/"+serviceID+"/builds", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CancelBuild cancels a running or queued build.
func (c *Client) CancelBuild(serviceID, buildID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodDelete, "/v1/services/"+serviceID+"/builds/"+buildID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- Database endpoints ----

// ListDatabases returns all databases for the current tenant.
func (c *Client) ListDatabases() ([]Database, error) {
	var resp []Database
	if err := c.doJSON(http.MethodGet, "/v1/databases", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetDatabase returns a database by ID.
func (c *Client) GetDatabase(dbID string) (*Database, error) {
	var resp Database
	if err := c.doJSON(http.MethodGet, "/v1/databases/"+dbID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateDatabase creates a new database.
func (c *Client) CreateDatabase(req CreateDatabaseRequest) (*Database, error) {
	var resp Database
	if err := c.doJSON(http.MethodPost, "/v1/databases", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteDatabase deletes a database.
func (c *Client) DeleteDatabase(dbID string) error {
	return c.doJSON(http.MethodDelete, "/v1/databases/"+dbID, nil, nil, nil)
}

// GetDatabaseConnectionString returns the connection string for a database.
// The access is audit-logged server-side.
func (c *Client) GetDatabaseConnectionString(dbID string) (string, error) {
	var resp ConnectionStringResponse
	if err := c.doJSON(http.MethodGet, "/v1/databases/"+dbID+"/connection-string", nil, &resp, nil); err != nil {
		return "", err
	}
	return resp.ConnectionString, nil
}

// ---- Environment endpoints ----

// ListEnvironments returns all environments for the current tenant.
func (c *Client) ListEnvironments() ([]Environment, error) {
	var resp []Environment
	if err := c.doJSON(http.MethodGet, "/v1/environments", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetEnvironment returns an environment by ID.
func (c *Client) GetEnvironment(envID string) (*Environment, error) {
	var resp Environment
	if err := c.doJSON(http.MethodGet, "/v1/environments/"+envID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateEnvironment creates a new environment.
func (c *Client) CreateEnvironment(req CreateEnvironmentRequest) (*Environment, error) {
	var resp Environment
	if err := c.doJSON(http.MethodPost, "/v1/environments", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteEnvironment deletes an environment.
func (c *Client) DeleteEnvironment(envID string) error {
	return c.doJSON(http.MethodDelete, "/v1/environments/"+envID, nil, nil, nil)
}

// StartEnvironment starts a stopped environment.
func (c *Client) StartEnvironment(envID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/start", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopEnvironment stops a running environment.
func (c *Client) StopEnvironment(envID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/stop", nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EnvironmentExec executes a command in an environment.
func (c *Client) EnvironmentExec(envID string, command []string, workDir string, timeoutSeconds int) (*ExecResult, error) {
	body := map[string]any{
		"command": command,
	}
	if workDir != "" {
		body["work_dir"] = workDir
	}
	if timeoutSeconds > 0 {
		body["timeout"] = timeoutSeconds
	}
	var resp ExecResult
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/exec", body, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ExtendEnvironmentLease extends the lease on an environment.
func (c *Client) ExtendEnvironmentLease(envID string, durationSeconds int) (*Environment, error) {
	body := map[string]int{"duration_seconds": durationSeconds}
	var resp Environment
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/lease", body, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListEnvironmentTemplates returns all available environment templates.
func (c *Client) ListEnvironmentTemplates() ([]EnvironmentTemplate, error) {
	var resp []EnvironmentTemplate
	if err := c.doJSON(http.MethodGet, "/v1/environments/templates", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetEnvironmentTemplate returns a specific environment template.
func (c *Client) GetEnvironmentTemplate(templateID string) (*EnvironmentTemplate, error) {
	var resp EnvironmentTemplate
	if err := c.doJSON(http.MethodGet, "/v1/environments/templates/"+templateID, nil, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SyncWorkspace syncs code from a git URL into an environment.
func (c *Client) SyncWorkspace(envID string, req SyncRequest) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/sync", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreatePreview creates a preview route for an environment port.
func (c *Client) CreatePreview(envID string, req CreatePreviewRequest) (*Preview, error) {
	var resp Preview
	if err := c.doJSON(http.MethodPost, "/v1/environments/"+envID+"/previews", req, &resp, nil); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListPreviews returns all preview routes for an environment.
func (c *Client) ListPreviews(envID string) ([]Preview, error) {
	var resp []Preview
	if err := c.doJSON(http.MethodGet, "/v1/environments/"+envID+"/previews", nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// DeletePreview deletes a preview route.
func (c *Client) DeletePreview(envID, previewID string) error {
	return c.doJSON(http.MethodDelete, "/v1/environments/"+envID+"/previews/"+previewID, nil, nil, nil)
}

// ---- Activity endpoints ----

// ListActivity returns activity events, filtered by the given ActivityFilter.
func (c *Client) ListActivity(filter ActivityFilter) ([]ActivityEvent, error) {
	params := url.Values{}
	if filter.ResourceType != "" {
		params.Set("resource_type", filter.ResourceType)
	}
	if filter.Action != "" {
		params.Set("action", filter.Action)
	}
	if filter.ServiceID != "" {
		params.Set("service_id", filter.ServiceID)
	}
	if filter.Since > 0 {
		params.Set("since", strconv.FormatInt(filter.Since, 10))
	}
	if filter.Limit > 0 {
		params.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		params.Set("offset", strconv.Itoa(filter.Offset))
	}

	path := "/v1/activity"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp []ActivityEvent
	if err := c.doJSON(http.MethodGet, path, nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp, nil
}
