// Package docker wraps the Docker Engine API for ah container lifecycle.
package docker

import (
	"context"
	"fmt"
	"bytes"
	"io"
	"log"
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
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Client is the interface that callers outside this package use to interact
// with Docker. DockerClient is the production implementation.
type Client interface {
	EnsureNetwork(ctx context.Context, name string) (string, error)
	ConnectNetwork(ctx context.Context, networkID, containerID string) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string) error
	NetworkList(ctx context.Context) ([]NetworkInfo, error)
	RemoveNetwork(ctx context.Context, networkID string) error
	RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *ResourceLimits) (string, error)
	StopContainer(ctx context.Context, containerID string) error
	StartContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
	LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error)
	InspectContainer(ctx context.Context, containerID string) (*ContainerInfo, error)
	PullImage(ctx context.Context, img string) error
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
	ListVolumes(ctx context.Context, prefix string) ([]string, error)
	DiskUsage(ctx context.Context) (*StorageUsage, error)
	RunEnvironment(ctx context.Context, cfg RunEnvironmentConfig) (string, error)
	ExecCreate(ctx context.Context, containerID string, cmd []string, workDir string) (string, error)
	ExecRun(ctx context.Context, execID string, timeout time.Duration) (stdout []byte, stderr []byte, exitCode int, err error)
}

// NetworkInfo holds metadata about a Docker network.
type NetworkInfo struct {
	ID         string
	Name       string
	Containers int // number of containers connected to this network
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
	CreatedAt    time.Time
	Status       string
	StartedAt    string
	ExitCode     int
	HealthStatus string // "", "starting", "healthy", "unhealthy", "none"
}

// EnsureNetwork creates a Docker network if it doesn't exist. Returns the network ID.
func (c *DockerClient) EnsureNetwork(ctx context.Context, name string) (string, error) {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return n.ID, nil
		}
	}
	// Internal bridge with ICC disabled:
	// - Internal: no outbound internet access from containers
	// - ICC disabled: containers on the same bridge cannot communicate with each other
	// - Traefik is connected to this network for ingress routing
	resp, err := c.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
	})
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

// ConnectNetwork connects a container to an additional network.
func (c *DockerClient) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	return c.cli.NetworkConnect(ctx, networkID, containerID, nil)
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
			ID:         n.ID,
			Name:       n.Name,
			Containers: len(n.Containers),
		})
	}
	return result, nil
}

// RemoveNetwork removes a Docker network by ID or name.
func (c *DockerClient) RemoveNetwork(ctx context.Context, networkID string) error {
	return c.cli.NetworkRemove(ctx, networkID)
}

// TenantNetworkName returns the deterministic network name for a tenant.
func TenantNetworkName(tenantID string) string {
	return "ah-tenant-" + tenantID
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
//   - Internal network blocks all outbound internet access from containers.
//   - ICC=false prevents containers on the same tenant bridge from communicating.
//   - Traefik is connected to each per-tenant network for ingress routing.
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

	// Connect Traefik to the per-tenant network for ingress routing.
	// This is idempotent — Docker ignores if already connected.
	// Note: Traefik gains L2 adjacency with tenant containers, but this is
	// standard for Docker+Traefik deployments. The primary isolation boundaries
	// are: gVisor runtime (syscall interception), CapDrop ALL, no-new-privileges,
	// internal network (no outbound), ICC disabled (no inter-container comms).
	traefikID, findErr := c.findTraefikContainer(ctx)
	if findErr != nil {
		log.Printf("WARNING: Traefik container not found — service %s will not be routable: %v", serviceID, findErr)
	} else if traefikID != "" {
		if connErr := c.cli.NetworkConnect(ctx, tenantNet, traefikID, nil); connErr != nil {
			// "already connected" is expected and safe to ignore
			if !strings.Contains(connErr.Error(), "already") {
				log.Printf("WARNING: failed to connect Traefik to network %s: %v", tenantNet, connErr)
			}
		}
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
		Image:  img,
		Env:    env,
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

// RemoveContainer force-removes a container.
func (c *DockerClient) RemoveContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
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
		CreatedAt:    created,
		Status:       strings.ToLower(info.State.Status),
		StartedAt:    info.State.StartedAt,
		ExitCode:     info.State.ExitCode,
		HealthStatus: healthStatus,
	}, nil
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
	_, _ = io.Copy(io.Discard, reader)
	return nil
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

// WipeVolume overwrites all files in the named volume with zeros using a
// short-lived busybox container. This prevents a future tenant from recovering
// data after a database is deleted. The container is removed automatically
// after the wipe completes (--rm equivalent via AutoRemove).
// A best-effort operation: errors are logged but do not block volume removal.
func (c *DockerClient) WipeVolume(ctx context.Context, name string) error {
	containerCfg := &container.Config{
		Image: "busybox:latest",
		Cmd:   []string{"sh", "-c", "find /data -type f -exec sh -c 'dd if=/dev/zero of=\"$1\" bs=1M conv=notrunc 2>/dev/null; rm -f \"$1\"' _ {} \\;"},
	}
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: name,
				Target: "/data",
			},
		},
		AutoRemove: true,
	}

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("wipe volume %s: create container: %w", name, err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("wipe volume %s: start container: %w", name, err)
	}

	// Wait for the wipe container to finish
	statusCh, errCh := c.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wipe volume %s: wait: %w", name, err)
		}
	case body := <-statusCh:
		if body.Error != nil {
			return fmt.Errorf("wipe volume %s: container error: %s", name, body.Error.Message)
		}
	}

	return nil
}

// RunDatabase creates and starts a container with host port mapping and
// persistent volume. Uses gVisor (runsc) runtime. Bound to 127.0.0.1 only.
// Used for databases and other infrastructure containers (e.g. Vikunja kanban).
func (c *DockerClient) RunDatabase(ctx context.Context, cfg RunDatabaseConfig) (string, error) {
	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	portStr := fmt.Sprintf("%d/tcp", cfg.ContainerPort)

	hostCfg := &container.HostConfig{
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
	}

	labels := map[string]string{
		"ah.managed": "true",
		"ah.type":    "database",
	}
	if cfg.Labels != nil {
		labels = cfg.Labels
	}

	containerCfg := &container.Config{
		Image: cfg.Image,
		Env:   env,
		ExposedPorts: nat.PortSet{
			nat.Port(portStr): struct{}{},
		},
		Labels: labels,
	}

	if len(cfg.Cmd) > 0 {
		containerCfg.Cmd = cfg.Cmd
	}

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, cfg.Name)
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

	// Build labels; ah.* keys are set last and cannot be overridden.
	labels := make(map[string]string, len(cfg.Labels)+4)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels["ah.tenant"] = cfg.TenantID
	labels["ah.environment"] = cfg.EnvID
	labels["ah.type"] = "environment"
	labels["traefik.enable"] = "false"

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

	var stdoutBuf, stderrBuf bytes.Buffer
	// stdcopy.StdCopy demuxes the multiplexed Docker stream.
	// Use cappedWriter to truncate output at maxExecOutputBytes.
	_, err = stdcopy.StdCopy(
		&cappedWriter{buf: &stdoutBuf, max: maxExecOutputBytes},
		&cappedWriter{buf: &stderrBuf, max: maxExecOutputBytes},
		resp.Reader,
	)
	if err != nil && ctx.Err() == nil {
		return nil, nil, -1, fmt.Errorf("exec read: %w", err)
	}

	inspect, err := c.cli.ContainerExecInspect(ctx, execID)
	if err != nil {
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), -1, fmt.Errorf("exec inspect: %w", err)
	}

	return stdoutBuf.Bytes(), stderrBuf.Bytes(), inspect.ExitCode, nil
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
