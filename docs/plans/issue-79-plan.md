# Issue #79: Dedicated Deployments Table - Implementation Plan

**Issue**: feat(deployments): implement dedicated deployments table for full deployment history
**Date**: 2026-03-21
**Status**: Plan (no code changes)

---

## 1. Summary of Current Behavior

### Current Deployments Endpoint

`GET /v1/services/{serviceID}/deployments` is handled by `handleServiceDeployments` in
`internal/api/services.go` (lines 443-472). It returns a **single** `DeploymentRecord`
derived on-the-fly from the service row:

```go
type DeploymentRecord struct {
    ID          string `json:"id"`
    ServiceID   string `json:"service_id"`
    Status      string `json:"status"`
    Image       string `json:"image"`
    StartedAt   int64  `json:"started_at"`
    ContainerID string `json:"container_id,omitempty"`
}
```

The record's ID is synthesized as `"deploy-" + svc.ID + "-" + svc.UpdatedAt` and its
`StartedAt` is set to the service's `updated_at` timestamp. There is no historical data --
the response always contains at most one record reflecting the service's current state.

### Where Deployments Actually Happen

There are **five distinct code paths** that create/rotate containers (i.e., constitute a
"deployment"):

| Code Path | File | Trigger | Description |
|---|---|---|---|
| `Manager.Deploy` | `internal/services/services.go:531` | `POST /services` (async goroutine), manual `POST /redeploy` via Restart | Pulls image, tears down old container, creates new container |
| `Manager.DeployImage` | `internal/services/deploy_image.go:16` | Build completion callback (`builds.DeployFunc`) | Deploys a pre-built nixpacks image without pull |
| `Manager.Restart` | `internal/services/services.go:792` | `POST /restart`, `POST /redeploy` | Stops+removes old container, creates new container with current env vars |
| `Manager.Start` | `internal/services/services.go:745` | `POST /start` | Starts an existing stopped container (no new container created) |
| `Manager.Stop` | `internal/services/services.go:701` | `POST /stop` | Stops running container |

Additionally, the **reconciler** (`internal/reconciler/reconciler.go`) can mark services as
`crashed` or `failed` when it detects container disappearance or stale deploys, and can
auto-recover circuits. These status transitions should also be recorded.

### Current Limitations

1. **No history**: Only the latest state is visible; all previous deploys are lost.
2. **No trigger attribution**: No way to know if a deploy was manual, from a build, or from
   a restart.
3. **No timing data**: No `completed_at` to measure deploy duration.
4. **No error capture**: Deploy failures are logged to stdout but not persisted in a
   queryable table.
5. **Compliance gap**: STORY-148 requires full audit trails of deployment events.

---

## 2. New Migration Spec

### Migration File

**Name**: `state_013_deployments.sql`

The latest existing migrations are `state_012_service_url.sql`. Note: there are two
`state_010_*` files (`kanbans` and `snapshots`) which is an existing naming anomaly. The
new migration should be `state_013_deployments.sql` to sequence after `012`.

### Schema

```sql
CREATE TABLE IF NOT EXISTS deployments (
  id TEXT PRIMARY KEY,
  service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL,
  build_id TEXT DEFAULT NULL,           -- FK to builds(id) if triggered by a build
  image TEXT NOT NULL,                  -- image ref at time of deploy
  status TEXT NOT NULL DEFAULT 'pending', -- pending, deploying, running, failed, crashed, stopped
  trigger TEXT NOT NULL DEFAULT 'manual', -- manual, build, restart, reconciler, auto_recovery
  container_id TEXT DEFAULT '',         -- Docker container ID once running
  error_message TEXT DEFAULT '',        -- error details if status=failed/crashed
  started_at INTEGER NOT NULL,          -- unix timestamp when deploy was initiated
  completed_at INTEGER,                 -- unix timestamp when deploy finished (success or failure)
  created_at INTEGER NOT NULL           -- row creation timestamp
);

CREATE INDEX IF NOT EXISTS idx_deployments_service ON deployments(service_id);
CREATE INDEX IF NOT EXISTS idx_deployments_tenant ON deployments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_deployments_service_created ON deployments(service_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status);
```

### Column Rationale

| Column | Why |
|---|---|
| `id` | Random hex ID (same `generateID()` pattern as services/builds, 32 hex chars) |
| `service_id` | FK with CASCADE delete -- when a service is deleted, its deployments are deleted |
| `tenant_id` | Denormalized for efficient tenant-scoped queries without JOIN |
| `build_id` | Nullable FK to `builds(id)` -- links build-triggered deploys to their build record |
| `image` | Captured at deploy time so historical records preserve the exact image deployed |
| `status` | Lifecycle: `pending` -> `deploying` -> `running` / `failed` / `crashed` / `stopped` |
| `trigger` | Enum-like text: `manual` (API call), `build` (nixpacks), `restart`, `reconciler`, `auto_recovery` |
| `container_id` | The Docker container ID once the container is running |
| `error_message` | Captures the error string for failed/crashed deploys |
| `started_at` | When the deploy was initiated (distinct from `created_at` for queued scenarios) |
| `completed_at` | When the deploy reached a terminal state; NULL while in progress |
| `created_at` | Row insertion timestamp |

### Why No `REFERENCES builds(id)` FK on `build_id`

The `build_id` column intentionally omits a formal `REFERENCES builds(id)` constraint.
Builds can be deleted or cleaned up independently of deployment history. A soft reference
(application-level join) preserves the deployment record even if the build is purged.

---

## 3. New `internal/deployments/` Package Design

### File: `internal/deployments/deployments.go`

```
internal/deployments/
  deployments.go      -- Deployment model, Store, CRUD operations
  deployments_test.go -- Unit tests
```

### Model

```go
// Deployment represents a single deployment event for a service.
type Deployment struct {
    ID           string `json:"id"`
    ServiceID    string `json:"service_id"`
    TenantID     string `json:"tenant_id"`
    BuildID      string `json:"build_id,omitempty"`
    Image        string `json:"image"`
    Status       string `json:"status"`
    Trigger      string `json:"trigger"`
    ContainerID  string `json:"container_id,omitempty"`
    ErrorMessage string `json:"error_message,omitempty"`
    StartedAt    int64  `json:"started_at"`
    CompletedAt  *int64 `json:"completed_at,omitempty"`
    CreatedAt    int64  `json:"created_at"`
}
```

### Trigger Constants

```go
const (
    TriggerManual       = "manual"        // User-initiated via API (POST /services, POST /redeploy)
    TriggerBuild        = "build"         // Triggered by build completion (builds.DeployFunc)
    TriggerRestart      = "restart"       // POST /restart or POST /redeploy
    TriggerReconciler   = "reconciler"    // Status change from reconciler (crash detection, timeout)
    TriggerAutoRecovery = "auto_recovery" // Circuit breaker auto-recovery
)
```

### Status Constants

```go
const (
    StatusPending   = "pending"
    StatusDeploying = "deploying"
    StatusRunning   = "running"
    StatusFailed    = "failed"
    StatusCrashed   = "crashed"
    StatusStopped   = "stopped"
)
```

### Store Interface

```go
// Store handles deployment record persistence.
type Store struct {
    db *sql.DB
}

func NewStore(db *sql.DB) *Store

// Create inserts a new deployment record. Returns the created Deployment.
func (s *Store) Create(ctx context.Context, d *Deployment) error

// UpdateStatus transitions a deployment to a new status, optionally setting
// container_id, error_message, and completed_at.
func (s *Store) UpdateStatus(ctx context.Context, deploymentID, status string, opts ...UpdateOption) error

// Get retrieves a single deployment by ID, scoped to tenant.
func (s *Store) Get(ctx context.Context, tenantID, deploymentID string) (*Deployment, error)

// ListByService returns paginated deployments for a service, newest first.
func (s *Store) ListByService(ctx context.Context, tenantID, serviceID string, limit, offset int) ([]*Deployment, error)

// ListByTenant returns paginated deployments across all services for a tenant, newest first.
func (s *Store) ListByTenant(ctx context.Context, tenantID string, limit, offset int) ([]*Deployment, error)

// LatestForService returns the most recent deployment for a service (for dashboard).
func (s *Store) LatestForService(ctx context.Context, serviceID string) (*Deployment, error)
```

### UpdateOption Pattern

```go
type UpdateOption func(*updateOpts)
type updateOpts struct {
    containerID  *string
    errorMessage *string
    completedAt  *int64
}

func WithContainerID(cid string) UpdateOption
func WithError(msg string) UpdateOption
func WithCompletedAt(ts int64) UpdateOption
```

This keeps `UpdateStatus` calls clean at the call site:

```go
store.UpdateStatus(ctx, deployID, StatusRunning, WithContainerID(containerID), WithCompletedAt(now))
store.UpdateStatus(ctx, deployID, StatusFailed, WithError("image pull failed"), WithCompletedAt(now))
```

---

## 4. Updated API Handler Design

### Modified Handler: `handleServiceDeployments`

The existing handler in `internal/api/services.go` will be rewritten to query the new
`deployments` table instead of synthesizing a record from the service row.

```go
func (s *Server) handleServiceDeployments(w http.ResponseWriter, r *http.Request) {
    // Unchanged: requireSvcManager, tenantID, serviceID extraction
    // Unchanged: verify service exists via svcManager.Get (tenant-scoped)

    limit, offset, err := parsePagination(r)  // existing helper
    if err != nil {
        writeError(w, http.StatusBadRequest, err.Error())
        return
    }

    deployments, err := s.deploymentStore.ListByService(r.Context(), tenantID, serviceID, limit, offset)
    // ...
    writeJSON(w, http.StatusOK, deployments)
}
```

### New Dependency: `deploymentStore`

The `Server` struct in `internal/api/server.go` needs a new field:

```go
type Server struct {
    // ... existing fields ...
    deploymentStore *deployments.Store
}
```

This is injected via `ServerConfig` the same way `svcManager`, `buildManager`, etc. are.

### Response Format (Backwards Compatible)

The new response is a **superset** of the old `DeploymentRecord`. All fields from the old
response are preserved with the same JSON keys. New fields are additive (not breaking per
the API versioning policy in CLAUDE.md).

Old response (single-element array):
```json
[{
  "id": "deploy-svc123-1679000000",
  "service_id": "svc123",
  "status": "running",
  "image": "nginx:latest",
  "started_at": 1679000000,
  "container_id": "abc123"
}]
```

New response (full paginated history):
```json
[{
  "id": "a1b2c3d4...",
  "service_id": "svc123",
  "tenant_id": "tenant-1",
  "build_id": "",
  "image": "nginx:latest",
  "status": "running",
  "trigger": "manual",
  "container_id": "abc123...",
  "error_message": "",
  "started_at": 1679000000,
  "completed_at": 1679000005,
  "created_at": 1679000000
}]
```

The old `DeploymentRecord` struct can be removed once the handler uses the new
`deployments.Deployment` model directly. This is an additive change (new fields in
response) and does NOT require an API version bump per the versioning policy.

---

## 5. Integration Points

### 5.1 Service Creation + Initial Deploy

**File**: `internal/api/services.go` - `handleServiceCreate` (line 28)
**File**: `internal/services/services.go` - `Manager.Deploy` (line 531)

**When**: `POST /v1/services` creates a service then spawns a goroutine calling
`svcManager.Deploy`.

**Integration**:
1. In `Manager.Deploy`, after `m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")`
   (line 577), insert a deployment record with status `"deploying"` and trigger `"manual"`.
2. On successful container start + DB update (line 654), update the deployment record to
   status `"running"` with the container ID and `completed_at`.
3. On any failure path (image pull, network setup, container start), update the deployment
   record to status `"failed"` with the error message and `completed_at`.

**Deployment record flow**:
```
Create(status=deploying, trigger=manual) -> UpdateStatus(running) OR UpdateStatus(failed)
```

### 5.2 Build-Triggered Deploy

**File**: `internal/services/deploy_image.go` - `Manager.DeployImage` (line 16)
**File**: `internal/builds/builds.go` - `Manager.runBuild` (line 219)

**When**: A build succeeds and calls `m.deployFn(ctx, tenantID, serviceID, imageTag)`.

**Integration**:
1. The `DeployFunc` signature needs to be extended to accept an optional `buildID`:
   ```go
   type DeployFunc func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error
   ```
   Alternatively, a `DeployImageOpts` struct can be introduced to avoid signature bloat.
2. In `DeployImage`, after updating service status to `"deploying"`, insert a deployment
   record with trigger `"build"` and the `build_id` set.
3. On success/failure, update the deployment record accordingly.

**Note**: The `builds.Manager.runBuild` (line 274) calls `m.deployFn` -- the build ID is
available in scope as `buildID`. Pass it through.

### 5.3 Restart / Redeploy

**File**: `internal/services/services.go` - `Manager.Restart` (line 792)
**File**: `internal/api/services.go` - `handleServiceRedeploy` calls `svcManager.Restart`

**When**: `POST /restart` or `POST /redeploy` is called.

**Integration**:
1. After acquiring the lock and reading service state, insert a deployment record with
   trigger `"restart"` and status `"deploying"`.
2. When the new container is successfully started and DB updated (line 955), update to
   `"running"`.
3. On failure, update to `"failed"`.

### 5.4 Stop

**File**: `internal/services/services.go` - `Manager.Stop` (line 701)

**When**: `POST /stop` is called.

**Integration**: Insert a deployment record with trigger `"manual"`, status `"stopped"`,
and `completed_at` set immediately. This records the stop event in the history.

### 5.5 Start (Existing Container)

**File**: `internal/services/services.go` - `Manager.Start` (line 745)

**When**: `POST /start` starts an existing stopped container.

**Integration**: Insert a deployment record with trigger `"manual"`, status `"running"`,
and `completed_at` set immediately. Records that the service was explicitly started.

### 5.6 Reconciler -- Crash Detection

**File**: `internal/reconciler/reconciler.go` - `reconcileOnce` (line 95)

**When**: The reconciler detects a container has disappeared or exited and marks the
service as `crashed`.

**Integration**:
1. After the `UPDATE services SET status = 'crashed'` statement (line 179), insert a
   deployment record with trigger `"reconciler"`, status `"crashed"`, and the crash reason
   as `error_message`.
2. The reconciler currently uses raw SQL against the `services` table. It will need a
   `*deployments.Store` injected via its constructor.

**Reconciler struct change**:
```go
type Reconciler struct {
    db          *sql.DB
    docker      docker.Client
    interval    time.Duration
    deployStore *deployments.Store  // NEW
}
```

### 5.7 Reconciler -- Stale Deploy Timeout

**File**: `internal/reconciler/reconciler.go` (line 343)

**When**: Services stuck in `"deploying"` for >10 minutes are marked `"failed"`.

**Integration**: After the bulk `UPDATE services SET status = 'failed'`, query the
affected service IDs and insert deployment records with trigger `"reconciler"`, status
`"failed"`, error `"deploy timed out (reconciler)"`.

### 5.8 Reconciler -- Circuit Breaker Auto-Recovery

**File**: `internal/reconciler/reconciler.go` (line 302)

**When**: A service's `circuit_retry_at` has elapsed and the circuit is reset.

**Integration**: Insert a deployment record with trigger `"auto_recovery"`, status
`"stopped"` (the service transitions to `stopped` before being restarted on the next
tick).

### 5.9 Service Deletion

**File**: `internal/services/services.go` - `Manager.Delete` (line 982)

**When**: `DELETE /services/{id}` is called.

**No explicit integration needed**: The `ON DELETE CASCADE` on the `service_id` FK will
automatically clean up all deployment records when a service is deleted.

---

## 6. Dependency Injection Path

The deployment store needs to flow from `main.go` through the system:

```
cmd/ah/main.go
  -> deployments.NewStore(store.StateDB)
  -> services.NewManager(..., deployStore)
  -> reconciler.New(..., deployStore)
  -> api.ServerConfig{DeploymentStore: deployStore}
```

The `services.Manager` needs the deployment store to insert records from
Deploy/DeployImage/Restart/Start/Stop. The reconciler needs it for crash/timeout/recovery
records.

---

## 7. Testing Strategy

### Unit Tests: `internal/deployments/deployments_test.go`

| Test | What It Verifies |
|---|---|
| `TestCreate_InsertsRecord` | Basic insert + read-back with all fields |
| `TestCreate_DuplicateID_Fails` | Primary key constraint |
| `TestUpdateStatus_Running` | Status transition + container_id + completed_at |
| `TestUpdateStatus_Failed` | Status transition + error_message + completed_at |
| `TestListByService_Pagination` | Correct ordering (newest first), limit/offset |
| `TestListByService_TenantIsolation` | Records from other tenants are excluded |
| `TestListByTenant_CrossService` | Returns deployments across all services for tenant |
| `TestLatestForService` | Returns only the most recent deployment |
| `TestCascadeDelete` | Deleting a service cascades to deployment records |

Use `testutil.NewStateDB(t)` for in-memory SQLite with real migrations.

### Integration Tests: `internal/api/server_test.go`

| Test | What It Verifies |
|---|---|
| `TestServiceDeployments_ReturnsPaginatedHistory` | Replace existing test; verify multiple records with pagination |
| `TestServiceDeployments_FilterByStatus` | Optional future filter param |
| `TestServiceDeployments_NotFound_Returns404` | Keep existing test |
| `TestServiceDeployments_EmptyHistory` | New service with no deploys returns `[]` |

### Reconciler Tests: `internal/reconciler/reconciler_test.go`

| Test | What It Verifies |
|---|---|
| `TestReconciler_CrashCreatesDeploymentRecord` | Crash detection inserts a deployment record |
| `TestReconciler_StaleDeployCreatesRecord` | Deploy timeout creates a failure record |
| `TestReconciler_CircuitRecoveryCreatesRecord` | Auto-recovery inserts a record |

---

## 8. Commit Plan

### Commit 1: `feat(db): add state_013_deployments migration`

**Files**:
- `internal/db/migrations/state_013_deployments.sql` (new)

**Scope**: Just the migration file. No Go code changes. Verifiable by running `go build ./...`
and `go test ./...` (migration will be applied to test DBs automatically via
`ApplyStateMigrations`).

### Commit 2: `feat(deployments): add deployments package with Store and model`

**Files**:
- `internal/deployments/deployments.go` (new)
- `internal/deployments/deployments_test.go` (new)

**Scope**: The standalone `deployments` package with `Deployment` model, `Store`,
and full unit test coverage. No integration with other packages yet. Can be
reviewed/tested independently.

### Commit 3: `feat(services): record deployment events in Deploy/DeployImage/Restart/Start/Stop`

**Files**:
- `internal/services/services.go` (modified -- Deploy, Start, Stop, Restart)
- `internal/services/deploy_image.go` (modified -- DeployImage)
- `internal/services/services_test.go` (modified if tests exist, or new tests)

**Scope**: Wire `deployments.Store` into `services.Manager`. Each lifecycle method
now creates/updates deployment records. The `Manager` struct gains a `deployStore`
field.

### Commit 4: `feat(reconciler): record deployment events on crash/timeout/recovery`

**Files**:
- `internal/reconciler/reconciler.go` (modified)
- `internal/reconciler/reconciler_test.go` (modified)

**Scope**: Wire `deployments.Store` into `Reconciler`. Crash detection, stale deploy
timeout, and circuit auto-recovery now insert deployment records.

### Commit 5: `feat(api): update deployments handler to use deployments table`

**Files**:
- `internal/api/services.go` (modified -- `handleServiceDeployments`, remove old `DeploymentRecord`)
- `internal/api/server.go` (modified -- add `deploymentStore` to `Server`)
- `internal/api/server_test.go` (modified -- update existing tests, add new pagination test)

**Scope**: The API handler now queries the real `deployments` table with pagination.
Old `DeploymentRecord` struct is removed. `ServerConfig` gains `DeploymentStore`.

### Commit 6: `feat(main): wire deployments.Store through dependency graph`

**Files**:
- `cmd/ah/main.go` (modified)

**Scope**: Instantiate `deployments.NewStore(store.StateDB)` and pass it to
`services.NewManager`, `reconciler.New`, and `api.ServerConfig`.

### Optional Commit 7: `feat(builds): pass buildID through DeployFunc for deploy attribution`

**Files**:
- `internal/builds/builds.go` (modified -- `DeployFunc` signature)
- `internal/services/deploy_image.go` (modified -- accept buildID parameter)

**Scope**: Extends the build-to-deploy handoff to carry the `buildID` so that
build-triggered deployments can be linked to their build record.

---

## 9. Migration Safety

- The migration is **additive only** (new table + new indexes). No existing tables or
  columns are modified.
- `ON DELETE CASCADE` on `service_id` ensures cleanup when services are deleted.
- Existing data is unaffected. The deployments table starts empty -- there is no backfill
  of historical data (by design, since the old system did not persist deployment events).
- The migration runs inside a transaction (per `db.go` migration runner behavior).

---

## 10. API Versioning Impact

Per the API versioning policy in `CLAUDE.md`:

- **Additive changes are NOT breaking** and do not require a version bump.
- The new response adds fields (`tenant_id`, `build_id`, `trigger`, `error_message`,
  `completed_at`, `created_at`) to the existing JSON array response.
- The response changes from a max-1-element array to a full paginated array. This is
  technically a behavior change but not a schema-breaking change -- clients parsing the
  array will get more elements, not differently-shaped elements.
- The `id` field format changes from `"deploy-{svcID}-{timestamp}"` to a random hex
  string. If any client relies on parsing the synthetic ID format, this could be breaking.
  **Recommendation**: Document this in the CHANGELOG and consider it acceptable since the
  ID was explicitly documented as derived/temporary.

**Verdict**: No API version bump required. Document in CHANGELOG.

---

## 11. Open Questions / Future Work

1. **Retention policy**: Should old deployment records be pruned after N days or N records
   per service? A GC pass could be added to the reconciler.
2. **Dashboard integration**: The supervisory dashboard could display deployment history
   per service. This is a separate frontend concern.
3. **Webhook notifications**: Future work could emit webhook events when deployments change
   status (not in scope for this issue).
4. **Filtering**: The API could support `?status=failed` or `?trigger=build` query params
   for filtering. Additive, can be done in a follow-up.
