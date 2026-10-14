# Plan: fix the 2026-10-14 codebase review findings

Status: implemented on branch fix/review-waves-1-5 (PR #150), 2026-10-14.
Source: full-repo review on 2026-10-14 (five read-only reviewers, one per area).

Context: the operator is the only user today; the agentic-hosting website
is the only deployed service. So:

- Bugs that need a hostile second tenant (global throttle exhaustion,
  bootstrap-token misuse, quota races, hostname collisions) are moved to
  later waves. Fix them before opening registration to other people.
- Breaking API changes do not need the 90-day deprecation window while
  there are no other clients. Note each one in CHANGELOG.
- Data loss and broken core features come first.

## How to read this plan

Each workstream below is one PR. A workstream lists the findings it fixes,
the fix, the regression test that must fail before the fix, and the files it
owns. Workstreams in the same wave touch different files and can run in
parallel. A later wave depends on an earlier one.

Finding status:

- **Confirmed**: the lead read the code path and the defect is real.
- **Reproduced**: a reviewer reproduced it with a probe test; the lead has not re-read it.
- **Plausible**: found by reading only. The worker must reproduce it with a
  failing test first. If the test cannot be made to fail, report back and drop it.

Rules for every workstream:

- Write the failing regression test first, then fix.
- Run `go build ./...`, `go vet ./...` and the package's tests. Run
  `go test -race` for any workstream that touches locks or shared state.
- Fix tests whose fixtures encode wrong behavior (for example CLI tests that
  use status values the server never emits). Do not weaken tests to pass.
- Run `/code-astra-review` once on the final diff before opening the PR.
- Changes to auth, crypto, rate limiting, the reconciler, the circuit breaker,
  health probes, or bootstrap token logic need that review before merge
  (see CLAUDE.md).
- Add a CHANGELOG entry. No schema changes without a new migration file.

## Wave 1: data loss and host-wide risk

These fix bugs that destroy data or affect every tenant. All five can run in parallel.

### W1-A: backup retention and backup tool (`cmd/ah/backup.go`)

| Finding | Status |
|---|---|
| Retention counts state and metering backups together; after 10 runs no state backups remain | Confirmed |
| `mode=ro` is ignored because the DSN has no `file:` prefix; backup opens the live DB read-write | Reproduced |
| Metering path is derived by slicing 3 chars off `dbPath`; panics on short paths, wrong for non-`.db` names | Reproduced |

Fix: apply retention per prefix (`ah-2*` and `ah-metering-*`), keep N of
each. Open with a `file:...?mode=ro` URI. Derive the metering path with the
same function `db.Open` uses (`strings.TrimSuffix`).

Test: new `cmd/ah/backup_test.go`. Simulate 12 backup runs and assert 10 state
and 10 metering backups remain. Assert a write through the backup DSN fails.

Operations follow-up (outside the code): check the production backup
directory. If state backups are already gone, take one manually now.

### W1-B: master-key rotation keeps API keys valid (`internal/crypto/`, `cmd/ah/rotate_key.go`, `internal/middleware/auth.go`)

| Finding | Status |
|---|---|
| API key hashes are `HMAC(secret, masterKey)`; rotation never updates them, so every key returns 401 after rotation | Confirmed |

Fix (decided): store a dedicated API-key hashing key, encrypted under the
master key (new `server_secrets` row), and re-encrypt that row during
rotation. Key hashes then never change. The migration creates the row from
the current master key, so existing hashes stay valid.

This touches the master key path, so it needs the security review noted in CLAUDE.md.

Test: extend `rotate_test.go`. Create a key, rotate, re-open with the new
master key, and assert `Auth` returns 200.

### W1-C: volume wipe cannot fill the disk (`internal/docker/client.go` `WipeVolume` only)

| Finding | Status |
|---|---|
| `dd` with no `count=` writes until the disk is full | Confirmed |
| Wipe container uses default runtime and bridge, no limits | Reproduced |
| On timeout the container keeps running; volume removal then fails | Reproduced |
| `busybox:latest` is never pulled; if absent, the wipe silently does nothing | Reproduced |

Fix: overwrite each file to its own size only (`count` from `stat -c %s`, or
`shred -n1 -z` if busybox's supports it, or `truncate -s 0` then `rm` if a
secure wipe is not required). Pin and pull the image. Use `runsc`,
`NetworkMode: none`, `CapDrop: ALL`, memory and pids limits. On context
expiry, kill and remove the container before returning.

Test: unit test asserting the generated command bounds each write, plus a
Docker-tagged integration test (skipped without Docker) that wipes a volume
on a small tmpfs and checks the file size stays the same.

### W1-D: long-running routes are cut off at 30s (`cmd/ah/main.go`)

| Finding | Status |
|---|---|
| `http.Server.WriteTimeout` is 30s, which cuts off `exec`, log follow and slow creates | Confirmed |
| `--listen-addr ::1` builds `::1:8080` | Reproduced |

Fix: remove the server-wide `WriteTimeout` (keep `ReadHeaderTimeout`; the
per-route `chimw.Timeout` already covers normal routes), or extend the write
deadline per request on long routes with
`http.NewResponseController(w).SetWriteDeadline`. Use `net.JoinHostPort`.

Test: server test with a 2s handler on a long route asserting the client gets
the full response.

## Wave 2: core flows that do not work

Parallel within the wave. W2-A and W2-B both touch `internal/services/services.go`;
W2-A owns `Start`/`Restart`/`Deploy`/`ResetCircuitBreaker`/`Delete`, and W2-B
owns nothing in that file except `isNotFoundError`. Merge W2-A first if they
conflict.

### W2-A: services can always be redeployed and deleted (`internal/services/`, `internal/api/services.go`, `internal/snapshots/`, new migration)

| Finding | Status |
|---|---|
| A service with no container can never be redeployed (Start/Restart/redeploy refuse) | Confirmed |
| A service with any snapshot cannot be deleted (foreign key with no `ON DELETE`) | Confirmed |
| Snapshot restore pulls from the registry, but snapshots are only tagged locally | Plausible |
| Restart error paths leave deployment rows `deploying` forever | Reproduced |
| Rename re-routes Traefik to `svc.Port`, dropping the `PORT` env override; rename takes no lock | Reproduced |

Fix: when `container_id` is empty, make `Restart` (and therefore `/redeploy`)
fall through to the full `Deploy` path. Keep `Start` strict but change its
error to point at `/redeploy`. In `Delete`, delete the service's snapshots and
untag their images inside the same operation (or add `ON DELETE CASCADE` via
a new migration that rebuilds the table). For restore, skip `PullImage`
when the image exists locally, or push the snapshot tag at creation time.
Mark the deployment failed on every Restart early return. Extract the
port-resolution logic used by Deploy and reuse it in `UpdateName`, under
`lockService`.

Test: pull fails once, then `/redeploy` succeeds. Reset circuit, then
`/redeploy` succeeds. Delete a service with a snapshot. Rename with `PORT` set
keeps the port.

### W2-B: reconciler crash detection and circuit breaker (`internal/reconciler/`)

| Finding | Status |
|---|---|
| Only `running` rows are scanned; first crash sets `crashed`, so the breaker never trips | Confirmed |
| Docker's `unless-stopped` shows crash loops as `restarting`, which is not counted | Reproduced |
| Step 1a stops unhealthy containers with a manual stop, which Docker never restarts | Reproduced |
| Step 1b logs "next tick will restart" but nothing does | Reproduced |
| Updates lack a compare-and-swap and overwrite concurrent deploys | Reproduced |
| `isNotFoundError` matches any error containing `404`, including container IDs | Confirmed |
| A failed lease-expiry stop is never retried | Reproduced |

Fix (decided): write the state machine at the top of `reconciler.go`.
Count crashes from Docker's `RestartCount`
(delta since the last tick, stored per service) and keep scanning
`crashed` rows; trip the breaker when the count crosses the threshold within
the window. Replace step 1a's stop with a restart, or remove 1a and let
Docker's health status drive restarts. Make 1b restart or change the message.
Add `AND status = ? AND container_id = ?` to every update and skip the Docker
action when `RowsAffected == 0`. Replace `isNotFoundError` (here and in
`services.go`) with `errdefs.IsNotFound`. Stop lease-expired containers before
updating the row, and retry rows whose container still runs.

This is reconciler and circuit-breaker logic, so it needs the security
review before merge.

Test: a multi-tick crash loop trips the breaker. A deploy that completes
during a tick is not overwritten. A deadline error whose URL contains `404`
is not treated as not-found.

### W2-C: databases and kanban provisioning (`internal/databases/`, `internal/kanbans/`, `RunDatabase` in `internal/docker/client.go`)

| Finding | Status |
|---|---|
| Redis health check sends `PING` without auth; every Redis create fails | Confirmed |
| NULL `container_id` makes failed rows unreadable and undeletable; tenant can never re-create a kanban | Reproduced |
| `ReconcileStale` never clears rows stuck in `provisioning` | Reproduced |
| Reactivation never restarts databases or kanban | Reproduced |
| `RunDatabase` has no limits, no CapDrop, shared default bridge, never pulls the image | Plausible |
| Kanban is marked `ready` even if admin setup failed | Plausible |

Fix: send `AUTH <password>` then `PING` (or treat `-NOAUTH` as up). Scan
`container_id` with `COALESCE(container_id, '')`. Let `Create` replace a
`failed` kanban row. Make `ReconcileStale` mark old `provisioning` rows
failed. Add a `StartAllForTenant` to both packages and call it from the
reactivate handler. Put database containers on the tenant's internal network
with the same caps and limits as `RunContainer`, and pull the image first.

Test: real-protocol Redis test (a fake that enforces `requirepass`), NULL row
Get/Delete, failed kanban re-create, suspend then reactivate restarts both.

### W2-D: metering writes data (`internal/metering/`, collector wiring in `cmd/ah/main.go`)

| Finding | Status |
|---|---|
| Nothing writes `usage_events` or `usage_daily`; endpoints always return `[]` | Confirmed |
| `memory_mb_avg` returns a sum | Reproduced |
| `AH_METER_DB_PATH` is never used | Reproduced |

Fix (decided: build the collector): sample `ContainerStats` for each
running service every 60s, insert into `usage_events`, roll up daily. Fix the
average. Wire `MeterDBPath` through `db.Open`.

Test: collector tick with a mock Docker client inserts one row per running
service; the daily endpoint returns the right average.

### W2-E: CLI matches the server (`cmd/ahc/`, `internal/ahclient/`)

| Finding | Status |
|---|---|
| `db provision` waits for `running`; server uses `ready` | Confirmed |
| `deploy` stops only on `error`/`crashed`; server uses `failed`; git deploy ignores build failure | Confirmed |
| `register`/`recover --save` writes an empty URL; `--json --save` saves nothing | Reproduced |
| Env vars replace the config file all-or-nothing; load errors swallowed | Reproduced |
| Fixed 30s HTTP timeout cuts off exec and slow creates | Reproduced |
| `status` exit code misses `crashed`, `circuit_open`, degraded health, full disk | Reproduced |
| `db provision --json` prints non-JSON and skips linking | Reproduced |
| Config save keeps loose permissions on an existing file; write not atomic | Reproduced |
| Exec exit code not propagated; `build list --json` ignored; `service create` default image missing | Reproduced |
| Trailing slash in URL gives `//v1` and 404 | Reproduced |

Fix all in one PR. Share status constants with the server where practical
(export them from a small package both sides import) so the two cannot drift
again. Replace the CLI test fixtures that use invented statuses.

## Wave 3: races and resource leaks

Run after Wave 2, because these touch the same files.

### W3-B: cancellation actually stops work (`internal/builds/`, `internal/builder/`, `internal/deployments/`)

| Finding | Status |
|---|---|
| Cancelled build still deploys; cancel can overwrite `succeeded` | Reproduced |
| Deployment cancel is DB-only and is overwritten when the deploy finishes | Reproduced |
| `StreamBuildLogs` can hang when the build finishes during subscribe | Reproduced |
| A single output line over 256KB hangs the build | Reproduced |
| Per-line `UPDATE log = log || ?` makes logging quadratic | Reproduced |

Fix: give each build a cancellable context stored by build ID; cancel it
from `CancelBuild`; re-check status before `deployFn`. Guard cancel and
status updates with `AND status IN (...)`. Subscribe before reading the log
and unsubscribe on return. Keep draining the pipe after `ErrTooLong`. Buffer
log lines and flush every second or every 64KB.

### W3-C: deploy locking (`internal/services/deploy_image.go`, `internal/middleware/idempotency.go`)

| Finding | Status |
|---|---|
| `DeployImage` skips `lockService` and the circuit breaker | Reproduced |
| Concurrent same-key idempotent requests both run | Reproduced |
| Idempotency silently stops storing after 50 keys per tenant | Reproduced |

Fix: take `lockService` in `DeployImage` in the same order as `Restart`;
check `CircuitOpen`. Store a pending idempotency entry before calling the
handler; concurrent requests with the same key wait or get 409. Evict the
oldest entry at the cap.

### W3-D: environments and warm pool (`internal/environments/`, `RunEnvironment`/`ExecRun` in `internal/docker/client.go`, `internal/gc/`)

| Finding | Status |
|---|---|
| Claimed pool containers keep pool labels and name: GC removes them, previews break, suspend misses them | Reproduced |
| `refill` holds the pool mutex across Docker calls; `Acquire` blocks for the whole refill | Reproduced |
| Exec timeout not enforced (`sleep infinity` runs forever) | Reproduced |
| Tenant suspend leaves environments and previews running; cleanup uses the 30s request context | Confirmed by code path |
| Preview hostnames can collide with service DNS labels | Plausible |
| No cap on previews per environment; labels can exceed 63 chars | Plausible |
| `EgressAllow` is never applied, so `/sync` cannot reach the internet | Plausible |
| `ConnectNetwork` failure leaks a claimed container; any `Inspect` error deletes pool rows | Plausible |

Fix: on claim, rename and relabel the container to the tenant's environment
identity. Release the pool mutex during Docker calls. Close the hijacked exec
connection on `ctx.Done()` and set `TimedOut`. Add an environments stop step to
tenant suspend, run on a detached context with its own timeout. Reserve the
`env-` prefix for service DNS labels. Cap previews and validate label length.
Decide whether `EgressAllow` should be wired or removed.

### W3-E: GC and disk growth (`internal/gc/`, `RemoveContainer`/`PullImage` in `internal/docker/client.go`)

| Finding | Status |
|---|---|
| Tenant networks removed with no age gate, including while a deploy pulls its image | Reproduced |
| Images and anonymous volumes are never collected | Reproduced |
| `PullImage` ignores stream errors and the timeout | Reproduced |

Fix: skip networks younger than 15 minutes and networks referenced by any
container (running or stopped). Set `RemoveVolumes: true`. Remove superseded
tenant images on redeploy and delete. Decode the pull stream and return its
error.

## Wave 4: multi-tenant abuse (moved from Wave 1)

These need a second, hostile tenant. Do them before opening registration.

### W4-A: per-tenant cap on long-running requests (`internal/api/server.go`)

| Finding | Status |
|---|---|
| `Throttle(200)` is global and pre-auth; 200 slow `exec` calls from one tenant make the whole API return 429 | Plausible |

Fix: move long-lived routes (exec, log follow, provisioning) out of the
global throttle and give them a per-tenant in-flight cap (for example 4).

Test: tenant B still gets 200 while tenant A holds its maximum number of
exec slots.

### W4-B: separate operator token from registration token (`internal/api/recovery.go`, `internal/api/tenants.go`, `internal/api/bootstrap.go`, `internal/config/`)

| Finding | Status |
|---|---|
| The registration token also authorizes recover, reactivate and quota update for any tenant | Plausible (depends on how the token is shared) |
| Invalid requests consume the global 20/hour budget, so anonymous junk blocks registration and recovery | Plausible |
| First 8 chars of a rejected token are logged (`cmd/ah/main.go:96`) | Reproduced |

Fix: add `AH_ADMIN_TOKEN` for recover, reactivate and quota update. Keep the
bootstrap token for registration only. Charge the global bucket only after
the token validates; keep the per-IP bucket for all requests. Log only token
length. This is bootstrap-token logic, so it needs the security review.

Compatibility: if `AH_ADMIN_TOKEN` is unset, log a startup warning and fall
back to the current behavior for one release, then remove the fallback.
Document the new variable in the ops docs and CHANGELOG.

Test: recover with a bootstrap token returns 403 when an admin token is set.
Twenty invalid-token requests do not block a valid registration.

### W4-C: quota checks inside the insert transaction (services, databases, environments)

| Finding | Status |
|---|---|
| Service, database and environment quotas are checked outside the insert transaction | Reproduced |

Fix: count and insert inside one `BEGIN IMMEDIATE` transaction in each
package. Test: 8 concurrent creates against a file DB with
`max_*=1` create exactly one.

## Wave 5: hardening and cleanup

Low-risk items. Can run as one or two PRs at any point after Wave 2; the script deletion can go first.

- Rate limiter data race on `lastSeen`; use `GetOrSet` for limiter creation (`internal/middleware/ratelimit.go`).
- Enforce the per-tenant `api_rate_limit` quota or remove it from the API.
- Revoked key can be re-cached by an in-flight request (`internal/middleware/auth.go`); add a generation counter.
- Health `?fresh=true`: ignore for tenant keys or add singleflight plus a minimum interval; redact host-wide fields for tenants.
- Health disk paths hard-coded to `/var/lib/ah` and `/var/lib/docker`; use config.
- `ValidateImage` accepts `localhost/...` and other tenants' local-registry paths; require the caller's tenant prefix.
- Install script: verify `checksums.txt`, wrap in `main`, fix the printed `register` syntax; add a goreleaser release workflow.
- `deploy/ah.service`: `ReadWritePaths=/etc/traefik/dynamic` (directory). Verify on the server with a test deploy.
- Delete `scripts/deploy.sh`, `db-provision.sh`, `status.sh` and `logs.sh` (decided); `ahc` covers them. Update docs that mention them.

## Decisions (2026-10-14)

1. W1-B: store a dedicated API-key hashing key encrypted under the master key.
2. Bootstrap token: only the operator holds it, so the admin-token split (W4-B) moves to Wave 4.
3. W2-D: build the metering collector.
4. W2-B: count crashes from Docker's restart count; Docker keeps restarting containers.
5. Wave 5: delete the shell scripts in `scripts/`.

## Suggested order and size

| Wave | PRs | Parallel | Notes |
|---|---|---|---|
| 1 | W1-A to W1-D | yes | W1-B needs the security review |
| 2 | W2-A to W2-E | yes (merge W2-A before W2-B) | W2-B needs the security review |
| 3 | W3-B to W3-E | yes after Wave 2 merges | |
| 4 | W4-A to W4-C | yes | Before opening registration to others; W4-B needs the security review |
| 5 | 1–2 PRs | yes | Script deletion can ship any time |

Each workstream is sized for one implementation subagent and one PR.
