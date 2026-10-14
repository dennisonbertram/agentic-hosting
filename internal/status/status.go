// Package status holds the status strings the server emits, so the CLI
// compares against real values. Source: internal/services, internal/databases,
// internal/builds.
package status

const (
	ServiceRunning = "running"
	ServiceFailed  = "failed"
	ServiceCrashed = "crashed"
	ServiceError   = "error" // accepted by the server's ValidStatuses

	DatabaseReady  = "ready"
	DatabaseFailed = "failed"

	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"
)
