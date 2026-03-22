package api

import (
	"context"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
)

type HealthResponse struct {
	Status string `json:"status"`
}

type DetailedHealthResponse struct {
	Status          string             `json:"status"`
	Docker          DockerInfo         `json:"docker"`
	GVisor          GVisorInfo         `json:"gvisor"`
	Disk            DiskInfo           `json:"disk"`
	DockerDisk      DiskInfo           `json:"docker_disk"`
	DockerStorage   *DockerStorageInfo `json:"docker_storage,omitempty"`
	TraefikNetworks *int               `json:"traefik_networks,omitempty"`
}

type DockerInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type GVisorInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type DiskInfo struct {
	TotalGB     float64 `json:"total_gb"`
	FreeGB      float64 `json:"free_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// DockerStorageInfo holds Docker object storage sizes from the Docker system df API.
type DockerStorageInfo struct {
	ImagesSizeBytes     int64   `json:"images_size_bytes"`
	ContainersSizeBytes int64   `json:"containers_size_bytes"`
	VolumesSizeBytes    int64   `json:"volumes_size_bytes"`
	BuildCacheSizeBytes int64   `json:"build_cache_size_bytes"`
	TotalSizeGB         float64 `json:"total_size_gb"`
}

var (
	detailedHealthCache     DetailedHealthResponse
	detailedHealthCacheValid bool
	detailedHealthCacheMu   sync.RWMutex
	detailedHealthCacheTime time.Time
	detailedHealthCacheTTL  = 30 * time.Second
)

// handleHealth returns minimal constant-time status for public (unauthenticated) requests.
// No DB calls — avoids DoS via unauthenticated health check flooding.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// handleHealthDetailed returns full system info (authenticated only).
// Pass ?fresh=true to bypass the 30s cache and force a live check.
// This is useful during incident response when operators need real-time status.
// The fresh result still updates the cache so subsequent requests benefit.
func (s *Server) handleHealthDetailed(w http.ResponseWriter, r *http.Request) {
	_ = middleware.GetTenantID(r.Context()) // auth enforced by middleware

	fresh := r.URL.Query().Get("fresh") == "true"

	if !fresh {
		detailedHealthCacheMu.RLock()
		if detailedHealthCacheValid && time.Since(detailedHealthCacheTime) < detailedHealthCacheTTL {
			resp := detailedHealthCache // copy by value under lock
			detailedHealthCacheMu.RUnlock()
			writeJSON(w, http.StatusOK, resp)
			return
		}
		detailedHealthCacheMu.RUnlock()
	}

	resp := s.buildDetailedHealth()

	detailedHealthCacheMu.Lock()
	detailedHealthCache = resp // store by value
	detailedHealthCacheValid = true
	detailedHealthCacheTime = time.Now()
	detailedHealthCacheMu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) buildDetailedHealth() DetailedHealthResponse {
	resp := DetailedHealthResponse{
		Status: "ok",
		Docker: DockerInfo{Available: false},
		GVisor: GVisorInfo{Available: false},
	}

	// Check Docker with 5s timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		resp.Docker = DockerInfo{Available: true, Version: strings.TrimSpace(string(out))}
	}

	// Check gVisor with 5s timeout
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if out, err := exec.CommandContext(ctx2, "runsc", "--version").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		version := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "runsc version") {
				version = strings.TrimPrefix(line, "runsc version ")
				break
			}
		}
		if version == "" {
			// Fallback: use first non-empty line if standard prefix not found
			version = strings.TrimSpace(string(out))
		}
		resp.GVisor = GVisorInfo{Available: version != "", Version: version}
	}

	// Check disk for /var/lib/ah (no exec, safe syscall)
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/var/lib/ah", &stat); err == nil {
		resp.Disk = statfsToDiskInfo(stat)
	}

	// Check disk for /var/lib/docker (separate partition may fill independently)
	var dockerStat syscall.Statfs_t
	if err := syscall.Statfs("/var/lib/docker", &dockerStat); err == nil {
		resp.DockerDisk = statfsToDiskInfo(dockerStat)
	}

	// Fetch Docker object-level storage usage via the Docker API
	if s.dockerClient != nil {
		ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel3()
		if du, err := s.dockerClient.DiskUsage(ctx3); err == nil {
			totalBytes := du.ImagesSize + du.ContainersSize + du.VolumesSize + du.BuildCacheSize
			resp.DockerStorage = &DockerStorageInfo{
				ImagesSizeBytes:     du.ImagesSize,
				ContainersSizeBytes: du.ContainersSize,
				VolumesSizeBytes:    du.VolumesSize,
				BuildCacheSizeBytes: du.BuildCacheSize,
				TotalSizeGB:         round2(float64(totalBytes) / (1024 * 1024 * 1024)),
			}
		} else {
			log.Printf("WARNING: failed to fetch Docker disk usage: %v", err)
		}
	}

	// Count tenant networks connected to Traefik (ah-tenant-* prefix).
	// Warn at >150, degrade at >200 to surface accumulation early.
	if s.dockerClient != nil {
		ctx4, cancel4 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel4()
		if nets, err := s.dockerClient.NetworkList(ctx4); err == nil {
			count := 0
			for _, n := range nets {
				if len(n.Name) > 10 && n.Name[:10] == "ah-tenant-" {
					count++
				}
			}
			resp.TraefikNetworks = &count
			if count > 200 {
				resp.Status = "degraded"
				log.Printf("WARNING: traefik network count %d exceeds 200 — degraded", count)
			} else if count > 150 {
				log.Printf("WARNING: traefik network count %d approaching limit (>150)", count)
			}
		} else {
			log.Printf("WARNING: failed to list Docker networks for health check: %v", err)
		}
	}

	// Degrade status if database is unreachable
	if s.store == nil || s.store.StateDB == nil {
		resp.Status = "degraded"
	} else if err := s.store.StateDB.Ping(); err != nil {
		resp.Status = "degraded"
	}

	// Degrade status if either disk path is critically full (>90% used)
	if resp.Disk.UsedPercent > 90 || resp.DockerDisk.UsedPercent > 90 {
		resp.Status = "degraded"
	}

	return resp
}

// resetDetailedHealthCache clears the detailed health cache.
// Exported for testing only (same package).
func resetDetailedHealthCache() {
	detailedHealthCacheMu.Lock()
	detailedHealthCacheValid = false
	detailedHealthCache = DetailedHealthResponse{}
	detailedHealthCacheTime = time.Time{}
	detailedHealthCacheMu.Unlock()
}

func statfsToDiskInfo(stat syscall.Statfs_t) DiskInfo {
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	totalGB := float64(totalBytes) / (1024 * 1024 * 1024)
	freeGB := float64(freeBytes) / (1024 * 1024 * 1024)
	usedPercent := 0.0
	if totalGB > 0 {
		usedPercent = ((totalGB - freeGB) / totalGB) * 100
	}
	return DiskInfo{
		TotalGB:     round2(totalGB),
		FreeGB:      round2(freeGB),
		UsedPercent: round2(usedPercent),
	}
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
