# Self-Healer Implementation — 2026-03-10

## What Was Built

Closed three gaps between the agentic.hosting landing page copy and actual behavior:

| Claim | Before | After |
|-------|--------|-------|
| "reconciler runs every 30s" | ❌ 60s | ✅ 30s |
| "Container crashed? Restarted." | ✅ Docker handles it | ✅ + auto circuit recovery |
| "Unhealthy? Restarted." | ❌ not detected | ✅ reconciler detects + stops → Docker restarts |

## Commit

`064232a` — pushed to `origin/main` on 2026-03-10.
Not yet deployed to Hetzner (65.21.67.254).

## Files Changed

### `cmd/ah/main.go` (line 181)
- `60*time.Second` → `30*time.Second` for reconciler interval

### `internal/db/migrations/state_009_circuit_recovery.sql` (new)
```sql
ALTER TABLE services ADD COLUMN circuit_retry_at INTEGER;
ALTER TABLE services ADD COLUMN circuit_open_count INTEGER NOT NULL DEFAULT 0;
```
- `circuit_retry_at`: unix timestamp for when auto-recovery should be attempted (NULL = not scheduled)
- `circuit_open_count`: tracks how many times circuit has opened (drives exponential backoff)

### `internal/docker/client.go`
- `ContainerInfo` struct: added `HealthStatus string` field
- `InspectContainer`: populates `HealthStatus` from `inspect.State.Health.Status` (nil-safe)
- `RunContainer`: added `Healthcheck` to `container.Config` — wget probe on service port, 30s interval, 5s timeout, 3 retries, 60s start grace period

### `internal/reconciler/reconciler.go`
Added three things:

**`circuitRetryBackoff(openCount int) time.Duration`** helper:
- 1st open → 30 min
- 2nd open → 1 hour
- 3rd+ → 4 hours (cap)

**Step 1a** (unhealthy restart): after crash-detection loop, inspects each running container. If `HealthStatus == "unhealthy"`, stops it (Docker's `RestartPolicy: unless-stopped` handles the actual restart), then increments `crash_count` and evaluates circuit breaker using same two-step UPDATE pattern as existing crash logic.

**Step 1b** (auto circuit recovery): queries `services WHERE circuit_open=1 AND circuit_retry_at IS NOT NULL AND circuit_retry_at <= now`. For each, resets `circuit_open=0, crash_count=0, crash_window_start=NULL, circuit_retry_at=NULL, status='stopped'`. Next reconciler tick sees `status='stopped'` and restarts.

**Existing circuit-open UPDATE** extended to also set `circuit_retry_at` and `circuit_open_count+1` when circuit opens.

## Testing Infrastructure

**There are zero test files in this repo.** No CI either. All testing is manual via `scripts/*.sh`.

Highest-value tests to add next:
1. `internal/reconciler` — unit tests for circuit breaker, `circuitRetryBackoff`, unhealthy detection (mock docker client)
2. `internal/db` — migration smoke tests against in-memory SQLite
3. `internal/docker` — `ContainerInfo.HealthStatus` nil-safety

## Next Steps

1. Deploy to Hetzner: `ssh -i ~/.ssh/id_hetzner_claudeops root@65.21.67.254`
   - Pull latest, build binary, restart `ah.service`
   - Migration runs automatically on startup
2. Verify: check log for `reconciler: starting (interval=30s)`
3. Optionally: write test suite for reconciler package

## How to Verify Each Fix

1. **Interval**: grep logs for `reconciler: starting (interval=30s)`
2. **Unhealthy restart**: deploy a service → exec into container → kill the process the health check pings → within ~90s Docker marks unhealthy → reconciler stops it → Docker restarts
3. **Auto circuit recovery**: crash a service 5× quickly → confirm `circuit_open=1` in SQLite → manually set `circuit_retry_at` to past timestamp → confirm reconciler auto-restarts on next tick
