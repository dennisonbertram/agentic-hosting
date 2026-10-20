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
	// NetRxBytes and NetTxBytes are cumulative bytes received and sent on all
	// of the container's interfaces since it started.
	NetRxBytes uint64
	NetTxBytes uint64
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
	out := &ContainerStats{CPUTotalNanos: s.CPUStats.CPUUsage.TotalUsage, MemoryBytes: s.MemoryStats.Usage}
	for _, n := range s.Networks {
		out.NetRxBytes += n.RxBytes
		out.NetTxBytes += n.TxBytes
	}
	return out, nil
}
