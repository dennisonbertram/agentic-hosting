// Package reconciler white-box tests for unexported helpers.
package reconciler

import (
	"testing"
	"time"
)

// TestCircuitRetryBackoff verifies that the backoff durations match the spec:
//   - openCount <= 1 → 30 minutes
//   - openCount == 2 → 1 hour
//   - openCount >= 3 → 4 hours
//
// These values govern how long a service stays in the open-circuit state before
// a recovery attempt is made. Too-short values risk amplifying restart storms;
// too-long values leave legitimate services down unnecessarily.
func TestCircuitRetryBackoff(t *testing.T) {
	tests := []struct {
		openCount int
		want      time.Duration
	}{
		{0, 30 * time.Minute},
		{1, 30 * time.Minute},
		{2, 1 * time.Hour},
		{3, 4 * time.Hour},
		{10, 4 * time.Hour},
		{100, 4 * time.Hour},
	}
	for _, tc := range tests {
		got := circuitRetryBackoff(tc.openCount)
		if got != tc.want {
			t.Errorf("circuitRetryBackoff(%d) = %v, want %v", tc.openCount, got, tc.want)
		}
	}
}
