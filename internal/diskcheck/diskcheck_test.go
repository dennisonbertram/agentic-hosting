package diskcheck

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck_GenerousThresholds(t *testing.T) {
	dir := t.TempDir()
	err := Check(dir, 99, 100)
	assert.NoError(t, err)
}

func TestCheck_BlockThreshold(t *testing.T) {
	dir := t.TempDir()
	// blockPct=0 means any usage triggers block
	err := Check(dir, 0, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient disk space")
}

func TestCheck_WarnThreshold(t *testing.T) {
	dir := t.TempDir()
	// warnPct=0 but blockPct=100 — should warn but not error
	err := Check(dir, 0, 100)
	assert.NoError(t, err)
}

func TestCheck_NonExistentPath(t *testing.T) {
	err := Check("/nonexistent/path/that/does/not/exist", 80, 90)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "check disk space")
}

func TestCheckAll_MixedPaths(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"/nonexistent/path/abc123", dir}
	// Non-existent path is skipped; real dir with generous thresholds passes
	err := CheckAll(paths, 99, 100)
	assert.NoError(t, err)
}

func TestCheckAll_AllNonExistent(t *testing.T) {
	paths := []string{"/nonexistent/path/one", "/nonexistent/path/two"}
	err := CheckAll(paths, 80, 90)
	assert.NoError(t, err)
}

func TestCheckAll_BlockOnRealPath(t *testing.T) {
	dir := t.TempDir()
	err := CheckAll([]string{dir}, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient disk space")
	assert.Contains(t, err.Error(), dir)
}

func TestCheckAll_EmptyPaths(t *testing.T) {
	err := CheckAll([]string{}, 80, 90)
	assert.NoError(t, err)
}

func TestCheckAll_SkipsNonExistentReturnsErrorOnReal(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "test")
	require.NoError(t, err)
	f.Close()

	paths := []string{"/nonexistent/aaa", dir}
	err = CheckAll(paths, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), dir)
}
