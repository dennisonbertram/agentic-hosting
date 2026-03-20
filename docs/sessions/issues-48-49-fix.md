# Session: Fix #48 and #49 — Restart container recreation + stale container_id

**Date:** 2026-03-19
**Commit:** e606dd5
**Branch:** main (pushed)

---

## What was planned

Fix two pre-existing bugs reported as issues:

- **#48**: `Restart()` called stop+start, but Docker container env is immutable after creation. Environment variable changes set via `POST /env` were never applied on restart.
- **#49**: `ResetCircuitBreaker()` stopped and removed the container but left `container_id` in DB. Subsequent `Start()` calls failed with "No such container". Same problem in `StopAllForTenant()`.

Then run a 3-pass Ralph Loop review and commit when all 3 pass.

---

## What was done

### Core bug fixes

**Fix #48 — Restart recreates container:**
- Rewrote `Restart()` from a simple stop+start to a full container recreation:
  1. Stop and remove old container (by ID and fallback by deterministic name)
  2. Clear `container_id` in DB before `RunContainer` (prevents DB pointing at removed container on failure)
  3. Load fresh env vars from DB via `getEnvVars()`
  4. `EnsureNetwork` (mirrors Deploy behaviour; guards tenant isolation)
  5. `RunContainer` with current image, current env, current port
  6. Update `container_id` and `status='running'` in DB
  7. Rewrite Traefik route
- Updated API response note: "restart will recreate the container with the new env vars"

**Fix #49 — Clear stale container_id:**
- `ResetCircuitBreaker()`: now includes `container_id = ''` in the DB reset UPDATE. Also added fallback cleanup-by-name, Traefik route removal, `checkTenantActive`, and `RowsAffected` check.
- `StopAllForTenant()`: after successful stop/remove, now clears `container_id` in DB using a CAS update (`WHERE container_id = ?`). Also treats "not found" from stop/remove as success (handles label-based pre-cleanup double-removal).
- `Start()`: detects Docker 404, clears stale `container_id` via CAS update, returns clear error "container no longer exists — deploy the service to start it".

### Additional hardening found during Ralph Loop review

The Ralph Loop ran 10+ iterations across 3 reviewer personas and found many valid issues beyond the original scope that were fixed as part of the PR:

- **Striped per-service locks** (256-slot `[256]sync.Mutex`) to serialise concurrent lifecycle ops. All destructive operations — Deploy, Restart, ResetCircuitBreaker, Delete, Start, Stop — now use the same lock.
- **Lock ordering discipline**: `deployQueue → deploySem → lockService` consistently across all operations (prevents deadlock).
- **Post-lock state re-read** (TOCTOU fix): Restart, Reset, Start, Stop all re-read `getOwned()` after acquiring the lock to get authoritative state.
- **Detached context** (`context.WithoutCancel`): Restart and Reset use a 2-minute detached context so client disconnects don't abort critical cleanup mid-flight.
- **Deploy backpressure for Restart and Reset**: both now participate in `deployQueue`/`deploySem` so they can't bypass rate limiting.
- **Disk check in Restart**: mirrors Deploy's `diskcheck.CheckAll`.
- **`cidShort()` helper**: prevents panic on container IDs shorter than 12 chars.
- **`isAlreadyStoppedError()` helper**: Restart and Stop now handle "container is not running" as non-fatal.
- **Deploy teardown**: stop/remove errors are logged but non-fatal; container_id is cleared in DB after removing old container before `RunContainer`.
- **Stop() graceful handling**: treats 404 and "already stopped" gracefully, with CAS clear on 404.
- **Unscoped status update fix**: `Deploy`'s `checkTenantActive` error path was using `updateStatusWithError` (no `tenant_id`) — changed to `updateStatusWithErrorScoped`.
- **StopAllForTenant CAS**: DB update uses `WHERE container_id = ?` to prevent clobbering a newer container_id from concurrent Deploy.
- **Fallback name-cleanup verification**: both Restart and Reset verify container absence via Inspect before clearing DB when `StopAndRemoveByName` fails with a non-404 error.
- **Tenant active re-check in Restart** before RunContainer (tenant could be suspended while teardown is in progress).

---

## Ralph Loop results

**Total iterations:** 13 pass attempts across 3 personas

**Final approved review files:**
- `code-reviews/issues-pass1-20260319-151458.md` — Adversarial: CRITICAL: 0, HIGH: 0, MEDIUM: 0, APPROVED: YES
- `code-reviews/issues-pass2-20260319-151811.md` — Skeptical user: CRITICAL: 0, HIGH: 0, MEDIUM: 2, APPROVED: YES
- `code-reviews/issues-pass3-20260319-151953.md` — Correctness: CRITICAL: 0, HIGH: 0, MEDIUM: 2, APPROVED: YES

**Notable issues found and fixed by Ralph Loop (in order of discovery):**
1. Container ID slice panic (`[:12]`) — added `cidShort()` helper
2. `sync.Map` unbounded growth — replaced with `[256]sync.Mutex` striped array
3. Missing serialization (Deploy vs Restart/Reset) — added `lockService` to all destructive ops
4. `ResetCircuitBreaker` container removal not verified — added fail-closed Inspect check
5. TOCTOU on `getOwned` before lock in Restart/Reset/Start/Stop — re-read under lock
6. Missing `EnsureNetwork` in Restart — added
7. Detached context needed for cleanup — added `context.WithoutCancel`
8. Lock ordering deadlock (Deploy: sem→lock vs Restart: lock→sem) — standardised to `sem→lock`
9. `StopAllForTenant` not-found not treated as success — fixed
10. StopAllForTenant DB update not CAS — fixed
11. Deploy teardown errors ignored, container_id not cleared — fixed
12. Stop() crashes on 404 — fixed with CAS clear

---

## What was left incomplete

- **Reconciler integration**: the crash monitor/reconciler (separate file) should also use `lockService` or CAS DB updates when updating `crash_count`/`circuit_open`. Flagged in review as out-of-scope for this PR — tracked for future work.
- **Per-tenant rate limiting**: Restart and Reset share the global deploy semaphore (5 slots). Per-tenant limits would further reduce DoS surface. Out-of-scope.
- **No explicit `/redeploy` endpoint**: after a circuit breaker reset or failed restart, users must delete + recreate the service (or use a snapshot). A dedicated `/redeploy` endpoint calling `Deploy()` would improve UX.
- **`updateStatus` / `updateStatusWithError` not tenant-scoped** in some internal paths: these helpers update by `id` only. Service IDs are random 32-hex so collision probability is negligible, but consistency improvement is tracked.

---

## Files changed

| File | Changes |
|---|---|
| `internal/services/services.go` | +428 / -29 — all lifecycle method rewrites |
| `internal/api/services.go` | +1 / -1 — updated env-set response note |
