package testutil

import (
	"context"
	"io"
	"strings"

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
