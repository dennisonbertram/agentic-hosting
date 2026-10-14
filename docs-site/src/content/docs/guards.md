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
| On open | Container stopped, `circuit_open = 1` |
| Manual reset | `POST /v1/services/{id}/reset` |

Backoff (`circuitRetryBackoff`):

| `circuit_open_count` | Wait |
|----------------------|------|
| 1 (and 0 treated as 1) | 30 minutes |
| 2 | 1 hour |
| 3+ | 4 hours |

When `circuit_retry_at` elapses, the reconciler clears the circuit and sets status `stopped` so a subsequent start can happen. If the app still crash-loops, the circuit opens again and the count escalates.

**Do not script reset in a tight loop.** Reset is for “we fixed the binary / env.”

Statuses you will see: `crashed`, and after enough failures the circuit columns flip. Some docs say `circuit_open` as a status string; the DB also has a boolean `circuit_open` column. Read both `status` and `circuit_open` on `GET /v1/services/{id}`.

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
| Default memory / CPU | 512 MB / 1 CPU |

When disk is over 90%, expect **503** or a wrapped `disk check:` error on create/deploy/build — not a silent queue.
