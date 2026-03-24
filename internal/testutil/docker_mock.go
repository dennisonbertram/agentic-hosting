package testutil

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// MockDockerClient is a test double for docker.Client.
// Set the function fields to control return values; inspect the recorded fields to verify calls.
type MockDockerClient struct {
	// EnsureNetwork
	EnsureNetworkFn    func(ctx context.Context, name string) (string, error)
	EnsureNetworkCalls []string // network names passed

	// ConnectNetwork
	ConnectNetworkFn    func(ctx context.Context, networkID, containerID string) error
	ConnectNetworkCalls [][2]string // [networkID, containerID] pairs

	// NetworkDisconnect
	NetworkDisconnectFn    func(ctx context.Context, networkID, containerID string) error
	NetworkDisconnectCalls [][2]string // [networkID, containerID] pairs

	// NetworkList
	NetworkListFn    func(ctx context.Context) ([]docker.NetworkInfo, error)
	NetworkListCalls int

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
	RunDatabaseFn    func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error)
	RunDatabaseCalls int

	// StopAndRemoveByName
	StopAndRemoveByNameFn    func(ctx context.Context, name string) error
	StopAndRemoveByNameCalls []string // container names

	// PruneDanglingImages
	PruneDanglingImagesFn    func(ctx context.Context) (int, error)
	PruneDanglingImagesCalls int

	// ListVolumes
	ListVolumesFn    func(ctx context.Context, prefix string) ([]string, error)
	ListVolumesCalls []string // prefixes queried

	// DiskUsage
	DiskUsageFn    func(ctx context.Context) (*docker.StorageUsage, error)
	DiskUsageCalls int

	// RunEnvironment
	RunEnvironmentFn    func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error)
	RunEnvironmentCalls int

	// ExecCreate
	ExecCreateFn    func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error)
	ExecCreateCalls []string // containerIDs

	// ExecRun
	ExecRunFn    func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error)
	ExecRunCalls []string // exec IDs

	// RenameContainer
	RenameContainerFn    func(ctx context.Context, containerID, newName string) error
	RenameContainerCalls int

	// CopyToContainer
	CopyToContainerFn    func(ctx context.Context, containerID string, dstPath string, content io.Reader) error
	CopyToContainerCalls []string // containerIDs
}

// Ensure MockDockerClient satisfies the docker.Client interface at compile time.
var _ docker.Client = (*MockDockerClient)(nil)

func (m *MockDockerClient) EnsureNetwork(ctx context.Context, name string) (string, error) {
	m.EnsureNetworkCalls = append(m.EnsureNetworkCalls, name)
	if m.EnsureNetworkFn != nil {
		return m.EnsureNetworkFn(ctx, name)
	}
	return "net-" + name, nil
}

func (m *MockDockerClient) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	m.ConnectNetworkCalls = append(m.ConnectNetworkCalls, [2]string{networkID, containerID})
	if m.ConnectNetworkFn != nil {
		return m.ConnectNetworkFn(ctx, networkID, containerID)
	}
	return nil
}

func (m *MockDockerClient) NetworkDisconnect(ctx context.Context, networkID, containerID string) error {
	m.NetworkDisconnectCalls = append(m.NetworkDisconnectCalls, [2]string{networkID, containerID})
	if m.NetworkDisconnectFn != nil {
		return m.NetworkDisconnectFn(ctx, networkID, containerID)
	}
	return nil
}

func (m *MockDockerClient) NetworkList(ctx context.Context) ([]docker.NetworkInfo, error) {
	m.NetworkListCalls++
	if m.NetworkListFn != nil {
		return m.NetworkListFn(ctx)
	}
	return nil, nil
}

func (m *MockDockerClient) RemoveNetwork(ctx context.Context, networkID string) error {
	m.RemoveNetworkCalls = append(m.RemoveNetworkCalls, networkID)
	if m.RemoveNetworkFn != nil {
		return m.RemoveNetworkFn(ctx, networkID)
	}
	return nil
}

func (m *MockDockerClient) RunContainer(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *docker.ResourceLimits) (string, error) {
	m.RunContainerCalls++
	if m.RunContainerFn != nil {
		return m.RunContainerFn(ctx, tenantID, serviceID, img, port, envVars, extraLabels, limits)
	}
	return "mock-container-" + serviceID, nil
}

func (m *MockDockerClient) StopContainer(ctx context.Context, containerID string) error {
	m.StopContainerCalls = append(m.StopContainerCalls, containerID)
	if m.StopContainerFn != nil {
		return m.StopContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) StartContainer(ctx context.Context, containerID string) error {
	m.StartContainerCalls = append(m.StartContainerCalls, containerID)
	if m.StartContainerFn != nil {
		return m.StartContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) RemoveContainer(ctx context.Context, containerID string) error {
	m.RemoveContainerCalls = append(m.RemoveContainerCalls, containerID)
	if m.RemoveContainerFn != nil {
		return m.RemoveContainerFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) LogsContainer(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
	m.LogsContainerCalls = append(m.LogsContainerCalls, containerID)
	if m.LogsContainerFn != nil {
		return m.LogsContainerFn(ctx, containerID, follow, tail)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *MockDockerClient) InspectContainer(ctx context.Context, containerID string) (*docker.ContainerInfo, error) {
	m.InspectContainerCalls = append(m.InspectContainerCalls, containerID)
	if m.InspectContainerFn != nil {
		return m.InspectContainerFn(ctx, containerID)
	}
	return &docker.ContainerInfo{Status: "running"}, nil
}

func (m *MockDockerClient) PullImage(ctx context.Context, img string) error {
	m.PullImageCalls = append(m.PullImageCalls, img)
	if m.PullImageFn != nil {
		return m.PullImageFn(ctx, img)
	}
	return nil
}

func (m *MockDockerClient) TagImage(ctx context.Context, source, target string) error {
	m.TagImageCalls = append(m.TagImageCalls, [2]string{source, target})
	if m.TagImageFn != nil {
		return m.TagImageFn(ctx, source, target)
	}
	return nil
}

func (m *MockDockerClient) RemoveImage(ctx context.Context, imageRef string) error {
	m.RemoveImageCalls = append(m.RemoveImageCalls, imageRef)
	if m.RemoveImageFn != nil {
		return m.RemoveImageFn(ctx, imageRef)
	}
	return nil
}

func (m *MockDockerClient) ListContainersByLabel(ctx context.Context, label, value string) ([]string, error) {
	m.ListContainersByLabelCalls++
	if m.ListContainersByLabelFn != nil {
		return m.ListContainersByLabelFn(ctx, label, value)
	}
	return nil, nil
}

func (m *MockDockerClient) GetContainerLabels(ctx context.Context, containerID string) map[string]string {
	m.GetContainerLabelsCalls = append(m.GetContainerLabelsCalls, containerID)
	if m.GetContainerLabelsFn != nil {
		return m.GetContainerLabelsFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) GetContainerName(ctx context.Context, containerID string) string {
	m.GetContainerNameCalls = append(m.GetContainerNameCalls, containerID)
	if m.GetContainerNameFn != nil {
		return m.GetContainerNameFn(ctx, containerID)
	}
	return ""
}

func (m *MockDockerClient) VerifyGVisorRuntime(ctx context.Context) error {
	m.VerifyGVisorRuntimeCalls++
	if m.VerifyGVisorRuntimeFn != nil {
		return m.VerifyGVisorRuntimeFn(ctx)
	}
	return nil
}

func (m *MockDockerClient) CreateVolume(ctx context.Context, name string) error {
	m.CreateVolumeCalls = append(m.CreateVolumeCalls, name)
	if m.CreateVolumeFn != nil {
		return m.CreateVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RemoveVolume(ctx context.Context, name string) error {
	m.RemoveVolumeCalls = append(m.RemoveVolumeCalls, name)
	if m.RemoveVolumeFn != nil {
		return m.RemoveVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RemoveVolumeSafe(ctx context.Context, name string) error {
	m.RemoveVolumeSafeCalls = append(m.RemoveVolumeSafeCalls, name)
	if m.RemoveVolumeSafeFn != nil {
		return m.RemoveVolumeSafeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) WipeVolume(ctx context.Context, name string) error {
	m.WipeVolumeCalls = append(m.WipeVolumeCalls, name)
	if m.WipeVolumeFn != nil {
		return m.WipeVolumeFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) RunDatabase(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
	m.RunDatabaseCalls++
	if m.RunDatabaseFn != nil {
		return m.RunDatabaseFn(ctx, cfg)
	}
	return "mock-db-container-" + cfg.Name, nil
}

func (m *MockDockerClient) StopAndRemoveByName(ctx context.Context, name string) error {
	m.StopAndRemoveByNameCalls = append(m.StopAndRemoveByNameCalls, name)
	if m.StopAndRemoveByNameFn != nil {
		return m.StopAndRemoveByNameFn(ctx, name)
	}
	return nil
}

func (m *MockDockerClient) PruneDanglingImages(ctx context.Context) (int, error) {
	m.PruneDanglingImagesCalls++
	if m.PruneDanglingImagesFn != nil {
		return m.PruneDanglingImagesFn(ctx)
	}
	return 0, nil
}

func (m *MockDockerClient) ListVolumes(ctx context.Context, prefix string) ([]string, error) {
	m.ListVolumesCalls = append(m.ListVolumesCalls, prefix)
	if m.ListVolumesFn != nil {
		return m.ListVolumesFn(ctx, prefix)
	}
	return nil, nil
}

func (m *MockDockerClient) DiskUsage(ctx context.Context) (*docker.StorageUsage, error) {
	m.DiskUsageCalls++
	if m.DiskUsageFn != nil {
		return m.DiskUsageFn(ctx)
	}
	return &docker.StorageUsage{}, nil
}

func (m *MockDockerClient) RunEnvironment(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
	m.RunEnvironmentCalls++
	if m.RunEnvironmentFn != nil {
		return m.RunEnvironmentFn(ctx, cfg)
	}
	return "mock-env-container-" + cfg.EnvID, nil
}

func (m *MockDockerClient) ExecCreate(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
	m.ExecCreateCalls = append(m.ExecCreateCalls, containerID)
	if m.ExecCreateFn != nil {
		return m.ExecCreateFn(ctx, containerID, cmd, workDir)
	}
	return "mock-exec-id", nil
}

func (m *MockDockerClient) ExecRun(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
	m.ExecRunCalls = append(m.ExecRunCalls, execID)
	if m.ExecRunFn != nil {
		return m.ExecRunFn(ctx, execID, timeout)
	}
	return nil, nil, 0, nil
}

func (m *MockDockerClient) RenameContainer(ctx context.Context, containerID, newName string) error {
	m.RenameContainerCalls++
	if m.RenameContainerFn != nil {
		return m.RenameContainerFn(ctx, containerID, newName)
	}
	return nil
}

func (m *MockDockerClient) CopyToContainer(ctx context.Context, containerID string, dstPath string, content io.Reader) error {
	m.CopyToContainerCalls = append(m.CopyToContainerCalls, containerID)
	if m.CopyToContainerFn != nil {
		return m.CopyToContainerFn(ctx, containerID, dstPath, content)
	}
	return nil
}
