---
title: Crash-loop & disk guards
description: Circuit breaker thresholds, exponential retry backoff, and the 80/90 disk watermarks that block deploys.
section: Operate
order: 10
---

# Crash-loop and disk guards

These exist because agents retry faster than humans.

## Circuit breaker

Implemented in `internal/reconciler` (not a library).

| Parameter | Value |
|-----------|--------|
| Crash window | 600 seconds |
| Open threshold | `crash_count >= 5` inside the window |
| On open | Container **stopped and removed**, `container_id` cleared, `circuit_open = 1` |
| Manual reset | `POST /v1/services/{id}/reset` then `POST .../redeploy` (`/start` returns 409 — no container) |

Backoff (`circuitRetryBackoff`):

| `circuit_open_count` | Wait |
|----------------------|------|
| 1 (and 0 treated as 1) | 30 minutes |
| 2 | 1 hour |
| 3+ | 4 hours |

When `circuit_retry_at` elapses, the reconciler clears the circuit and sets status `stopped`. The container is already gone; **nothing restarts it**. Use `POST .../redeploy` (not `/start`). If the app still crash-loops, the circuit opens again and the count escalates.

**Do not script reset in a tight loop.** Reset is for “we fixed the binary / env.”

`status` is never the string `"circuit_open"`. After the breaker trips, `status` stays `crashed` and `circuit_open` is a separate boolean. After auto-recovery or manual reset, `status` is `stopped`. Read both fields on `GET /v1/services/{id}`.

## Disk watermarks

`internal/diskcheck.Check(path, warnPct, blockPct)`:

- Used % ≥ warn → log warning, allow
- Used % ≥ block → return error, caller fails the operation
- Total bytes 0 → allow (cannot measure)

Every production call site uses **80 / 90** on `/var/lib/ah` and `/var/lib/docker` (or the backup directory):

- Service deploy (`services.go`, `deploy_image.go`)
- Build start
- Database create
- Kanban create
- `ah backup`

`GET /v1/system/health/detailed` reports `disk` (`/var/lib/ah`) and `docker_disk` (`/var/lib/docker`). If either `used_percent > 90`, overall `status` becomes `degraded`. Pass `?fresh=true` to skip the 30s cache.

Traefik network count is also a soft guard: warn >150 `ah-tenant-*` networks, `degraded` >200.

## Other backpressure

| Limit | Value |
|-------|--------|
| Global in-flight HTTP | 200 (`Throttle`) |
| Request body | 1 MB |
| Concurrent image deploys | 5 (`maxConcurrentDeploys`) |
| Queued deploys | 20 |
| Build slots | 10 |
| PIDs per service container | 256 |
| Default memory / CPU (new tenant) | **2048 MB / 2 CPUs** from `tenant_quotas` (`state_001_init.sql`). `RunContainer` applies those as container limits. Fallback **512 MB / 1 CPU** only if the quota row cannot be loaded (`internal/docker/client.go`). Limits are per-tenant and still bounded by the host. |

When disk is over 90%:

- `POST /v1/services` still returns **201** `deploying`; the async deploy then sets `status=failed` and `last_error` to the diskcheck message
- Build create, DB create, restart, and redeploy return **500** `{"error":"internal error"}` — diskcheck is a plain `fmt.Errorf`, and `apierr.WriteAPIError` falls back to 500. This is **not** 503.
