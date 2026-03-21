package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetailedHealth_IncludesDockerStorage(t *testing.T) {
	// Reset cache so previous test runs don't leak into this one.
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = false
	detailedHealthCacheMu.Unlock()

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	mockDocker := &testutil.MockDockerClient{
		DiskUsageFn: func(ctx context.Context) (*docker.StorageUsage, error) {
			return &docker.StorageUsage{
				ImagesSize:     1024 * 1024 * 1024 * 2, // 2 GB
				ContainersSize: 1024 * 1024 * 500,      // 500 MB
				VolumesSize:    1024 * 1024 * 1024,      // 1 GB
				BuildCacheSize: 1024 * 1024 * 256,       // 256 MB
			}, nil
		},
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    mockDocker,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var resp DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	require.NotNil(t, resp.DockerStorage, "docker_storage should be present in response")
	assert.Equal(t, int64(1024*1024*1024*2), resp.DockerStorage.ImagesSizeBytes, "images_size_bytes")
	assert.Equal(t, int64(1024*1024*500), resp.DockerStorage.ContainersSizeBytes, "containers_size_bytes")
	assert.Equal(t, int64(1024*1024*1024), resp.DockerStorage.VolumesSizeBytes, "volumes_size_bytes")
	assert.Equal(t, int64(1024*1024*256), resp.DockerStorage.BuildCacheSizeBytes, "build_cache_size_bytes")
	assert.Greater(t, resp.DockerStorage.TotalSizeGB, 0.0, "total_size_gb should be positive")
	assert.Equal(t, 1, mockDocker.DiskUsageCalls, "DiskUsage should have been called once")
}

func TestDetailedHealth_DockerStorageError_OmitsField(t *testing.T) {
	// Reset cache.
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = false
	detailedHealthCacheMu.Unlock()

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	mockDocker := &testutil.MockDockerClient{
		DiskUsageFn: func(ctx context.Context) (*docker.StorageUsage, error) {
			return nil, fmt.Errorf("docker not reachable")
		},
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    mockDocker,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	_, hasDockerStorage := resp["docker_storage"]
	assert.False(t, hasDockerStorage, "docker_storage should be omitted on error (omitempty)")
}

func TestDetailedHealth_NoDockerClient_OmitsDockerStorage(t *testing.T) {
	// Reset cache.
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = false
	detailedHealthCacheMu.Unlock()

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// No Docker client passed.
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	_, hasDockerStorage := resp["docker_storage"]
	assert.False(t, hasDockerStorage, "docker_storage should be omitted when no Docker client")
}

func TestDetailedHealth_ResponseIncludesDockerDisk(t *testing.T) {
	// Reset cache.
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = false
	detailedHealthCacheMu.Unlock()

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	// docker_disk should be present in the response even if /var/lib/docker doesn't
	// exist (it will be zeroed). The key point is the JSON field is emitted.
	_, hasDockerDisk := resp["docker_disk"]
	assert.True(t, hasDockerDisk, "docker_disk field should always be present in response")
}

func TestDetailedHealth_CacheBypass(t *testing.T) {
	// Make sure cache is warm.
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = true
	detailedHealthCacheTime = time.Now()
	detailedHealthCache = DetailedHealthResponse{
		Status: "ok",
		Docker: DockerInfo{Available: false},
	}
	detailedHealthCacheMu.Unlock()

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	callCount := 0
	mockDocker := &testutil.MockDockerClient{
		DiskUsageFn: func(ctx context.Context) (*docker.StorageUsage, error) {
			callCount++
			return &docker.StorageUsage{ImagesSize: 42}, nil
		},
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    mockDocker,
	})

	// First request should serve from cache (DiskUsage not called).
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rr1 := httptest.NewRecorder()
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)
	assert.Equal(t, 0, callCount, "cached response should not call DiskUsage")
}

func TestStatfsToDiskInfo(t *testing.T) {
	// Unit test the helper with known values.
	di := statfsToDiskInfo(makeFakeStatfs(100*1024*1024*1024, 20*1024*1024*1024))
	assert.InDelta(t, 100.0, di.TotalGB, 0.1, "total_gb should be ~100")
	assert.InDelta(t, 20.0, di.FreeGB, 0.1, "free_gb should be ~20")
	assert.InDelta(t, 80.0, di.UsedPercent, 0.1, "used_percent should be ~80")
}

func TestStatfsToDiskInfo_ZeroTotal(t *testing.T) {
	di := statfsToDiskInfo(makeFakeStatfs(0, 0))
	assert.Equal(t, 0.0, di.TotalGB)
	assert.Equal(t, 0.0, di.FreeGB)
	assert.Equal(t, 0.0, di.UsedPercent, "zero total should yield 0% used, not NaN")
}
