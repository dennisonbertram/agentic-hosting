// Package docker wraps the Docker Engine API for ah container lifecycle.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Client is the interface that callers outside this package use to interact
// with Docker. DockerClient is the production implementation.
type Client interface {
	EnsureNetwork(ctx context.Context, name string) (string, error)
	EnsureIngressNetwork(ctx context.Context, name string) (string, error)
	EnsureDatabaseNetwork(ctx context.Context, name string) (string, error)
	ConnectNetwork(ctx context.Context, networkID, containerID string) error
	ConnectNetworkAlias(ctx context.Context, networkID, containerID, alias string) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string) error
	NetworkList(ctx context.Context) ([]NetworkInfo, error)
	NetworkInspect(ctx context.Context, networkID string) (NetworkInfo, error)
	RemoveNetwork(ctx context.Context, networkID string) error
	RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *ResourceLimits) (string, error)
	StopContainer(ctx context.Context, containerID string) error
	StartContainer(ctx context.Context, containerID string) error
	RestartContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
	LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error)
	InspectContainer(ctx context.Context, containerID string) (*ContainerInfo, error)
	PullImage(ctx context.Context, img string) error
	ImageExists(ctx context.Context, img string) (bool, error)
	TagImage(ctx context.Context, source, target string) error
	RemoveImage(ctx context.Context, imageRef string) error
	ListContainersByLabel(ctx context.Context, label, value string) ([]string, error)
	GetContainerLabels(ctx context.Context, containerID string) map[string]string
	GetContainerName(ctx context.Context, containerID string) string
	VerifyGVisorRuntime(ctx context.Context) error
	CreateVolume(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string) error
	RemoveVolumeSafe(ctx context.Context, name string) error
	WipeVolume(ctx context.Context, name string) error
	RunDatabase(ctx context.Context, cfg RunDatabaseConfig) (string, error)
	StopAndRemoveByName(ctx context.Context, name string) error
	PruneDanglingImages(ctx context.Context) (int, error)
	PruneAnonymousVolumes(ctx context.Context) (int, error)
	ListImages(ctx context.Context) ([]ImageInfo, error)
	ListContainerImages(ctx context.Context) ([]string, error)
	ListNetworkContainers(ctx context.Context, networkName string) ([]string, error)
	ListVolumes(ctx context.Context, prefix string) ([]string, error)
	DiskUsage(ctx context.Context) (*StorageUsage, error)
	RunEnvironment(ctx context.Context, cfg RunEnvironmentConfig) (string, error)
	ExecCreate(ctx context.Context, containerID string, cmd []string, workDir string) (string, error)
	ExecRun(ctx context.Context, execID string, timeout time.Duration) (stdout []byte, stderr []byte, exitCode int, err error)
	RenameContainer(ctx context.Context, containerID, newName string) error
	CopyToContainer(ctx context.Context, containerID string, dstPath string, content io.Reader) error
	ContainerStats(ctx context.Context, containerID string) (*ContainerStats, error)
}

// NetworkInfo holds metadata about a Docker network.
type NetworkInfo struct {
	ID             string
	Name           string
	Containers     int // number of containers connected to this network
	ContainerNames []string
	Created        time.Time
}

// ImageInfo describes a local image.
type ImageInfo struct {
	ID      string
	Tags    []string
	Created time.Time
}

// StorageUsage holds aggregate Docker storage consumption broken down by type.
type StorageUsage struct {
	ImagesSize     int64 `json:"images_size_bytes"`
	ContainersSize int64 `json:"containers_size_bytes"`
	VolumesSize    int64 `json:"volumes_size_bytes"`
	BuildCacheSize int64 `json:"build_cache_size_bytes"`
}

// Compile-time check: DockerClient must satisfy Client.
var _ Client = (*DockerClient)(nil)

// DockerClient wraps the Docker Engine API client with ah-specific defaults.
type DockerClient struct {
	cli *client.Client
}

// NewClient creates a Docker API client using the default socket.
func NewClient() (*DockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerClient{cli: cli}, nil
}

// Close releases the Docker client resources.
func (c *DockerClient) Close() error {
	return c.cli.Close()
}

// ContainerInfo holds inspected container state.
type ContainerInfo struct {
	CreatedAt     time.Time
	Status        string
	StartedAt     string
	ExitCode      int
	HealthStatus  string // "", "starting", "healthy", "unhealthy", "none"
	EffectivePort int
	RestartCount  int // times Docker's restart policy has restarted this container
}

// EnsureNetwork creates a Docker network if it doesn't exist. Returns the network ID.
func (c *DockerClient) EnsureNetwork(ctx context.Context, name string) (string, error) {
	return c.ensureBridgeNetwork(ctx, name, false)
}

// EnsureIngressNetwork creates an internal bridge used only by Traefik and one
// routed workload. ICC is enabled on this dedicated pair network so the proxy
// can reach the workload; tenant and pool networks retain ICC=false.
func (c *DockerClient) EnsureIngressNetwork(ctx context.Context, name string) (string, error) {
	return c.ensureBridgeNetwork(ctx, name, true)
}

// EnsureDatabaseNetwork creates an internal bridge used only by one managed
// database and the tenant services that have its connection string. ICC is
// enabled so those workloads can reach the engine; tenant bridges stay ICC=false.
func (c *DockerClient) EnsureDatabaseNetwork(ctx context.Context, name string) (string, error) {
	return c.ensureBridgeNetwork(ctx, name, true)
}

func (c *DockerClient) ensureBridgeNetwork(ctx context.Context, name string, allowIngress bool) (string, error) {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return n.ID, nil
		}
	}
	resp, err := c.cli.NetworkCreate(ctx, name, bridgeNetworkCreateOptions(allowIngress))
	if err != nil {
		// Handle race condition: another goroutine may have created it
		if strings.Contains(err.Error(), "already exists") {
			networks2, err2 := c.cli.NetworkList(ctx, network.ListOptions{})
			if err2 != nil {
				return "", fmt.Errorf("list networks after race: %w", err2)
			}
			for _, n := range networks2 {
				if n.Name == name {
					return n.ID, nil
				}
			}
		}
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

func bridgeNetworkCreateOptions(allowIngress bool) network.CreateOptions {
	icc := "false"
	if allowIngress {
		icc = "true"
	}
	return network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": icc,
		},
	}
}

// ConnectNetwork connects a container to an additional network.
func (c *DockerClient) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	err := c.cli.NetworkConnect(ctx, networkID, containerID, nil)
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "already connected") || strings.Contains(strings.ToLower(err.Error()), "already exists")) {
		return nil
	}
	return err
}

// ConnectNetworkAlias attaches a container with a network-scoped DNS alias.
func (c *DockerClient) ConnectNetworkAlias(ctx context.Context, networkID, containerID, alias string) error {
	err := c.cli.NetworkConnect(ctx, networkID, containerID, &network.EndpointSettings{
		Aliases: []string{alias},
	})
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "already connected") || strings.Contains(strings.ToLower(err.Error()), "already exists")) {
		return nil
	}
	return err
}

// NetworkDisconnect disconnects a container from a network.
// Ignores "not connected" errors (idempotent).
func (c *DockerClient) NetworkDisconnect(ctx context.Context, networkID, containerID string) error {
	err := c.cli.NetworkDisconnect(ctx, networkID, containerID, false)
	if err != nil && strings.Contains(err.Error(), "is not connected") {
		return nil
	}
	return err
}

// NetworkList returns all Docker networks with container counts.
func (c *DockerClient) NetworkList(ctx context.Context) ([]NetworkInfo, error) {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	result := make([]NetworkInfo, 0, len(networks))
	for _, n := range networks {
		result = append(result, NetworkInfo{
			ID:             n.ID,
			Name:           n.Name,
			Containers:     len(n.Containers),
			ContainerNames: networkContainerNames(n.Containers),
			Created:        n.Created,
		})
	}
	return result, nil
}

// NetworkInspect returns authoritative endpoint membership. Docker Engine's
// network list response may omit Containers, so callers making ownership or
// cleanup decisions must inspect the individual network.
func (c *DockerClient) NetworkInspect(ctx context.Context, networkID string) (NetworkInfo, error) {
	n, err := c.cli.NetworkInspect(ctx, networkID, network.InspectOptions{})
	if err != nil {
		return NetworkInfo{}, fmt.Errorf("inspect network %s: %w", networkID, err)
	}
	return NetworkInfo{ID: n.ID, Name: n.Name, Containers: len(n.Containers), ContainerNames: networkContainerNames(n.Containers)}, nil
}

func networkContainerNames(endpoints map[string]network.EndpointResource) []string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}

// RemoveNetwork removes a Docker network by ID or name.
func (c *DockerClient) RemoveNetwork(ctx context.Context, networkID string) error {
	return c.cli.NetworkRemove(ctx, networkID)
}

// TenantNetworkName returns the deterministic network name for a tenant.
func TenantNetworkName(tenantID string) string {
	return "ah-tenant-" + tenantID
}

// ServiceIngressNetworkName returns a network dedicated to one tenant service
// and the platform proxy. Workloads do not share this network with each other.
func ServiceIngressNetworkName(tenantID, serviceID string) string {
	return "ah-ingress-" + tenantID + "-service-" + serviceID
}

// ServiceIngressAlias returns a name that resolves only on this service's
// dedicated ingress network.
func ServiceIngressAlias(tenantID, serviceID string) string {
	// Service IDs are globally unique. Omitting the tenant ID keeps the name
	// below Docker's 63-character DNS-label limit for full 32-character IDs.
	return "ah-route-service-" + serviceID
}

// EnvironmentIngressNetworkName returns a network dedicated to one environment
// with previews and the platform proxy.
func EnvironmentIngressNetworkName(tenantID, environmentID string) string {
	return "ah-ingress-" + tenantID + "-environment-" + environmentID
}

// EnvironmentIngressAlias returns a name that resolves only on this
// environment's dedicated preview ingress network.
func EnvironmentIngressAlias(tenantID, environmentID string) string {
	// Environment IDs are globally unique. Keep this network-scoped alias
	// within Docker's 63-character DNS-label limit for full 32-character IDs.
	return "ah-route-env-" + environmentID
}

// TenantIngressNetworkPrefix returns the prefix for a tenant's per-workload
// ingress networks, used during tenant cleanup.
func TenantIngressNetworkPrefix(tenantID string) string {
	return "ah-ingress-" + tenantID + "-"
}

const (
	databaseNetworkPrefix = "ah-dbnet-"
	databaseAliasPrefix   = "ah-db-"
	dockerDNSLabelLimit   = 63
)

// DatabaseNetworkName returns the per-database internal network
// (Internal=true, ICC=true). The name is kept at or under Docker's 63-character
// DNS-label limit by shortening the tenant ID first; the database ID is
// globally unique and is kept as complete as possible.
func DatabaseNetworkName(tenantID, dbID string) string {
	name := databaseNetworkPrefix + tenantID + "-" + dbID
	if len(name) <= dockerDNSLabelLimit {
		return name
	}
	maxDB := dockerDNSLabelLimit - len(databaseNetworkPrefix)
	if maxDB < 1 {
		return databaseNetworkPrefix
	}
	if len(dbID) >= maxDB {
		return databaseNetworkPrefix + dbID[:maxDB]
	}
	maxTenant := dockerDNSLabelLimit - len(databaseNetworkPrefix) - 1 - len(dbID)
	if maxTenant < 0 {
		maxTenant = 0
	}
	if len(tenantID) > maxTenant {
		tenantID = tenantID[:maxTenant]
	}
	return databaseNetworkPrefix + tenantID + "-" + dbID
}

// DatabaseAlias is the network-scoped DNS name used in connection strings.
// Database IDs are globally unique, so the tenant ID is omitted to stay under
// Docker's 63-character DNS-label limit.
func DatabaseAlias(dbID string) string {
	alias := databaseAliasPrefix + dbID
	if len(alias) > dockerDNSLabelLimit {
		return alias[:dockerDNSLabelLimit]
	}
	return alias
}

// TenantDatabaseNetworkPrefix is the prefix of a tenant's per-database networks.
func TenantDatabaseNetworkPrefix(tenantID string) string {
	name := DatabaseNetworkName(tenantID, "")
	return strings.TrimSuffix(name, "-") + "-"
}

// IsDatabaseNetwork reports whether name is a managed per-database network.
func IsDatabaseNetwork(name string) bool {
	return strings.HasPrefix(name, databaseNetworkPrefix)
}

// ResourceLimits holds per-container resource constraints from tenant quotas.
type ResourceLimits struct {
	MemoryMB int64
	CPUCores float64
}

// RunContainer creates and starts a container with gVisor runtime.
//
// Network isolation architecture:
//   - Container is placed on a per-tenant internal Docker network (Internal=true, ICC=false).
//   - A separate internal network connects only this service and Traefik for ingress.
//   - Other workloads are not attached to that service ingress network.
//   - Cross-tenant isolation: each tenant has a separate bridge network.
//   - Defense-in-depth: gVisor (runsc) runtime, CapDrop ALL, no-new-privileges,
//     ReadonlyRootfs, PidsLimit, MemorySwap=Memory (no swap).
//   - Health checks are left to the image author; ah does not inject a shell
//     probe because many minimal images do not ship curl/wget.
func (c *DockerClient) RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *ResourceLimits) (string, error) {
	name := containerName(tenantID, serviceID)

	if port <= 0 {
		port = 8000
	}

	tenantNet := TenantNetworkName(tenantID)
	ingressNet := ServiceIngressNetworkName(tenantID, serviceID)
	ingressID, err := c.EnsureIngressNetwork(ctx, ingressNet)
	if err != nil {
		return "", fmt.Errorf("ensure service ingress network: %w", err)
	}
	if err := c.ConnectNetwork(ctx, ingressID, "paas-traefik"); err != nil {
		return "", fmt.Errorf("connect Traefik to service ingress network: %w", err)
	}

	// Apply resource limits from tenant quotas, with safe defaults
	memoryBytes := int64(512 * 1024 * 1024) // default 512MB
	nanoCPUs := int64(1_000_000_000)        // default 1 CPU
	if limits != nil {
		if limits.MemoryMB > 0 {
			memoryBytes = limits.MemoryMB * 1024 * 1024
		}
		if limits.CPUCores > 0 {
			nanoCPUs = int64(limits.CPUCores * 1_000_000_000)
		}
	}

	hostCfg := &container.HostConfig{
		Runtime: "runsc",
		Resources: container.Resources{
			Memory:     memoryBytes,
			NanoCPUs:   nanoCPUs,
			PidsLimit:  int64Ptr(256),
			MemorySwap: memoryBytes, // no swap
		},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:    container.NetworkMode(tenantNet),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":     "rw,noexec,nosuid,size=64m",
			"/var/run": "rw,noexec,nosuid,size=16m",
			"/var/tmp": "rw,noexec,nosuid,size=16m",
			"/run":     "rw,noexec,nosuid,size=16m",
		},
	}

	containerCfg := buildServiceContainerConfig(tenantID, serviceID, img, port, envVars, extraLabels)
	// CRITICAL: force traefik.enable=false to prevent Traefik's Docker provider
	// from reading ANY labels from this container (including malicious image labels).
	// Routing is handled by the Traefik file provider instead.
	containerCfg.Labels["traefik.enable"] = "false"

	resp, err := c.cli.ContainerCreate(ctx,
		containerCfg,
		hostCfg,
		&network.NetworkingConfig{},
		nil,
		name,
	)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if err := c.ConnectNetworkAlias(ctx, ingressID, resp.ID, ServiceIngressAlias(tenantID, serviceID)); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("connect service to ingress network: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
}

func int64Ptr(v int64) *int64 { return &v }

func buildServiceContainerConfig(tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string) *container.Config {
	env := make([]string, 0, len(envVars))
	for k, v := range envVars {
		env = append(env, k+"="+v)
	}

	// Apply extraLabels first so that ah.* labels below cannot be overwritten.
	labels := make(map[string]string, len(extraLabels)+2)
	for k, v := range extraLabels {
		labels[k] = v
	}
	// ah.* labels are set last — they are immutable control-plane keys used
	// for cleanup/attribution and must not be overridable by callers.
	labels["ah.tenant"] = tenantID
	labels["ah.service"] = serviceID

	return &container.Config{
		Image: img,
		Env:   env,
		ExposedPorts: nat.PortSet{
			nat.Port(fmt.Sprintf("%d/tcp", port)): struct{}{},
		},
		Labels: labels,
	}
}

// findTraefikContainer finds the platform Traefik container by exact name.
// Only matches the container named "paas-traefik" to prevent tenant containers
// from being mistaken for Traefik (e.g. by deploying a traefik image).
func (c *DockerClient) findTraefikContainer(ctx context.Context) (string, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return "", err
	}
	for _, ctr := range containers {
		for _, name := range ctr.Names {
			// Docker prefixes container names with "/".
			if name == "/paas-traefik" || name == "paas-traefik" {
				return ctr.ID, nil
			}
		}
	}
	return "", fmt.Errorf("traefik container not found (expected container named paas-traefik)")
}

// StopContainer stops a running container with a 10s timeout.
func (c *DockerClient) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer starts a stopped container.
func (c *DockerClient) StartContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// RestartContainer restarts a container (stop with 10s timeout, then start).
func (c *DockerClient) RestartContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return c.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// RemoveContainer force-removes a container and its anonymous volumes.
// Named volumes (databases, environments) are not affected.
func (c *DockerClient) RemoveContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: true})
}

// LogsContainer returns a reader for container logs.
func (c *DockerClient) LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
	tailStr := "all"
	if tail > 0 {
		tailStr = fmt.Sprintf("%d", tail)
	}
	return c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tailStr,
		Timestamps: true,
	})
}

// InspectContainer returns the container's current state.
func (c *DockerClient) InspectContainer(ctx context.Context, containerID string) (*ContainerInfo, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container: %w", err)
	}
	created, _ := time.Parse(time.RFC3339Nano, info.Created)
	healthStatus := ""
	if info.State.Health != nil {
		healthStatus = info.State.Health.Status
	}
	return &ContainerInfo{
		CreatedAt:     created,
		Status:        strings.ToLower(info.State.Status),
		StartedAt:     info.State.StartedAt,
		ExitCode:      info.State.ExitCode,
		HealthStatus:  healthStatus,
		EffectivePort: configuredContainerPort(info.Config),
		RestartCount:  info.RestartCount,
	}, nil
}

// configuredContainerPort extracts only the numeric routing port from an
// inspected service container. PORT in the container config is authoritative;
// otherwise use a single exposed TCP port when unambiguous.
func configuredContainerPort(cfg *container.Config) int {
	if cfg == nil {
		return 0
	}
	port := 0
	for _, value := range cfg.Env {
		key, raw, ok := strings.Cut(value, "=")
		if key != "PORT" || !ok {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}
	if port != 0 {
		return port
	}
	for exposed := range cfg.ExposedPorts {
		if exposed.Proto() != "tcp" {
			continue
		}
		parsed, err := strconv.Atoi(exposed.Port())
		if err != nil || parsed < 1 || parsed > 65535 {
			continue
		}
		if port != 0 && port != parsed {
			return 0
		}
		port = parsed
	}
	return port
}

// PullImage pulls an image with a 5-minute timeout.
func (c *DockerClient) PullImage(ctx context.Context, img string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	reader, err := c.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer reader.Close()
	if err := consumeImagePullResponse(ctx, reader); err != nil {
		return fmt.Errorf("read image pull response: %w", err)
	}
	return nil
}

type imagePullMessage struct {
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// consumeImagePullResponse reads the Docker Engine JSON message stream. The
// daemon can report pull failures in an HTTP 200 response body, so transport
// success alone does not mean the image is available. Error text is omitted
// because daemon messages may include registry details.
func consumeImagePullResponse(ctx context.Context, stream io.Reader) error {
	decoder := json.NewDecoder(stream)
	messages := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var message imagePullMessage
		err := decoder.Decode(&message)
		if err == io.EOF {
			if messages == 0 {
				return fmt.Errorf("empty image pull response")
			}
			return nil
		}
		if err != nil {
			var syntaxErr *json.SyntaxError
			if errors.As(err, &syntaxErr) {
				return fmt.Errorf("invalid image pull response")
			}
			return fmt.Errorf("read image pull stream: %w", err)
		}
		messages++
		if message.Error != "" || message.ErrorDetail.Message != "" {
			return fmt.Errorf("Docker daemon reported an image pull error")
		}
	}
}

// ImageExists reports whether the image is present in the local Docker image store.
func (c *DockerClient) ImageExists(ctx context.Context, img string) (bool, error) {
	_, _, err := c.cli.ImageInspectWithRaw(ctx, img)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// TagImage adds a new tag to an existing image.
func (c *DockerClient) TagImage(ctx context.Context, source, target string) error {
	return c.cli.ImageTag(ctx, source, target)
}

// RemoveImage removes an image by reference. Used to clean up snapshot tags.
func (c *DockerClient) RemoveImage(ctx context.Context, imageRef string) error {
	_, err := c.cli.ImageRemove(ctx, imageRef, image.RemoveOptions{PruneChildren: false})
	return err
}

// ListContainersByLabel lists containers matching a label filter.
func (c *DockerClient) ListContainersByLabel(ctx context.Context, label, value string) ([]string, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var ids []string
	for _, ctr := range containers {
		if v, ok := ctr.Labels[label]; ok {
			if value == "" || v == value {
				ids = append(ids, ctr.ID)
			}
		}
	}
	return ids, nil
}

// GetContainerLabels returns the labels for a container, or nil on error.
func (c *DockerClient) GetContainerLabels(ctx context.Context, containerID string) map[string]string {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil
	}
	return info.Config.Labels
}

// GetContainerName returns the name of a container, or empty string on error.
func (c *DockerClient) GetContainerName(ctx context.Context, containerID string) string {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return ""
	}
	return info.Name
}

// containerName generates a deterministic container name from tenant and service IDs.
func containerName(tenantID, serviceID string) string {
	return fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
}

// VerifyGVisorRuntime checks that the Docker daemon has the gVisor (runsc) runtime available.
func (c *DockerClient) VerifyGVisorRuntime(ctx context.Context) error {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	for name := range info.Runtimes {
		if name == "runsc" {
			return nil
		}
	}
	return fmt.Errorf("gVisor runtime (runsc) not found in Docker daemon")
}

// RunDatabaseConfig holds parameters for running a database container.
type RunDatabaseConfig struct {
	Name          string
	Image         string
	HostPort      int
	ContainerPort int
	Env           map[string]string
	Cmd           []string
	VolumeName    string
	MountPath     string
	Labels        map[string]string // optional: overrides default labels when set
	User          string            // optional: container user (e.g. "0:0")
	NetworkName   string            // optional: primary network (managed DBs use ah-dbnet-*)
	NetworkAlias  string            // optional: DNS alias on NetworkName
}

// CreateVolume creates a named Docker volume.
func (c *DockerClient) CreateVolume(ctx context.Context, name string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
	})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// RemoveVolume removes a named Docker volume (force).
func (c *DockerClient) RemoveVolume(ctx context.Context, name string) error {
	return c.cli.VolumeRemove(ctx, name, true)
}

// RemoveVolumeSafe removes a named Docker volume without force.
// This will fail if the volume is still in use by a container, which is the
// desired behavior for GC — we never want to force-remove a volume that might
// be attached to a running container.
func (c *DockerClient) RemoveVolumeSafe(ctx context.Context, name string) error {
	return c.cli.VolumeRemove(ctx, name, false)
}

const wipeImage = "busybox:1.36"

// wipeScript overwrites each regular file with exactly its own size in zeros
// (conv=notrunc, input capped by head -c) and then removes it, so it can never
// grow a file or fill the shared disk.
const wipeScript = `find /data -type f -exec sh -c 'head -c "$(wc -c < "$1")" /dev/zero | dd of="$1" bs=64k conv=notrunc 2>/dev/null; rm -f "$1"' _ {} \;`

// wipeConfigs builds the container configs for WipeVolume. Root needs
// DAC_OVERRIDE/FOWNER because volume files may belong to other UIDs.
func wipeConfigs(volume string) (*container.Config, *container.HostConfig) {
	return &container.Config{
			Image:  wipeImage,
			Cmd:    []string{"sh", "-c", wipeScript},
			Labels: map[string]string{"ah.managed": "true", "ah.type": "wipe"},
		}, &container.HostConfig{
			// AutoRemove releases the volume even if this process dies mid-wipe.
			AutoRemove:  true,
			Runtime:     "runsc",
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"DAC_OVERRIDE", "FOWNER"},
			Resources: container.Resources{
				Memory:     64 * 1024 * 1024,
				MemorySwap: 64 * 1024 * 1024,
				PidsLimit:  int64Ptr(64),
			},
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data"}},
		}
}

// WipeVolume overwrites all files in the named volume with zeros (bounded to
// each file's own size) in a short-lived sandboxed busybox container, so a
// future tenant cannot recover deleted data. On ctx expiry the container is
// killed and removed.
func (c *DockerClient) WipeVolume(ctx context.Context, name string) error {
	if _, err := c.cli.ImageInspect(ctx, wipeImage); err != nil {
		if err := c.PullImage(ctx, wipeImage); err != nil {
			return fmt.Errorf("wipe volume %s: %w", name, err)
		}
	}
	containerCfg, hostCfg := wipeConfigs(name)

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("wipe volume %s: create container: %w", name, err)
	}
	// Force-remove (kills if running) on every exit path, including ctx expiry
	// (fresh context). Not-found is expected: AutoRemove usually got there first.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.cli.ContainerRemove(cctx, resp.ID, container.RemoveOptions{Force: true})
	}()

	// With AutoRemove the container vanishes on exit, so wait for "removed",
	// registered before start (as the docker CLI does for --rm); the exit code
	// is still reported.
	statusCh, errCh := c.cli.ContainerWait(ctx, resp.ID, container.WaitConditionRemoved)
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("wipe volume %s: start container: %w", name, err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wipe volume %s: wait: %w", name, err)
		}
	case body := <-statusCh:
		if body.Error != nil {
			return fmt.Errorf("wipe volume %s: container error: %s", name, body.Error.Message)
		}
		if body.StatusCode != 0 {
			return fmt.Errorf("wipe volume %s: container exited with status %d", name, body.StatusCode)
		}
	}
	return nil
}

const (
	databaseMemoryBytes = 512 * 1024 * 1024
	databasePidsLimit   = 256
)

var databaseCaps = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER"}

// databaseHostConfig builds the hardened host config for database containers.
// Host publish stays on 127.0.0.1 for control-plane health probes. Never bind
// 0.0.0.0. When NetworkName is set, that internal network is primary so the
// container is not left on Docker's default bridge.
func databaseHostConfig(cfg RunDatabaseConfig, portStr string) *container.HostConfig {
	h := &container.HostConfig{
		Runtime: "runsc",
		PortBindings: nat.PortMap{
			nat.Port(portStr): []nat.PortBinding{
				{HostIP: "127.0.0.1", HostPort: fmt.Sprintf("%d", cfg.HostPort)},
			},
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: cfg.VolumeName,
				Target: cfg.MountPath,
			},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Resources: container.Resources{
			Memory:     databaseMemoryBytes,
			MemorySwap: databaseMemoryBytes, // no swap
			PidsLimit:  int64Ptr(databasePidsLimit),
		},
		SecurityOpt: []string{"no-new-privileges"},
		// Official postgres/redis/vikunja entrypoints chown the data dir and
		// drop to an unprivileged user, so keep only the caps that needs.
		CapDrop: []string{"ALL"},
		CapAdd:  databaseCaps,
	}
	if cfg.NetworkName != "" {
		h.NetworkMode = container.NetworkMode(cfg.NetworkName)
	}
	return h
}

// pullOrUseLocal pulls img to refresh mutable tags, but tolerates a failed pull
// (registry down, rate limited) when the image is already in the local store.
func pullOrUseLocal(ctx context.Context, img string, pull func(context.Context, string) error, exists func(context.Context, string) (bool, error)) error {
	err := pull(ctx, img)
	if err == nil {
		return nil
	}
	if ok, _ := exists(ctx, img); ok {
		log.Printf("WARNING: pull of %s failed (%v); using local image", img, err)
		return nil
	}
	return err
}

// RunDatabase creates and starts a container with host port mapping and
// persistent volume. Uses gVisor (runsc) runtime. Bound to 127.0.0.1 only.
// Used for databases and other infrastructure containers (e.g. Vikunja kanban).
func (c *DockerClient) RunDatabase(ctx context.Context, cfg RunDatabaseConfig) (string, error) {
	if err := pullOrUseLocal(ctx, cfg.Image, c.PullImage, c.ImageExists); err != nil {
		return "", fmt.Errorf("pull image %s: %w", cfg.Image, err)
	}

	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	portStr := fmt.Sprintf("%d/tcp", cfg.ContainerPort)

	hostCfg := databaseHostConfig(cfg, portStr)

	labels := map[string]string{
		"ah.managed": "true",
		"ah.type":    "database",
	}
	if cfg.Labels != nil {
		labels = cfg.Labels
	}

	containerCfg := &container.Config{
		Image: cfg.Image,
		User:  cfg.User,
		Env:   env,
		ExposedPorts: nat.PortSet{
			nat.Port(portStr): struct{}{},
		},
		Labels: labels,
	}

	if len(cfg.Cmd) > 0 {
		containerCfg.Cmd = cfg.Cmd
	}

	var networking *network.NetworkingConfig
	if cfg.NetworkName != "" && cfg.NetworkAlias != "" {
		networking = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				cfg.NetworkName: {Aliases: []string{cfg.NetworkAlias}},
			},
		}
	}

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, networking, nil, cfg.Name)
	if err != nil {
		return "", fmt.Errorf("create database container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start database container: %w", err)
	}

	return resp.ID, nil
}

// StopAndRemoveByName stops and removes a container by its name.
// Returns nil if the container doesn't exist.
func (c *DockerClient) StopAndRemoveByName(ctx context.Context, name string) error {
	// Try to inspect by name
	info, err := c.cli.ContainerInspect(ctx, name)
	if err != nil {
		return err // container doesn't exist or other error
	}
	timeout := 10
	_ = c.cli.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout})
	return c.cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{Force: true})
}

// PruneDanglingImages removes dangling (untagged, unreferenced) images.
// Returns the number of images removed.
func (c *DockerClient) PruneDanglingImages(ctx context.Context) (int, error) {
	report, err := c.cli.ImagesPrune(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	if err != nil {
		return 0, fmt.Errorf("prune images: %w", err)
	}
	return len(report.ImagesDeleted), nil
}

// PruneAnonymousVolumes removes unused anonymous volumes (named volumes are kept).
func (c *DockerClient) PruneAnonymousVolumes(ctx context.Context) (int, error) {
	report, err := c.cli.VolumesPrune(ctx, filters.NewArgs())
	if err != nil {
		return 0, fmt.Errorf("prune volumes: %w", err)
	}
	return len(report.VolumesDeleted), nil
}

// ListImages returns all local images with their tags and creation time.
func (c *DockerClient) ListImages(ctx context.Context) ([]ImageInfo, error) {
	imgs, err := c.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	out := make([]ImageInfo, 0, len(imgs))
	for _, i := range imgs {
		out = append(out, ImageInfo{ID: i.ID, Tags: i.RepoTags, Created: time.Unix(i.Created, 0)})
	}
	return out, nil
}

// ListContainerImages returns the image ref and image ID of every container,
// running or stopped.
func (c *DockerClient) ListContainerImages(ctx context.Context) ([]string, error) {
	ctrs, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var out []string
	for _, ct := range ctrs {
		out = append(out, ct.Image, ct.ImageID)
	}
	return out, nil
}

// ListNetworkContainers returns the names of all containers (running or
// stopped) attached to the named network.
func (c *DockerClient) ListNetworkContainers(ctx context.Context, networkName string) ([]string, error) {
	ctrs, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("network", networkName))})
	if err != nil {
		return nil, fmt.Errorf("list containers on network %s: %w", networkName, err)
	}
	var out []string
	for _, ct := range ctrs {
		for _, n := range ct.Names {
			out = append(out, strings.TrimPrefix(n, "/"))
		}
	}
	return out, nil
}

// DiskUsage returns aggregate Docker storage consumption by category.
// Calls the Docker Engine /system/df API.
func (c *DockerClient) DiskUsage(ctx context.Context) (*StorageUsage, error) {
	du, err := c.cli.DiskUsage(ctx, types.DiskUsageOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker disk usage: %w", err)
	}

	usage := &StorageUsage{}

	for _, img := range du.Images {
		if img != nil {
			usage.ImagesSize += img.Size
		}
	}
	for _, ctr := range du.Containers {
		if ctr != nil {
			usage.ContainersSize += ctr.SizeRw
		}
	}
	for _, vol := range du.Volumes {
		if vol != nil && vol.UsageData != nil {
			usage.VolumesSize += vol.UsageData.Size
		}
	}
	for _, bc := range du.BuildCache {
		if bc != nil {
			usage.BuildCacheSize += bc.Size
		}
	}

	return usage, nil
}

// ListVolumes returns volume names matching the given prefix.
func (c *DockerClient) ListVolumes(ctx context.Context, prefix string) ([]string, error) {
	resp, err := c.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", prefix)),
	})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	var names []string
	for _, v := range resp.Volumes {
		names = append(names, v.Name)
	}
	return names, nil
}

// RunEnvironmentConfig holds parameters for running an environment container.
type RunEnvironmentConfig struct {
	TenantID    string
	EnvID       string
	Image       string
	MemoryMB    int64
	CPUMillis   int64
	VolumeName  string
	NetworkName string
	Labels      map[string]string
	EgressAllow bool
	WarmPool    bool // explicit lifecycle marker for containers created for the warm pool
}

// RunEnvironment creates and starts an environment container with gVisor runtime.
// Environments are dev workspaces: writable rootfs, sleep infinity entrypoint,
// workspace volume at /workspace.
func (c *DockerClient) RunEnvironment(ctx context.Context, cfg RunEnvironmentConfig) (string, error) {
	name := fmt.Sprintf("ah-env-%s-%s", cfg.TenantID, cfg.EnvID)

	// Pull the image if not available locally.
	if err := c.PullImage(ctx, cfg.Image); err != nil {
		return "", fmt.Errorf("pull image %s: %w", cfg.Image, err)
	}

	memoryBytes := cfg.MemoryMB * 1024 * 1024
	nanoCPUs := cfg.CPUMillis * 1_000_000

	labels := environmentContainerLabels(cfg)

	containerCfg := &container.Config{
		Image:      cfg.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: "/workspace",
		Labels:     labels,
	}

	initTrue := true
	hostCfg := &container.HostConfig{
		Runtime: "runsc",
		Init:    &initTrue,
		Resources: container.Resources{
			Memory:     memoryBytes,
			MemorySwap: memoryBytes, // no swap
			NanoCPUs:   nanoCPUs,
			PidsLimit:  int64Ptr(256),
		},
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: cfg.VolumeName,
				Target: "/workspace",
			},
		},
		Tmpfs: map[string]string{
			"/tmp":     "rw,noexec,nosuid,size=64m",
			"/var/run": "rw,noexec,nosuid,size=16m",
			"/var/tmp": "rw,noexec,nosuid,size=16m",
			"/run":     "rw,noexec,nosuid,size=16m",
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:   container.NetworkMode(cfg.NetworkName),
	}

	netCfg := &network.NetworkingConfig{}

	// Ensure the tenant network exists before creating the container.
	if _, err := c.EnsureNetwork(ctx, cfg.NetworkName); err != nil {
		return "", fmt.Errorf("ensure network %s: %w", cfg.NetworkName, err)
	}

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("create environment container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start environment container: %w", err)
	}

	return resp.ID, nil
}

// environmentContainerLabels builds container labels; the ah.* keys are set
// last and cannot be overridden by cfg.Labels. Docker labels are immutable, so
// warm-pool containers keep ah.type=warm-pool until claimed; GC and ownership
// lookups for claimed ones go through the DB. The warm-pool type comes from
// cfg.WarmPool; a literal ah.type=warm-pool in cfg.Labels is also accepted for
// internal callers, but any other ah.type value is replaced.
func environmentContainerLabels(cfg RunEnvironmentConfig) map[string]string {
	labels := make(map[string]string, len(cfg.Labels)+4)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels["ah.tenant"] = cfg.TenantID
	labels["ah.environment"] = cfg.EnvID
	if cfg.WarmPool || cfg.Labels["ah.type"] == "warm-pool" {
		labels["ah.type"] = "warm-pool"
	} else {
		labels["ah.type"] = "environment"
	}
	labels["traefik.enable"] = "false"
	return labels
}

// RenameContainer renames a container.
func (c *DockerClient) RenameContainer(ctx context.Context, containerID, newName string) error {
	return c.cli.ContainerRename(ctx, containerID, newName)
}

// ExecCreate creates an exec instance in a container. Returns the exec ID.
func (c *DockerClient) ExecCreate(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
	if workDir == "" {
		workDir = "/workspace"
	}
	execCfg := container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
		WorkingDir:   workDir,
	}
	resp, err := c.cli.ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	return resp.ID, nil
}

// maxExecOutputBytes limits stdout/stderr capture to 1MB each.
const maxExecOutputBytes = 1024 * 1024

// ExecRun attaches to an exec instance, reads stdout/stderr, and returns
// the output along with the exit code. Output is capped at 1MB per stream.
func (c *DockerClient) ExecRun(ctx context.Context, execID string, timeout time.Duration) (stdout []byte, stderr []byte, exitCode int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.cli.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, -1, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	// The hijacked connection ignores ctx, so close it when the deadline hits;
	// that unblocks StdCopy below.
	go func() {
		<-ctx.Done()
		resp.Close()
	}()

	var stdoutBuf, stderrBuf bytes.Buffer
	// stdcopy.StdCopy demuxes the multiplexed Docker stream.
	// Use cappedWriter to truncate output at maxExecOutputBytes.
	_, copyErr := stdcopy.StdCopy(
		&cappedWriter{buf: &stdoutBuf, max: maxExecOutputBytes},
		&cappedWriter{buf: &stderrBuf, max: maxExecOutputBytes},
		resp.Reader,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Return partial output; the caller derives timed_out from the error.
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), -1, fmt.Errorf("exec aborted: %w", ctxErr)
	}
	if copyErr != nil {
		return nil, nil, -1, fmt.Errorf("exec read: %w", copyErr)
	}

	inspect, err := c.cli.ContainerExecInspect(ctx, execID)
	if err != nil {
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), -1, fmt.Errorf("exec inspect: %w", err)
	}

	return stdoutBuf.Bytes(), stderrBuf.Bytes(), inspect.ExitCode, nil
}

// CopyToContainer copies a tar archive into the container at dstPath.
func (c *DockerClient) CopyToContainer(ctx context.Context, containerID string, dstPath string, content io.Reader) error {
	return c.cli.CopyToContainer(ctx, containerID, dstPath, content, container.CopyToContainerOptions{})
}

// cappedWriter writes to buf up to max bytes, silently discarding the rest.
type cappedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil // discard but report success
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	w.buf.Write(p)
	return len(p), nil
}

// drainPullStream reads Docker's JSON progress stream to the end and returns
// the first error message in it, or the read error (including a timeout).
// PullImage uses consumeImagePullResponse, which omits daemon error text;
// this variant keeps it for callers and tests that want the daemon message.
func drainPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg jsonmessage.JSONMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
	}
}
