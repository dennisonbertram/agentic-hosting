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

// TestHealthDetailed_DefaultUsesCache verifies that the detailed health endpoint
// returns cached results within the TTL window when ?fresh is not set.
func TestHealthDetailed_DefaultUsesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates the cache.
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	var resp1 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr1.Body).Decode(&resp1))
	require.Equal(t, "ok", resp1.Status, "DB is up, status should be ok")

	// Close the DB so buildDetailedHealth would return "degraded" if called.
	stateDB.Close()

	// Second request — without ?fresh, should return the cached "ok" response.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	assert.Equal(t, "ok", resp2.Status,
		"cached response should still show 'ok' even though DB is now closed")
}

// TestHealthDetailed_FreshBypassesCache verifies that ?fresh=true skips the cache
// and returns live probe results, allowing operators to see real-time status
// during incident response.
func TestHealthDetailed_FreshBypassesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates the cache with "ok".
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	var resp1 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr1.Body).Decode(&resp1))
	require.Equal(t, "ok", resp1.Status)

	// Close the DB so buildDetailedHealth will return "degraded".
	stateDB.Close()

	// Second request WITH ?fresh=true — should bypass cache and return "degraded".
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed?fresh=true", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	assert.Equal(t, "degraded", resp2.Status,
		"fresh=true should bypass cache and detect the closed DB as degraded")
}

// TestHealthDetailed_FreshUpdatesCache verifies that after a ?fresh=true request,
// subsequent normal requests return the fresh result (i.e. the cache was updated).
func TestHealthDetailed_FreshUpdatesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates cache with "ok".
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	// Close DB, then do a fresh request to update cache to "degraded".
	stateDB.Close()

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed?fresh=true", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	require.Equal(t, "degraded", resp2.Status)

	// Third request — normal (no fresh), should now return cached "degraded".
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr3, req3)
	require.Equal(t, http.StatusOK, rr3.Code)

	var resp3 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr3.Body).Decode(&resp3))
	assert.Equal(t, "degraded", resp3.Status,
		"cache should have been updated by the fresh request, so normal request returns degraded")
}

func TestDetailedHealth_IncludesDockerStorage(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

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
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

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
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

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
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

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

func TestDetailedHealth_IncludesTraefikNetworks(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	mockDocker := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			return []docker.NetworkInfo{
				{ID: "n1", Name: "ah-tenant-aaa", Containers: 2},
				{ID: "n2", Name: "ah-tenant-bbb", Containers: 1},
				{ID: "n3", Name: "bridge", Containers: 5},
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

	require.Equal(t, http.StatusOK, rr.Code)

	var resp DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	require.NotNil(t, resp.TraefikNetworks, "traefik_networks should be present")
	assert.Equal(t, 2, *resp.TraefikNetworks, "should count only ah-tenant-* networks")
	assert.Equal(t, "ok", resp.Status, "2 networks should not degrade status")
}

func TestDetailedHealth_TraefikNetworks_DegradedOver200(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Generate 201 tenant networks to trigger degradation.
	mockDocker := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			nets := make([]docker.NetworkInfo, 201)
			for i := 0; i < 201; i++ {
				nets[i] = docker.NetworkInfo{
					ID:         fmt.Sprintf("n%d", i),
					Name:       fmt.Sprintf("ah-tenant-%04d", i),
					Containers: 1,
				}
			}
			return nets, nil
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

	var resp DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	require.NotNil(t, resp.TraefikNetworks)
	assert.Equal(t, 201, *resp.TraefikNetworks)
	assert.Equal(t, "degraded", resp.Status, "status should degrade when >200 tenant networks")
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
