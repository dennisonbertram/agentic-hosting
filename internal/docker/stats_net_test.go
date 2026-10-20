package docker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerStats_SumsNetworkCounters(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /containers/cid/stats", 200, map[string]any{
		"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 42}},
		"memory_stats": map[string]any{"usage": 1024},
		"networks": map[string]any{
			"eth0": map[string]any{"rx_bytes": 100, "tx_bytes": 10},
			"eth1": map[string]any{"rx_bytes": 5, "tx_bytes": 1},
		},
	})
	st, err := c.ContainerStats(context.Background(), "cid")
	require.NoError(t, err)
	assert.EqualValues(t, 42, st.CPUTotalNanos)
	assert.EqualValues(t, 1024, st.MemoryBytes)
	assert.EqualValues(t, 105, st.NetRxBytes)
	assert.EqualValues(t, 11, st.NetTxBytes)
}
