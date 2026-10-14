package metering

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each sample row covers SampleSeconds seconds, so a service using a steady
// 100 MB across 3 samples must report an average of 100, not the sum.
func TestQueryMetrics_MemoryMBAvgIsAverage(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 3; i++ {
		insertEvent(t, sqlDB, fmt.Sprintf("a%d", i), "t1", "s1", 1, 100*SampleSeconds, 0, 0, ts+int64(i)*60)
	}
	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	m, err := store.QueryServiceMetrics("t1", "s1", "daily", since, until)
	require.NoError(t, err)
	require.Len(t, m, 1)
	assert.InDelta(t, 100.0, m[0].MemoryMBAvg, 0.01)
}
