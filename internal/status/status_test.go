package status_test

import (
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/status"
	"github.com/stretchr/testify/assert"
)

// The CLI compares against these strings, so they must stay values the server
// actually accepts for services.
func TestServiceStatusesAreServerValues(t *testing.T) {
	for _, s := range []string{status.ServiceRunning, status.ServiceFailed, status.ServiceCrashed, status.ServiceError} {
		assert.True(t, services.ValidStatuses[s], "service status %q not in services.ValidStatuses", s)
	}
}
