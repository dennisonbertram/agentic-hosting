package reconciler_test

import (
	"testing"
)

// TestReconciler_CircuitBreaker verifies that 5 crashes in the window opens the circuit.
func TestReconciler_CircuitBreaker(t *testing.T) {
	t.Skip("TODO: implement")
}

// TestReconciler_AutoRecovery verifies that a circuit past circuit_retry_at is auto-recovered.
func TestReconciler_AutoRecovery(t *testing.T) {
	t.Skip("TODO: implement")
}

// TestReconciler_UnhealthyRestart verifies that unhealthy containers are stopped.
func TestReconciler_UnhealthyRestart(t *testing.T) {
	t.Skip("TODO: implement")
}

// TestReconciler_StaleDeployment verifies services stuck deploying >10min are marked failed.
func TestReconciler_StaleDeployment(t *testing.T) {
	t.Skip("TODO: implement")
}
