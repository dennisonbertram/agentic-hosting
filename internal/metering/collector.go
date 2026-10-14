package metering

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Collector samples CPU and memory of running service containers every
// SampleSeconds and records them in usage_events and usage_daily.
type Collector struct {
	state        *sql.DB
	meter        *sql.DB
	docker       docker.Client
	lastCPU      map[string]uint64 // container ID -> cumulative CPU ns at last recorded sample
	warned       map[string]bool   // container IDs whose stats failure was already logged
	statsTimeout time.Duration
}

func NewCollector(state, meter *sql.DB, d docker.Client) *Collector {
	return &Collector{state: state, meter: meter, docker: d, lastCPU: map[string]uint64{}, warned: map[string]bool{}, statsTimeout: 10 * time.Second}
}

// Run collects until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(SampleSeconds * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Tick(ctx)
		}
	}
}

// Tick records one sample for every running service. A failure for one
// container is logged and does not affect the others.
func (c *Collector) Tick(ctx context.Context) {
	rows, err := c.state.QueryContext(ctx, `SELECT id, tenant_id, container_id FROM services WHERE status = 'running' AND container_id IS NOT NULL AND container_id != ''`)
	if err != nil {
		log.Printf("metering: list running services: %v", err)
		return
	}
	type svc struct{ id, tenant, container string }
	var svcs []svc
	for rows.Next() {
		var s svc
		if err := rows.Scan(&s.id, &s.tenant, &s.container); err != nil {
			log.Printf("metering: scan service: %v", err)
			continue
		}
		svcs = append(svcs, s)
	}
	rows.Close()

	now := time.Now().UTC()
	seen := map[string]bool{}
	for _, s := range svcs {
		if ctx.Err() != nil {
			return
		}
		seen[s.container] = true
		sctx, cancel := context.WithTimeout(ctx, c.statsTimeout)
		st, err := c.docker.ContainerStats(sctx, s.container)
		cancel()
		if err != nil {
			// A running row whose container is gone fails every tick; log it once.
			if !c.warned[s.container] {
				c.warned[s.container] = true
				log.Printf("metering: stats for container %s (service %s): %v (further failures not logged)", s.container, s.id, err)
			}
			continue
		}
		delete(c.warned, s.container)
		// CPU counter is cumulative and keyed by container, so a replaced container
		// starts a new baseline. The first sample records 0; a counter reset
		// (container restart) records the new total.
		var cpuSec float64
		if prev, ok := c.lastCPU[s.container]; ok {
			if st.CPUTotalNanos >= prev {
				cpuSec = float64(st.CPUTotalNanos-prev) / 1e9
			} else {
				cpuSec = float64(st.CPUTotalNanos) / 1e9
			}
		}
		memMBSec := float64(st.MemoryBytes) / (1024 * 1024) * SampleSeconds

		if err := c.record(s.tenant, s.id, cpuSec, memMBSec, now); err != nil {
			log.Printf("metering: record service %s: %v", s.id, err)
			continue // keep the old baseline so the next delta covers this interval
		}
		c.lastCPU[s.container] = st.CPUTotalNanos
	}
	// Drop state for containers that are no longer running services.
	for id := range c.lastCPU {
		if !seen[id] {
			delete(c.lastCPU, id)
		}
	}
	for id := range c.warned {
		if !seen[id] {
			delete(c.warned, id)
		}
	}
}

func (c *Collector) record(tenantID, serviceID string, cpuSec, memMBSec float64, now time.Time) error {
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return err
	}
	tx, err := c.meter.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, recorded_at)
		VALUES (?, ?, ?, 'sample', ?, ?, ?)`, hex.EncodeToString(idb), tenantID, serviceID, cpuSec, memMBSec, now.Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO usage_daily (tenant_id, service_id, date, cpu_seconds, memory_mb_seconds)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, date, service_id) DO UPDATE SET
			cpu_seconds = cpu_seconds + excluded.cpu_seconds,
			memory_mb_seconds = memory_mb_seconds + excluded.memory_mb_seconds`,
		tenantID, serviceID, now.Format("2006-01-02"), cpuSec, memMBSec); err != nil {
		return err
	}
	return tx.Commit()
}
