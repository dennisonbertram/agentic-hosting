// Package docker wraps the Docker Engine API for ah container lifecycle.
package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Client is the interface that callers outside this package use to interact
// with Docker. DockerClient is the production implementation.
type Client interface {
	EnsureNetwork(ctx context.Context, name string) (string, error)
	ConnectNetwork(ctx context.Context, networkID, containerID string) error
	RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *ResourceLimits) (string, error)
	StopContainer(ctx context.Context, containerID string) error
	StartContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
	LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error)
	InspectContainer(ctx context.Context, containerID string) (*ContainerInfo, error)
	PullImage(ctx context.Context, img string) error
	ListContainersByLabel(ctx context.Context, label, value string) ([]string, error)
	GetContainerLabels(ctx context.Context, containerID string) map[string]string
	GetContainerName(ctx context.Context, containerID string) string
	VerifyGVisorRuntime(ctx context.Context) error
	CreateVolume(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string) error
	RemoveVolumeSafe(ctx context.Context, name string) error
	RunDatabase(ctx context.Context, cfg RunDatabaseConfig) (string, error)
	StopAndRemoveByName(ctx context.Context, name string) error
	PruneDanglingImages(ctx context.Context) (int, error)
	ListVolumes(ctx context.Context, prefix string) ([]string, error)
	RunDevEnvironment(ctx context.Context, cfg RunDevEnvConfig) (string, error)
	ExecCreate(ctx context.Context, containerID string, cmd []string, tty bool) (string, error)
	ExecAttach(ctx context.Context, execID string) (io.ReadCloser, io.Writer, error)
	ExecInspect(ctx context.Context, execID string) (int, bool, error)
	CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader) error
	CopyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error)
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

	resp, err := c.cli.ContainerCreate(ctx,
		buildServiceContainerConfig(tenantID, serviceID, img, port, envVars, extraLabels),
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

	labels := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", serviceID):                      fmt.Sprintf("Host(`%s.localhost`)", serviceID),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", serviceID):               "web",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID): fmt.Sprintf("%d", port),
		"ah.tenant":  tenantID,
		"ah.service": serviceID,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}

	return &container.Config{
		Image:  img,
		Env:    env,
		Labels: labels,
	}
}

// findTraefikContainer finds the Traefik container by image or name.
func (c *DockerClient) findTraefikContainer(ctx context.Context) (string, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return "", err
	}
	for _, ctr := range containers {
		if strings.Contains(ctr.Image, "traefik") {
			return ctr.ID, nil
		}
		for _, name := range ctr.Names {
			if strings.Contains(name, "traefik") {
				return ctr.ID, nil
			}
		}
	}
	return "", fmt.Errorf("traefik container not found")
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

// RunDatabase creates and starts a database container with host port mapping
// and persistent volume. Database containers do NOT use gVisor (they need direct
// filesystem access for data storage), but are bound to 127.0.0.1 only.
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

	containerCfg := &container.Config{
		Image: cfg.Image,
		Env:   env,
		ExposedPorts: nat.PortSet{
			nat.Port(portStr): struct{}{},
		},
		Labels: map[string]string{
			"ah.managed": "true",
			"ah.type":    "database",
		},
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

// RunDevEnvConfig holds parameters for running a dev environment container.
type RunDevEnvConfig struct {
	TenantID   string
	EnvID      string
	Image      string
	VolumeName string
	Limits     *ResourceLimits
}

// RunDevEnvironment creates and starts a dev environment container with gVisor.
// Unlike services: writable filesystem, no Traefik routing, sleep infinity entrypoint,
// /workspace volume mount, higher PidsLimit (512), larger tmpfs.
func (c *DockerClient) RunDevEnvironment(ctx context.Context, cfg RunDevEnvConfig) (string, error) {
	name := fmt.Sprintf("ah-env-%s-%s", cfg.TenantID[:8], cfg.EnvID[:16])
	tenantNet := TenantNetworkName(cfg.TenantID)

	memoryBytes := int64(512 * 1024 * 1024)
	nanoCPUs := int64(1_000_000_000)
	if cfg.Limits != nil {
		if cfg.Limits.MemoryMB > 0 {
			memoryBytes = cfg.Limits.MemoryMB * 1024 * 1024
		}
		if cfg.Limits.CPUCores > 0 {
			nanoCPUs = int64(cfg.Limits.CPUCores * 1_000_000_000)
		}
	}

	hostCfg := &container.HostConfig{
		Runtime: "runsc",
		Resources: container.Resources{
			Memory:     memoryBytes,
			NanoCPUs:   nanoCPUs,
			PidsLimit:  int64Ptr(512),
			MemorySwap: memoryBytes,
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		NetworkMode:   container.NetworkMode(tenantNet),
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
		Tmpfs: map[string]string{
			"/tmp": "rw,noexec,nosuid,size=256m",
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: cfg.VolumeName,
				Target: "/workspace",
			},
		},
	}

	containerCfg := &container.Config{
		Image:      cfg.Image,
		Entrypoint: []string{"sleep", "infinity"},
		WorkingDir: "/workspace",
		Labels: map[string]string{
			"ah.managed":     "true",
			"ah.type":        "environment",
			"ah.tenant":      cfg.TenantID,
			"ah.environment": cfg.EnvID,
		},
	}

	resp, err := c.cli.ContainerCreate(ctx, containerCfg, hostCfg, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return "", fmt.Errorf("create environment container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start environment container: %w", err)
	}

	return resp.ID, nil
}

// ExecCreate creates an exec instance in a container and returns the exec ID.
func (c *DockerClient) ExecCreate(ctx context.Context, containerID string, cmd []string, tty bool) (string, error) {
	resp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          tty,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	return resp.ID, nil
}

// ExecAttach attaches to an exec instance and returns the I/O streams.
// The returned ReadCloser provides stdout/stderr; the Writer accepts stdin.
// Caller must close the ReadCloser when done.
func (c *DockerClient) ExecAttach(ctx context.Context, execID string) (io.ReadCloser, io.Writer, error) {
	resp, err := c.cli.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{
		Tty: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("exec attach: %w", err)
	}
	return io.NopCloser(resp.Reader), resp.Conn, nil
}

// ExecInspect returns the exit code and running state of an exec instance.
func (c *DockerClient) ExecInspect(ctx context.Context, execID string) (int, bool, error) {
	resp, err := c.cli.ContainerExecInspect(ctx, execID)
	if err != nil {
		return 0, false, fmt.Errorf("exec inspect: %w", err)
	}
	return resp.ExitCode, resp.Running, nil
}

// CopyToContainer copies a tar archive stream into the container at dstPath.
func (c *DockerClient) CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader) error {
	return c.cli.CopyToContainer(ctx, containerID, dstPath, content, container.CopyToContainerOptions{})
}

// CopyFromContainer returns a tar archive of the file/directory at srcPath.
func (c *DockerClient) CopyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error) {
	rc, _, err := c.cli.CopyFromContainer(ctx, containerID, srcPath)
	if err != nil {
		return nil, fmt.Errorf("copy from container: %w", err)
	}
	return rc, nil
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
