package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/docker/docker/errdefs"
)

// NewNotFoundError returns an error shaped like the real Docker client's
// missing-container error: wrapped with %w around an errdefs NotFound.
func NewNotFoundError(containerID string) error {
	return fmt.Errorf("inspect container: %w", errdefs.NotFound(errors.New("No such container: "+containerID)))
}

// MockDockerClient is a test double for docker.Client.
// Set the function fields to control return values; inspect the recorded fields to verify calls.
type MockDockerClient struct {
	mu sync.Mutex // guards the *Calls recorders written by concurrent tests

	// EnsureNetwork
	EnsureNetworkFn            func(ctx context.Context, name string) (string, error)
	EnsureNetworkCalls         []string // network names passed
	EnsureIngressNetworkFn     func(ctx context.Context, name string) (string, error)
	EnsureIngressNetworkCalls  []string // dedicated routed-workload network names
	EnsureDatabaseNetworkFn    func(ctx context.Context, name string) (string, error)
	EnsureDatabaseNetworkCalls []string // per-database ICC=true network names

	// ConnectNetwork
	ConnectNetworkFn         func(ctx context.Context, networkID, containerID string) error
	ConnectNetworkCalls      [][2]string // [networkID, containerID] pairs
	ConnectNetworkAliasFn    func(ctx context.Context, networkID, containerID, alias string) error
	ConnectNetworkAliasCalls [][3]string // [networkID, containerID, alias]

	// NetworkDisconnect
	NetworkDisconnectFn    func(ctx context.Context, networkID, containerID string) error
	NetworkDisconnectCalls [][2]string // [networkID, containerID] pairs

	// NetworkList
	NetworkListFn       func(ctx context.Context) ([]docker.NetworkInfo, error)
	NetworkListCalls    int
	NetworkInspectFn    func(ctx context.Context, networkID string) (docker.NetworkInfo, error)
	NetworkInspectCalls []string

	// RemoveNetwork
	RemoveNetworkFn    func(ctx context.Context, networkID string) error
	RemoveNetworkCalls []string // network IDs removed

	// RunContainer
	RunContainerFn    func(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *docker.ResourceLimits) (string, error)
	RunContainerCalls int

	// StopContainer
	StopContainerFn    func(ctx context.Context, containerID string) error
	StopContainerCalls []string // containerIDs stopped

	// StartContainer
	StartContainerFn    func(ctx context.Context, containerID string) error
	StartContainerCalls []string // containerIDs started

	// RestartContainer
	RestartContainerFn    func(ctx context.Context, containerID string) error
	RestartContainerCalls []string // containerIDs restarted

	// RemoveContainer
	RemoveContainerFn    func(ctx context.Context, containerID string) error
	RemoveContainerCalls []string // containerIDs removed

	// LogsContainer
	LogsContainerFn    func(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error)
	LogsContainerCalls []string // containerIDs whose logs were requested

	// InspectContainer
	InspectContainerFn    func(ctx context.Context, containerID string) (*docker.ContainerInfo, error)
	InspectContainerCalls []string // containerIDs inspected

	// PullImage
	PullImageFn    func(ctx context.Context, img string) error
	PullImageCalls []string // images pulled

	// ImageExists (default: false, so deploys pull)
	ImageExistsFn func(ctx context.Context, img string) (bool, error)

	// ImageSize (default: 0 bytes)
	ImageSizeFn func(ctx context.Context, img string) (int64, error)

	// TagImage
	TagImageFn    func(ctx context.Context, source, target string) error
	TagImageCalls [][2]string // [source, target] pairs

	// RemoveImage
	RemoveImageFn    func(ctx context.Context, imageRef string) error
	RemoveImageCalls []string

	// ListContainersByLabel
	ListContainersByLabelFn    func(ctx context.Context, label, value string) ([]string, error)
	ListContainersByLabelCalls int

	// GetContainerLabels
	GetContainerLabelsFn    func(ctx context.Context, containerID string) map[string]string
	GetContainerLabelsCalls []string // containerIDs queried

	// GetContainerName
	GetContainerNameFn    func(ctx context.Context, containerID string) string
	GetContainerNameCalls []string // containerIDs queried

	// VerifyGVisorRuntime
	VerifyGVisorRuntimeFn    func(ctx context.Context) error
	VerifyGVisorRuntimeCalls int

	// CreateVolume
	CreateVolumeFn    func(ctx context.Context, name string) error
	CreateVolumeCalls []string // volume names created

	// RemoveVolume
	RemoveVolumeFn    func(ctx context.Context, name string) error
	RemoveVolumeCalls []string // volume names removed

	// RemoveVolumeSafe
	RemoveVolumeSafeFn    func(ctx context.Context, name string) error
	RemoveVolumeSafeCalls []string // volume names removed

	// WipeVolume
	WipeVolumeFn    func(ctx context.Context, name string) error
	WipeVolumeCalls []string // volume names wiped

	// RunDatabase
	RunDatabaseFn      func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error)
	RunDatabaseCalls   int
	RunDatabaseConfigs []docker.RunDatabaseConfig

	// StopAndRemoveByName
	StopAndRemoveByNameFn    func(ctx context.Context, name string) error
	StopAndRemoveByNameCalls []string // container names

	// PruneDanglingImages
	PruneDanglingImagesFn    func(ctx context.Context) (int, error)
	PruneDanglingImagesCalls int

	PruneAnonymousVolumesFn    func(ctx context.Context) (int, error)
	PruneAnonymousVolumesCalls int
	ListImagesFn               func(ctx context.Context) ([]docker.ImageInfo, error)
	ListContainerImagesFn      func(ctx context.Context) ([]string, error)
	// ListNetworkContainersFn returns the names of containers on a network.
	ListNetworkContainersFn func(ctx context.Context, networkName string) ([]string, error)

	// ListVolumes
	ListVolumesFn    func(ctx context.Context, prefix string) ([]string, error)
	ListVolumesCalls []string // prefixes queried

	// DiskUsage
	DiskUsageFn    func(ctx context.Context) (*docker.StorageUsage, error)
	DiskUsageCalls int

	// RunEnvironment
	RunEnvironmentFn    func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error)
	RunEnvironmentCalls int
	// RunEnvironmentConfigs records every config passed, in call order.
	RunEnvironmentConfigs []docker.RunEnvironmentConfig

	// ExecCreate
	ExecCreateFn    func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error)
	ExecCreateCalls []string // containerIDs

	// ExecRun
	ExecRunFn    func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error)
	ExecRunCalls []string // exec IDs

	// RenameContainer
	RenameContainerFn    func(ctx context.Context, containerID, newName string) error
	RenameContainerCalls int
	RenameContainerArgs  [][2]string // [containerID, newName]

	// CopyToContainer
	CopyToContainerFn    func(ctx context.Context, containerID string, dstPath string, content io.Reader) error
	CopyToContainerCalls []string // containerIDs

	// ContainerStats
	ContainerStatsFn func(ctx context.Context, containerID string) (*docker.ContainerStats, error)
}

// Ensure MockDockerClient satisfies the docker.Client interface at compile time.
var _ docker.Client = (*MockDockerClient)(nil)

func (m *MockDockerClient) EnsureNetwork(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	m.EnsureNetworkCalls = append(m.EnsureNetworkCalls, name)
	m.mu.Unlock()
	if m.EnsureNetworkFn != nil {
		return m.EnsureNetworkFn(ctx, name)
	}
	return "net-" + name, nil
}

func (m *MockDockerClient) EnsureIngressNetwork(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	m.EnsureIngressNetworkCalls = append(m.EnsureIngressNetworkCalls, name)
	m.mu.Unlock()
	if m.EnsureIngressNetworkFn != nil {
		return m.EnsureIngressNetworkFn(ctx, name)
	}
	return "ingress-" + name, nil
}

func (m *MockDockerClient) EnsureDatabaseNetwork(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	m.EnsureDatabaseNetworkCalls = append(m.EnsureDatabaseNetworkCalls, name)
	m.mu.Unlock()
	if m.EnsureDatabaseNetworkFn != nil {
		return m.EnsureDatabaseNetworkFn(ctx, name)
	}
	return "dbnet-" + name, nil
}

func (m *MockDockerClient) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	m.mu.Lock()
	m.ConnectNetworkCalls = append(m.ConnectNetworkCalls, [2]string{networkID, containerID})
	m.mu.Unlock()
	if m.ConnectNetworkFn != nil {
		return m.ConnectNetworkFn(ctx, networkID, containerID)
	}
	return nil
}

func (m *MockDockerClient) ConnectNetworkAlias(ctx context.Context, networkID, containerID, alias string) error {
	m.mu.Lock()
	m.ConnectNetworkAliasCalls = append(m.ConnectNetworkAliasCalls, [3]string{networkID, containerID, alias})
	m.mu.Unlock()
	if m.ConnectNetworkAliasFn != nil {
		return m.ConnectNetworkAliasFn(ctx, networkID, containerID, alias)
	}
	return nil
}

func (m *MockDockerClient) NetworkDisconnect(ctx context.Context, networkID, containerID string) error {
	m.mu.Lock()
	m.NetworkDisconnectCalls = append(m.NetworkDisconnectCalls, [2]string{networkID, containerID})
	m.mu.Unlock()
	if m.NetworkDisconnectFn != nil {
		return m.NetworkDisconnectFn(ctx, networkID, containerID)
	}
	return nil
}

func (m *MockDockerClient) NetworkList(ctx context.Context) ([]docker.NetworkInfo, error) {
	m.mu.Lock()
	m.NetworkListCalls++
	m.mu.Unlock()
	if m.NetworkListFn != nil {
		return m.NetworkListFn(ctx)
	}
	return nil, nil
}

func (m *MockDockerClient) NetworkInspect(ctx context.Context, networkID string) (docker.NetworkInfo, error) {
	m.mu.Lock()
	m.NetworkInspectCalls = append(m.NetworkInspectCalls, networkID)
	m.mu.Unlock()
	if m.NetworkInspectFn != nil {
		return m.NetworkInspectFn(ctx, networkID)
	}
	if m.NetworkListFn != nil {
		networks, err := m.NetworkListFn(ctx)
		if err != nil {
			return docker.NetworkInfo{}, err
		}
		for _, network := range networks {
			if network.ID == networkID {
				return network, nil
			}
		}
	}
	return docker.NetworkInfo{}, nil
}

func (m *MockDockerClient) RemoveNetwork(ctx context.Context, networkID string) error {
	m.mu.Lock()
	m.RemoveNetworkCalls = append(m.RemoveNetworkCalls, networkID)
	m.mu.Unlock()
	if m.RemoveNetworkFn != nil {
		return m.RemoveNetworkFn(ctx, networkID)
	}
	return nil
}

func (m *MockDockerClient) RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *docker.ResourceLimits) (string, error) {
	m.mu.Lock()
	m.RunContainerCalls++
	m.mu.Unlock()
	if m.RunContainerFn != nil {
		return m.RunContainerFn(ctx, tenantID, serviceID, img, port, envVars, extraLabels, limits)
	}
	return "mock-container-" + serviceID, nil
}

func (m *MockDockerClient) StopContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	m.StopContainerCalls = append(m.StopContainerCalls, containerID)
	m.mu.Unlock()
	if m.StopContainerFn != nil {
		return m.StopContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) StartContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	m.StartContainerCalls = append(m.StartContainerCalls, containerID)
	m.mu.Unlock()
	if m.StartContainerFn != nil {
		return m.StartContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) RestartContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	m.RestartContainerCalls = append(m.RestartContainerCalls, containerID)
	m.mu.Unlock()
	if m.RestartContainerFn != nil {
		return m.RestartContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) RemoveContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	m.RemoveContainerCalls = append(m.RemoveContainerCalls, containerID)
	m.mu.Unlock()
	if m.RemoveContainerFn != nil {
		return m.RemoveContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
	m.mu.Lock()
	m.LogsContainerCalls = append(m.LogsContainerCalls, containerID)
	m.mu.Unlock()
	if m.LogsContainerFn != nil {
		return m.LogsContainerFn(ctx, containerID, follow, tail)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *MockDockerClient) InspectContainer(ctx context.Context, containerID string) (*docker.ContainerInfo, error) {
	m.mu.Lock()
	m.InspectContainerCalls = append(m.InspectContainerCalls, containerID)
	m.mu.Unlock()
	if m.InspectContainerFn != nil {
		return m.InspectContainerFn(ctx, containerID)
	}
	return &docker.ContainerInfo{Status: "running"}, nil
}

func (m *MockDockerClient) PullImage(ctx context.Context, img string) error {
	m.mu.Lock()
	m.PullImageCalls = append(m.PullImageCalls, img)
	m.mu.Unlock()
	if m.PullImageFn != nil {
		return m.PullImageFn(ctx, img)
	}
	return nil
}

func (m *MockDockerClient) ImageExists(ctx context.Context, img string) (bool, error) {
	if m.ImageExistsFn != nil {
		return m.ImageExistsFn(ctx, img)
	}
	return false, nil
}

func (m *MockDockerClient) ImageSize(ctx context.Context, img string) (int64, error) {
	if m.ImageSizeFn != nil {
		return m.ImageSizeFn(ctx, img)
	}
	return 0, nil
}

func (m *MockDockerClient) TagImage(ctx context.Context, source, target string) error {
	m.mu.Lock()
	m.TagImageCalls = append(m.TagImageCalls, [2]string{source, target})
	m.mu.Unlock()
	if m.TagImageFn != nil {
		return m.TagImageFn(ctx, source, target)
	}
	return nil
}

func (m *MockDockerClient) RemoveImage(ctx context.Context, imageRef string) error {
	m.mu.Lock()
	m.RemoveImageCalls = append(m.RemoveImageCalls, imageRef)
	m.mu.Unlock()
	if m.RemoveImageFn != nil {
		return m.RemoveImageFn(ctx, imageRef)
	}
	return nil
}

func (m *MockDockerClient) ListContainersByLabel(ctx context.Context, label, value string) ([]string, error) {
	m.mu.Lock()
	m.ListContainersByLabelCalls++
	m.mu.Unlock()
	if m.ListContainersByLabelFn != nil {
		return m.ListContainersByLabelFn(ctx, label, value)
	}
	return nil, nil
}

func (m *MockDockerClient) GetContainerLabels(ctx context.Context, containerID string) map[string]string {
	m.mu.Lock()
	m.GetContainerLabelsCalls = append(m.GetContainerLabelsCalls, containerID)
	m.mu.Unlock()
	if m.GetContainerLabelsFn != nil {
		return m.GetContainerLabelsFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) GetContainerName(ctx context.Context, containerID string) string {
	m.mu.Lock()
	m.GetContainerNameCalls = append(m.GetContainerNameCalls, containerID)
	m.mu.Unlock()
	if m.GetContainerNameFn != nil {
		return m.GetContainerNameFn(ctx, containerID)
	}
	return ""
}

func (m *MockDockerClient) VerifyGVisorRuntime(ctx context.Context) error {
	m.mu.Lock()
	m.VerifyGVisorRuntimeCalls++
	m.mu.Unlock()
	if m.VerifyGVisorRuntimeFn != nil {
		return m.VerifyGVisorRuntimeFn(ctx)
	}
	return nil
}

func (m *MockDockerClient) CreateVolume(ctx context.Context, name string) error {
	m.mu.Lock()
	m.CreateVolumeCalls = append(m.CreateVolumeCalls, name)
	m.mu.Unlock()
	if m.CreateVolumeFn != nil {
		return m.CreateVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RemoveVolume(ctx context.Context, name string) error {
	m.mu.Lock()
	m.RemoveVolumeCalls = append(m.RemoveVolumeCalls, name)
	m.mu.Unlock()
	if m.RemoveVolumeFn != nil {
		return m.RemoveVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RemoveVolumeSafe(ctx context.Context, name string) error {
	m.mu.Lock()
	m.RemoveVolumeSafeCalls = append(m.RemoveVolumeSafeCalls, name)
	m.mu.Unlock()
	if m.RemoveVolumeSafeFn != nil {
		return m.RemoveVolumeSafeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) WipeVolume(ctx context.Context, name string) error {
	m.mu.Lock()
	m.WipeVolumeCalls = append(m.WipeVolumeCalls, name)
	m.mu.Unlock()
	if m.WipeVolumeFn != nil {
		return m.WipeVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RunDatabase(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
	m.mu.Lock()
	m.RunDatabaseCalls++
	m.RunDatabaseConfigs = append(m.RunDatabaseConfigs, cfg)
	m.mu.Unlock()
	if m.RunDatabaseFn != nil {
		return m.RunDatabaseFn(ctx, cfg)
	}
	return "mock-db-container-" + cfg.Name, nil
}

func (m *MockDockerClient) StopAndRemoveByName(ctx context.Context, name string) error {
	m.mu.Lock()
	m.StopAndRemoveByNameCalls = append(m.StopAndRemoveByNameCalls, name)
	m.mu.Unlock()
	if m.StopAndRemoveByNameFn != nil {
		return m.StopAndRemoveByNameFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) PruneDanglingImages(ctx context.Context) (int, error) {
	m.mu.Lock()
	m.PruneDanglingImagesCalls++
	m.mu.Unlock()
	if m.PruneDanglingImagesFn != nil {
		return m.PruneDanglingImagesFn(ctx)
	}
	return 0, nil
}

func (m *MockDockerClient) PruneAnonymousVolumes(ctx context.Context) (int, error) {
	m.mu.Lock()
	m.PruneAnonymousVolumesCalls++
	m.mu.Unlock()
	if m.PruneAnonymousVolumesFn != nil {
		return m.PruneAnonymousVolumesFn(ctx)
	}
	return 0, nil
}

func (m *MockDockerClient) ListImages(ctx context.Context) ([]docker.ImageInfo, error) {
	if m.ListImagesFn != nil {
		return m.ListImagesFn(ctx)
	}
	return nil, nil
}

func (m *MockDockerClient) ListContainerImages(ctx context.Context) ([]string, error) {
	if m.ListContainerImagesFn != nil {
		return m.ListContainerImagesFn(ctx)
	}
	return nil, nil
}

func (m *MockDockerClient) ListNetworkContainers(ctx context.Context, networkName string) ([]string, error) {
	if m.ListNetworkContainersFn != nil {
		return m.ListNetworkContainersFn(ctx, networkName)
	}
	return nil, nil
}

func (m *MockDockerClient) ListVolumes(ctx context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	m.ListVolumesCalls = append(m.ListVolumesCalls, prefix)
	m.mu.Unlock()
	if m.ListVolumesFn != nil {
		return m.ListVolumesFn(ctx, prefix)
	}
	return nil, nil
}

func (m *MockDockerClient) DiskUsage(ctx context.Context) (*docker.StorageUsage, error) {
	m.mu.Lock()
	m.DiskUsageCalls++
	m.mu.Unlock()
	if m.DiskUsageFn != nil {
		return m.DiskUsageFn(ctx)
	}
	return &docker.StorageUsage{}, nil
}

func (m *MockDockerClient) RunEnvironment(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
	m.mu.Lock()
	m.RunEnvironmentCalls++
	m.RunEnvironmentConfigs = append(m.RunEnvironmentConfigs, cfg)
	m.mu.Unlock()
	if m.RunEnvironmentFn != nil {
		return m.RunEnvironmentFn(ctx, cfg)
	}
	return "mock-env-container-" + cfg.EnvID, nil
}

func (m *MockDockerClient) ExecCreate(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
	m.mu.Lock()
	m.ExecCreateCalls = append(m.ExecCreateCalls, containerID)
	m.mu.Unlock()
	if m.ExecCreateFn != nil {
		return m.ExecCreateFn(ctx, containerID, cmd, workDir)
	}
	return "mock-exec-id", nil
}

func (m *MockDockerClient) ExecRun(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
	m.mu.Lock()
	m.ExecRunCalls = append(m.ExecRunCalls, execID)
	m.mu.Unlock()
	if m.ExecRunFn != nil {
		return m.ExecRunFn(ctx, execID, timeout)
	}
	return nil, nil, 0, nil
}

func (m *MockDockerClient) RenameContainer(ctx context.Context, containerID, newName string) error {
	m.mu.Lock()
	m.RenameContainerCalls++
	m.RenameContainerArgs = append(m.RenameContainerArgs, [2]string{containerID, newName})
	m.mu.Unlock()
	if m.RenameContainerFn != nil {
		return m.RenameContainerFn(ctx, containerID, newName)
	}
	return nil
}

func (m *MockDockerClient) CopyToContainer(ctx context.Context, containerID string, dstPath string, content io.Reader) error {
	m.mu.Lock()
	m.CopyToContainerCalls = append(m.CopyToContainerCalls, containerID)
	m.mu.Unlock()
	if m.CopyToContainerFn != nil {
		return m.CopyToContainerFn(ctx, containerID, dstPath, content)
	}
	return nil
}

func (m *MockDockerClient) ContainerStats(ctx context.Context, containerID string) (*docker.ContainerStats, error) {
	if m.ContainerStatsFn != nil {
		return m.ContainerStatsFn(ctx, containerID)
	}
	return &docker.ContainerStats{}, nil
}
