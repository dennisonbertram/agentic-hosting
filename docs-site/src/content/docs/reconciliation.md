---
title: Reconciliation
description: The 30-second reconciler loop — crash detection, unhealthy restarts, stale deploys, split-brain, and GC.
section: Operate
order: 9
---

# Reconciliation

`internal/reconciler` runs every **30 seconds** (`main.go` passes `30*time.Second`). It is the difference between “SQLite says running” and “Docker has a live container.”

Do not `docker stop` a tenant container and expect it to stay down. If the row is `running`, the next ticks will treat that as a crash.

## What each tick does

### 1. Crash detection

For each service with `status` `running` or `crashed`, `circuit_open = 0`, and a `container_id`:

- Docker `RestartCount` grew since last tick → crash event(s)
- Missing container → crash
- Exited / dead → crash
- Transient inspect errors are **skipped** (not counted)

Crash accounting:

- Window is **600 seconds**
- First crash in a new window sets `crash_count = 1`
- Further crashes in the window increment
- At **5** crashes inside the window: `circuit_open = 1`, `circuit_open_count++`, `circuit_retry_at` set, container **stopped and removed**, `container_id` cleared

See [Crash-loop & disk guards](./guards/) for backoff.

### 1a. Unhealthy

If Docker reports `unhealthy`, the reconciler **restarts** the container (a manual stop would stay stopped under `unless-stopped`) and counts it as a crash. The platform does **not** inject a wget HEALTHCHECK — `RunContainer` comments say healthchecks are left to the image author because many images lack curl/wget.

### 1b. Auto recovery

Rows with `circuit_open = 1` and `circuit_retry_at <= now` are reset: circuit closed, `crash_count = 0`, status `stopped`. The container was already removed when the breaker tripped; **nothing restarts it**. Use `POST .../redeploy` (`/start` returns 409 `service has no container`).

### 2. Stale deploys

`deploying` longer than **10 minutes** → `failed`.

### 3. Missing containers

A stored `container_id` that Docker cannot find (when not already handled as a running-crash) → `stopped`.

### 4. Split-brain

Containers labeled `ah.service` that are not in the DB are logged for GC.

## Garbage collector

`internal/gc`, interval **5 minutes**, first run delayed **2 minutes** after process start.

Removes (resource age must be **> 10 minutes** to avoid racing deploys):

- Orphan service / database containers
- Orphan `ah-db-*` volumes
- Warm-pool leftovers (`ah-pool-` volumes, `ah.type=warm-pool`)
- Build directories older than **1 hour**
- Dangling images
- Snapshots over `--snapshot-max-per-service` (default 10) or `--snapshot-max-age` (default 30 days)

## Warm pool (environments)

Not the reconciler. `environments.PoolManager` refills every **60s**, pool size **2** per enabled template, max **6** total. Default / node / python are pooled; **go is cold-start**. Ghost rows pointing at missing containers are pruned on refill.

## Implications for operators

| Action | Use |
|--------|-----|
| Stop a service | `POST /v1/services/{id}/stop` |
| Pause crash loops | Wait for circuit, or `POST .../reset` then `POST .../redeploy` after you fix the app |
| Delete leftovers | API delete + wait for GC (10 min age gate) |
| Inspect drift | `GET /v1/system/health/detailed?fresh=true` and `docker ps` |
