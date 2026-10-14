package docker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/docker/docker/api/types/container"
)

// ContainerStats is a point-in-time resource reading for one container.
type ContainerStats struct {
	CPUTotalNanos uint64 // cumulative CPU time since the container started
	MemoryBytes   uint64 // current memory usage
}

// ContainerStats takes a single (non-streaming) stats reading.
func (c *DockerClient) ContainerStats(ctx context.Context, containerID string) (*ContainerStats, error) {
	r, err := c.cli.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("container stats: %w", err)
	}
	defer r.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode container stats: %w", err)
	}
	return &ContainerStats{CPUTotalNanos: s.CPUStats.CPUUsage.TotalUsage, MemoryBytes: s.MemoryStats.Usage}, nil
}
