package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenWithMetering_UsesConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	meterPath := filepath.Join(dir, "custom-meter.sqlite")

	store, err := db.OpenWithMetering(filepath.Join(dir, "test.db"), meterPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	_, err = os.Stat(meterPath)
	require.NoError(t, err, "configured metering path should be created")
	_, err = os.Stat(filepath.Join(dir, "test-metering.db"))
	assert.True(t, os.IsNotExist(err), "derived path must not be used when configured")
}
