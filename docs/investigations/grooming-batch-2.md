# GitHub Issues Grooming — Batch 2

**Date**: 2026-03-21
**Issues**: #107, #106, #105, #104, #103, #102

---

## #107 — feat(tenant): implement tenant reactivation after suspension

**Status**: NOT IMPLEMENTED (candidate blocker identified)

### Evidence
- `internal/api/tenants.go:354–414` — `handleTenantDelete()` marks tenant as `'suspended'`, revokes API keys, and stops containers
- `internal/api/recovery.go:82–99` — Key recovery endpoint checks `tenantStatus != "active"` and rejects suspended tenants with HTTP 403
- Suspension is one-way: no reactivation endpoint exists
- `CHANGELOG.md` mentions "Databases and kanbans stopped when tenant is suspended" (v0.4.0) but no reactivation

### Evaluation

**Clarity**: Clear. Suspend exists; reactivate is the inverse operation.

**Acceptance Criteria** (needs definition):
- [ ] POST /v1/tenants/reactivate endpoint (or similar) that changes status from 'suspended' → 'active'
- [ ] Restart all stopped services, databases, kanbans for the tenant
- [ ] Clear DB and container cleanup before restart (idempotent)
- [ ] Only allowed if tenant email+bootstrap-token match (same security model as recovery)
- [ ] Test: reactivate → containers are running; services accessible

**Scope**: Medium. Mirrors suspension logic but in reverse.
- Clean up any orphaned containers before restart
- Validate tenant still has quota for restarting resources
- Confirm API key recovery still works after reactivation

**Blockers**: 
- **API design question**: Should it be a separate endpoint (`POST /v1/tenants/reactivate`) or a PATCH/PUT to set status? 
  - Recommend separate endpoint for semantic clarity (like service restart is separate from start)
- **Quota validation**: After suspension, quotas may have changed. Should we auto-adjust resource limits?

**Labels**: `enhancement`, `tenant-mgmt`, `security`

**Effort**: ~2–3 hours (logic mirrors delete; main work is testing cleanup and validation)

---

## #106 — enhancement(builds): preserve final N lines of build logs when 5MB cap is reached

**Status**: PARTIALLY IMPLEMENTED (cap exists, truncation needs refinement)

### Evidence
- `internal/builds/builds.go:23` — `const maxLogSize = 5 * 1024 * 1024` (5MB limit defined)
- `internal/builds/builds.go:300–318` — `appendLog()` silently drops lines after 5MB is reached
  ```go
  if currentSize+lineBytes > maxLogSize {
      m.logMu.Unlock()
      return // silently drop — log is full
  }
  ```
- Current behavior: **stops logging when cap is hit** (no preservation of final lines)
- Issue requests: preserve last N lines (e.g., last 100 lines of build output) instead of stopping mid-stream

### Evaluation

**Clarity**: Very clear. Describes exact issue: current silent drop loses valuable tail of output.

**Acceptance Criteria**:
- [ ] When log reaches 5MB, truncate from the middle (keep head + tail)
- [ ] Minimum final N lines to preserve (recommend 100–200 lines)
- [ ] Add marker line: `[ah] Log truncated: kept first X lines and last Y lines`
- [ ] Total log size still respects 5MB cap
- [ ] Test: build with large output → verify final lines are present

**Scope**: Small. Localized to `appendLog()` and log retrieval.
- Ring buffer or dual-buffer strategy (keep head + tail)
- Update `StreamBuildLogs()` to handle marker line

**Blockers**: None technical. Design question:
- **Tail size**: How many final lines? (100? 500? configurable?)
- **Head truncation**: Do we discard middle or oldest lines?

**Labels**: `enhancement`, `builds`, `ux`

**Effort**: ~1–2 hours (straightforward ring-buffer logic)

---

## #105 — feat(health): include Docker storage usage in detailed health endpoint

**Status**: NOT IMPLEMENTED

### Evidence
- `internal/api/health.go:20–41` — `DetailedHealthResponse` struct has:
  ```go
  type DetailedHealthResponse struct {
      Status string     `json:"status"`
      Docker DockerInfo `json:"docker"`      // Only Version available
      GVisor GVisorInfo `json:"gvisor"`
      Disk   DiskInfo   `json:"disk"`         // Filesystem usage, not Docker
  }
  ```
- `DockerInfo` only includes `Available` and `Version` — no storage metrics
- `DiskInfo` reports `/var/lib/ah` filesystem stats (total, free, used %)
- Docker data dir (`/var/lib/docker`) is not reported separately

### Evaluation

**Clarity**: Clear. Docker storage != filesystem storage. Need Docker-specific stats.

**Acceptance Criteria**:
- [ ] Extend `DockerInfo` with storage fields: `UsedGB`, `TotalGB`, `UsedPercent` (match `DiskInfo` format)
- [ ] Query Docker API (`docker system df`) or parse `/var/lib/docker` directly
- [ ] Return only Docker-managed storage (images, containers, volumes)
- [ ] Cache result like other health checks (30s TTL)
- [ ] Test: create containers/images → verify usage increases

**Scope**: Small. Isolated to health endpoint.
- Add new `docker system df` call with timeout
- Parse output or use Docker API stats endpoint

**Blockers**: None. Standard Docker API call.

**Labels**: `feature`, `health`, `observability`

**Effort**: ~1 hour (straightforward Docker API call)

---

## #104 — fix(databases): add name validation rules for database names

**Status**: PARTIALLY IMPLEMENTED (basic length check only)

### Evidence
- `internal/databases/databases.go:109–115` — `Create()` validation:
  ```go
  if req.Name == "" || len(req.Name) > 128 {
      return nil, apierr.Validation("name is required (max 128 chars)")
  }
  ```
- **That's it.** No regex, no character set rules, no reserved-name checks.
- Postgres requires database names to be valid identifiers (alphanumeric + underscore, no leading digit)
- Redis is more lenient but should still follow conventions

### Evaluation

**Clarity**: Clear. Add validation to prevent invalid database names downstream.

**Acceptance Criteria**:
- [ ] Postgres: require valid SQL identifier (pattern: `^[a-zA-Z_][a-zA-Z0-9_]*$`)
- [ ] Redis: same pattern (optional but recommended for consistency)
- [ ] Reject reserved names (`postgres`, `template0`, `template1` for Postgres)
- [ ] Reject names with leading digits or special characters
- [ ] Error message lists allowed patterns
- [ ] Test: invalid names rejected; valid names accepted

**Scope**: Very small. Single validation function.
- Add regex pattern
- Check reserved names list
- Update error message

**Blockers**: None. Design question:
- **Type-aware validation**: Different rules for Postgres vs Redis?
  - Recommend: same rules for both (simplicity)

**Labels**: `fix`, `databases`, `validation`

**Effort**: ~30 minutes (simple regex + reserved-name list)

---

## #103 — feat(snapshots): implement snapshot retention policies and automatic cleanup

**Status**: NOT IMPLEMENTED

### Evidence
- `internal/snapshots/snapshots.go:1–321` — Only CRUD operations exist:
  - `Create()` — no retention policy
  - `List()` — paginated listing
  - `Get()` — retrieves by ID
  - `Delete()` — manual deletion only
  - `RestoreEnvVars()` — utility
- **No automatic cleanup**, no retention policy configuration, no background job
- Schema created in `state_010_snapshots.sql` has no `expires_at`, `retention_days`, or similar

### Evaluation

**Clarity**: Clear. Need policies to auto-delete old snapshots (storage management).

**Acceptance Criteria**:
- [ ] Tenant-level policy: `max_snapshots_per_service` and/or `snapshot_retention_days`
- [ ] Default policy: keep last 10 per service, auto-delete after 30 days (configurable)
- [ ] Reconciler or background job to run cleanup (e.g., hourly)
- [ ] When cleanup deletes: tag Docker image, delete DB record, clean up tagged image
- [ ] Preserve manually-pinned snapshots (add `pinned` flag?)
- [ ] API: GET snapshots shows expiration date; DELETE forces removal before policy
- [ ] Test: create 15 snapshots → verify excess auto-deleted; old snapshots removed after 30d

**Scope**: Medium. Requires:
- Schema changes (add `expires_at`, `pinned` columns)
- Cleanup job (in reconciler or separate background task)
- API field additions
- Docker cleanup logic

**Blockers**: 
- **Storage quota**: Snapshots are tagged Docker images. Is storage counted against tenant quota?
  - Likely yes (they consume registry space)
- **Granularity**: Per-service or per-tenant policy?
  - Recommend: configurable per-tenant, applied globally to all snapshots

**Labels**: `feature`, `snapshots`, `storage-mgmt`

**Effort**: ~3–4 hours (schema migration, cleanup job, API changes, testing)

---

## #102 — enhancement(kanban): make kanban provisioning fully async like database provisioning

**Status**: NOT IMPLEMENTED (currently synchronous)

### Evidence
- `internal/api/kanbans.go:19–32` — `handleKanbanCreate()` calls `s.kanbanManager.Create()` and returns result synchronously:
  ```go
  kb, err := s.kanbanManager.Create(r.Context(), tenantID)
  if err != nil {
      apierr.WriteAPIError(w, err)
      return
  }
  writeJSON(w, http.StatusCreated, kb)
  ```
- `internal/kanbans/kanbans.go:127–339` — `Create()` **blocks the entire request** for:
  - Volume creation (~1s)
  - Container startup and health check (~5–30s for Vikunja to boot)
  - Setup (create admin user, project, buckets) (~2–5s)
  - **Total: 10–40 seconds** synchronously
- Databases, by contrast, are async: `Create()` returns immediately with `status: 'provisioning'`, then background goroutine completes provisioning

### Evaluation

**Clarity**: Very clear. Kanban provisioning is blocking; databases are non-blocking. Inconsistency.

**Acceptance Criteria**:
- [ ] `Create()` returns immediately with `status: 'provisioning'` (like databases)
- [ ] Background goroutine completes volume, container, health check, setup
- [ ] GET endpoint shows current status
- [ ] On startup, reconcile stale kanbans (already done: `ReconcileStale()`)
- [ ] Client polls status until `'ready'` (same pattern as databases)
- [ ] Error handling: if background provisioning fails, status → `'failed'` (like databases)
- [ ] Test: create kanban → returns instantly; status transitions provisioning → ready

**Scope**: Medium. Mirrors database async pattern.
- Move health check + setup into background goroutine
- Return provisioning status immediately
- Add error handling in background task
- Update API docs to match database pattern

**Blockers**: None technical. Straightforward refactoring.

**Labels**: `enhancement`, `kanban`, `api-consistency`

**Effort**: ~2–3 hours (refactor provisioning logic into background task; test polling)

---

## Summary Table

| Issue | Status | Effort | Blockers | Priority |
|-------|--------|--------|----------|----------|
| #107 | Not Impl | 2–3h | API design question | Medium |
| #106 | Partial | 1–2h | Tail size decision | Medium |
| #105 | Not Impl | 1h | None | Low |
| #104 | Partial | 30min | Type-aware rules? | Low |
| #103 | Not Impl | 3–4h | Storage quota model | Medium |
| #102 | Not Impl | 2–3h | None | High (consistency) |

### Recommended Priority
1. **#102** (async kanban) — consistency with databases, straightforward
2. **#107** (reactivation) — completes suspend/delete feature
3. **#103** (retention) — storage management (defer if not urgent)
4. **#106** (tail preservation) — improves build observability
5. **#105** (Docker storage) — nice-to-have for health endpoint
6. **#104** (name validation) — low-risk, easy fix

---

**Next Steps**:
1. Design decision: #107 endpoint shape, #106 tail size, #103 policy model
2. Create detailed issues with acceptance criteria and blockers
3. Tag for sprint assignment
