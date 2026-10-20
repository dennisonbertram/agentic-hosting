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

// Resource kinds recorded in usage_events.resource_kind and usage_daily.resource_kind.
const (
	ResourceKindService = "service"
	ResourceKindSandbox = "sandbox"
)

// Collector samples CPU and memory of running service and sandbox containers every
// SampleSeconds and records them in usage_events and usage_daily.
type Collector struct {
	state        *sql.DB
	meter        *sql.DB
	docker       docker.Client
	lastCPU      map[string]uint64    // container ID -> cumulative CPU ns at last recorded sample
	lastNet      map[string][2]uint64 // container ID -> cumulative [rx, tx] bytes (egress sandboxes)
	warned       map[string]bool      // container IDs whose stats failure was already logged
	statsTimeout time.Duration
	// startedAt (unix seconds) separates sandboxes created while this
	// collector was running, whose counters it has seen from zero, from ones
	// left over from a previous ah process, whose earlier usage is unknown.
	startedAt int64
}

func NewCollector(state, meter *sql.DB, d docker.Client) *Collector {
	return &Collector{state: state, meter: meter, docker: d, lastCPU: map[string]uint64{}, lastNet: map[string][2]uint64{}, warned: map[string]bool{}, statsTimeout: 10 * time.Second, startedAt: time.Now().Unix()}
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
	type svc struct {
		id, tenant, container, kind string
		meterNet                    bool // record network bytes (internet-egress sandboxes)
		// fromZero: the container started after this collector did, so with
		// no baseline its cumulative counters are all unrecorded usage.
		fromZero bool
	}
	var svcs []svc
	for rows.Next() {
		s := svc{kind: ResourceKindService}
		if err := rows.Scan(&s.id, &s.tenant, &s.container); err != nil {
			log.Printf("metering: scan service: %v", err)
			continue
		}
		svcs = append(svcs, s)
	}
	rows.Close()

	// Running sandboxes are sampled the same way; the sandbox ID goes in the
	// service_id column with resource_kind='sandbox'. IDs are random 128-bit
	// values, so they cannot collide with service IDs in usage_daily's key.
	// Sandboxes with internet egress also record network bytes, so per-tenant
	// egress shows in /v1/tenant/usage/metrics?kind=sandbox.
	sbxRows, err := c.state.QueryContext(ctx, `SELECT id, tenant_id, container_id, egress = 'internet', created_at FROM environments
		WHERE kind = 'sandbox' AND status = 'running' AND container_id IS NOT NULL AND container_id != ''`)
	if err != nil {
		log.Printf("metering: list running sandboxes: %v", err)
	} else {
		for sbxRows.Next() {
			s := svc{kind: ResourceKindSandbox}
			var createdAt int64
			if err := sbxRows.Scan(&s.id, &s.tenant, &s.container, &s.meterNet, &createdAt); err != nil {
				log.Printf("metering: scan sandbox: %v", err)
				continue
			}
			// A sandbox created after this collector started has a container
			// whose counters began at zero under our watch. Without this a
			// sandbox living less than one tick would record no CPU or egress.
			s.fromZero = createdAt > c.startedAt
			svcs = append(svcs, s)
		}
		sbxRows.Close()
	}

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
				log.Printf("metering: stats for container %s (%s %s): %v (further failures not logged)", s.container, s.kind, s.id, err)
			}
			continue
		}
		delete(c.warned, s.container)
		// CPU counter is cumulative and keyed by container, so a replaced container
		// starts a new baseline. The first sample records 0, except for a
		// sandbox created while this collector ran (fromZero), whose whole
		// counter is new usage; a counter reset (container restart) records the
		// new total.
		var cpuSec float64
		if prev, ok := c.lastCPU[s.container]; ok {
			if st.CPUTotalNanos >= prev {
				cpuSec = float64(st.CPUTotalNanos-prev) / 1e9
			} else {
				cpuSec = float64(st.CPUTotalNanos) / 1e9
			}
		} else if s.fromZero {
			cpuSec = float64(st.CPUTotalNanos) / 1e9
		}
		memMBSec := float64(st.MemoryBytes) / (1024 * 1024) * SampleSeconds

		// Network counters follow the CPU rules: the first sample is the
		// baseline, a reset (restart) records the new total.
		var rx, tx uint64
		if s.meterNet {
			if prev, ok := c.lastNet[s.container]; ok {
				rx, tx = counterDelta(st.NetRxBytes, prev[0]), counterDelta(st.NetTxBytes, prev[1])
			} else if s.fromZero {
				rx, tx = st.NetRxBytes, st.NetTxBytes
			}
		}

		if err := c.record(s.tenant, s.id, s.kind, cpuSec, memMBSec, rx, tx, now); err != nil {
			log.Printf("metering: record %s %s: %v", s.kind, s.id, err)
			continue // keep the old baseline so the next delta covers this interval
		}
		c.lastCPU[s.container] = st.CPUTotalNanos
		if s.meterNet {
			c.lastNet[s.container] = [2]uint64{st.NetRxBytes, st.NetTxBytes}
		}
	}
	// Drop state for containers that are no longer running services.
	for id := range c.lastCPU {
		if !seen[id] {
			delete(c.lastCPU, id)
		}
	}
	for id := range c.lastNet {
		if !seen[id] {
			delete(c.lastNet, id)
		}
	}
	for id := range c.warned {
		if !seen[id] {
			delete(c.warned, id)
		}
	}
}

// counterDelta is cur-prev for a cumulative counter, or cur after a reset.
func counterDelta(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}

func (c *Collector) record(tenantID, serviceID, kind string, cpuSec, memMBSec float64, rxBytes, txBytes uint64, now time.Time) error {
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return err
	}
	tx, err := c.meter.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, recorded_at, resource_kind,
		network_ingress_bytes, network_egress_bytes)
		VALUES (?, ?, ?, 'sample', ?, ?, ?, ?, ?, ?)`, hex.EncodeToString(idb), tenantID, serviceID, cpuSec, memMBSec, now.Unix(), kind,
		int64(rxBytes), int64(txBytes)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO usage_daily (tenant_id, service_id, date, cpu_seconds, memory_mb_seconds, resource_kind,
		network_ingress_bytes, network_egress_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, date, service_id) DO UPDATE SET
			cpu_seconds = cpu_seconds + excluded.cpu_seconds,
			memory_mb_seconds = memory_mb_seconds + excluded.memory_mb_seconds,
			network_ingress_bytes = network_ingress_bytes + excluded.network_ingress_bytes,
			network_egress_bytes = network_egress_bytes + excluded.network_egress_bytes`,
		tenantID, serviceID, now.Format("2006-01-02"), cpuSec, memMBSec, kind, int64(rxBytes), int64(txBytes)); err != nil {
		return err
	}
	return tx.Commit()
}
