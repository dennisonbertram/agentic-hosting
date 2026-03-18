# Agentic-Hosting Improvement Prompts

Each prompt below is self-contained with full context, file paths, line numbers, and acceptance criteria. They can be executed independently and in any order (though the numbered order is recommended).

---

## Prompt 1: Replace String-Based Error Handling with Typed Sentinel Errors

### Context

The agentic-hosting codebase is a Go 1.25 single-binary PaaS. Error routing in API handlers currently uses `strings.Contains(err.Error(), ...)` to decide HTTP status codes. This appears in 20+ locations across 4 files and is the single most pervasive code quality issue.

### Current Behavior (with exact locations)

**`internal/api/services.go` lines 64-79** — `handleServiceCreate`:
```go
msg := err.Error()
if strings.Contains(msg, "service limit reached") {
    writeError(w, http.StatusForbidden, msg)
} else if strings.Contains(msg, "invalid") || strings.Contains(msg, "not allowed") {
    writeError(w, http.StatusBadRequest, msg)
} else if strings.Contains(msg, "tenant") {
    writeError(w, http.StatusForbidden, msg)
}
```
This same pattern repeats at lines 123-128, 141-144, 160-188, 204-210, 279, 343, 370.

**`internal/api/builds.go` lines 38-56** — `handleBuildCreate`:
```go
msg := err.Error()
if strings.Contains(msg, "not found") {
    writeError(w, http.StatusNotFound, msg)
}
if strings.Contains(msg, "not allowed") || ... {
    writeError(w, http.StatusBadRequest, msg)
}
if strings.Contains(msg, "already running") {
    writeError(w, http.StatusConflict, msg)
}
```

**`internal/api/databases.go` lines 70-78** — uses exact string equality:
```go
if err.Error() == "database not found" {
    writeError(w, http.StatusNotFound, "database not found")
}
```

**`internal/api/databases.go` lines 122-134** — `isUserError()` uses fragile prefix matching:
```go
func isUserError(err error) bool {
    msg := err.Error()
    switch {
    case len(msg) > 7 && msg[:7] == "invalid":
        return true
    case msg == "name is required (max 128 chars)":
        return true
    case len(msg) >= 23 && msg[:23] == "database quota exceeded":
        return true
    }
    return false
}
```

### Why This Matters

1. Error messages can change in library updates, silently breaking error routing
2. Cannot use `errors.Is()` / `errors.As()` — the standard Go error-handling idiom
3. A typo in a string literal causes wrong HTTP status codes with no compiler warning
4. The same error categories (not found, conflict, validation, quota) are matched differently in each handler

### Task

1. **Create `internal/apierr/errors.go`** — Define sentinel errors and a typed `APIError` struct:
   - `ErrNotFound` (maps to 404)
   - `ErrConflict` (maps to 409) — for "already running", "already exists"
   - `ErrValidation` (maps to 400) — for "invalid", "not allowed", malformed input
   - `ErrQuotaExceeded` (maps to 403) — for "service limit reached", "database quota exceeded", tenant limits
   - `ErrForbidden` (maps to 403) — for tenant isolation violations
   - Include an `APIError` struct: `type APIError struct { Code int; Message string; Err error }` that implements `error` and `Unwrap()`
   - Include a helper: `func WriteAPIError(w http.ResponseWriter, err error)` that uses `errors.As()` to extract `APIError` and write the correct status code, falling back to 500

2. **Update all service/database/build managers** to return these typed errors instead of `fmt.Errorf("not found")`. The managers are in:
   - `internal/services/services.go` — wrap "not found" returns with `apierr.ErrNotFound`
   - `internal/services/deploy_image.go` — wrap conflict/validation errors
   - `internal/builds/` — wrap build-related errors
   - `internal/databases/databases.go` — wrap database errors

3. **Replace all string-matching error handlers** in:
   - `internal/api/services.go` — all ~12 locations
   - `internal/api/builds.go` — lines 38-56, 119, 157
   - `internal/api/databases.go` — lines 70-78, 122-134 (delete `isUserError()` entirely)

4. **Delete the `isUserError()` function** in `databases.go` — it becomes unnecessary.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] All existing tests pass (`go test ./...`)
- [ ] Zero uses of `strings.Contains(err.Error(), ...)` remain in `internal/api/`
- [ ] Zero uses of `err.Error() == "..."` remain in `internal/api/`
- [ ] `isUserError()` function is deleted
- [ ] Every API handler uses `errors.Is()`, `errors.As()`, or `apierr.WriteAPIError()` for error routing
- [ ] New `internal/apierr/errors_test.go` with tests for `WriteAPIError` and error wrapping/unwrapping

---

## Prompt 2: Replace O(n) Cache Eviction with Heap-Based LRU

### Context

Three independent in-memory caches in the codebase use identical O(n) linear-scan eviction when at capacity. Under high load (5000+ cached auth keys, 10000+ rate limit entries), every new insert triggers a full map scan while holding a mutex — causing latency spikes and lock contention.

### Current Behavior (with exact locations)

**`internal/middleware/auth.go` lines 71-89** — Auth cache (max 5000 entries):
```go
func (c *authCache) set(keyID string, entry *authCacheEntry) {
    c.mu.Lock()
    if len(c.entries) >= authCacheMaxKeys {  // 5000
        var oldestKey string
        var oldestTime time.Time
        for k, v := range c.entries {  // O(n) scan
            if oldestKey == "" || v.cachedAt.Before(oldestTime) {
                oldestKey = k
                oldestTime = v.cachedAt
            }
        }
        if oldestKey != "" {
            delete(c.entries, oldestKey)
        }
    }
    c.entries[keyID] = entry
    c.mu.Unlock()
}
```

**`internal/middleware/ratelimit.go` lines 51-82** — Rate limiter (max 10000 entries):
```go
func (rl *RateLimiter) getLimiter(key string) *rate.Limiter {
    rl.mu.Lock()
    defer rl.mu.Unlock()
    // ... same O(n) eviction pattern with lastSeen instead of cachedAt
    if len(rl.entries) >= maxRateLimitEntries {  // 10000
        // linear scan for oldest lastSeen
    }
}
```

**`internal/api/tenants.go` lines 120-132** — Registration limiter (max 10000 entries):
```go
if len(rl.entries) >= regMaxEntries {  // 10000
    // same O(n) linear scan for oldest windowAt
}
```

Each cache also has a background cleanup goroutine that periodically evicts expired entries (auth.go lines 45-57: every 1min; ratelimit.go lines 37-49: every 5min; tenants.go lines 88-102: every 5min).

### Why This Matters

- At capacity, every insert is O(n) while holding the mutex
- Auth cache: 5000-entry scan on every new API key seen
- Rate limiter: 10000-entry scan on every new tenant/IP
- These are hot paths — every authenticated request touches the auth cache
- An attacker can force continuous evictions by rotating API keys

### Task

1. **Create `internal/cache/lru.go`** — A generic bounded cache with O(log n) eviction using `container/heap`:
   ```go
   type Cache[K comparable, V any] struct {
       mu      sync.Mutex
       entries map[K]*entry[K, V]
       heap    entryHeap[K, V]
       maxSize int
   }
   ```
   - `Set(key K, value V)` — insert or update, evict oldest if at capacity via heap pop (O(log n))
   - `Get(key K) (V, bool)` — lookup by key, update timestamp in heap
   - `Delete(key K)` — explicit removal (for auth cache invalidation)
   - `Len() int`
   - `Cleanup(olderThan time.Duration)` — remove all entries older than duration (replaces background goroutines)

2. **Create `internal/cache/lru_test.go`** — Tests for:
   - Basic set/get/delete
   - Eviction at capacity evicts oldest
   - Update refreshes timestamp
   - Cleanup removes expired entries
   - Concurrent access safety

3. **Refactor `internal/middleware/auth.go`**:
   - Replace `authCache` struct with `cache.Cache[string, *authCacheEntry]`
   - Remove the O(n) eviction logic (lines 71-89)
   - Keep the `InvalidateKey()` method using `cache.Delete()`
   - Keep the background cleanup goroutine but delegate to `cache.Cleanup(authCacheTTL)`

4. **Refactor `internal/middleware/ratelimit.go`**:
   - Replace `entries map[string]*rateLimitEntry` with `cache.Cache[string, *rateLimitEntry]`
   - Remove O(n) eviction in `getLimiter()` (lines 61-74)
   - Keep background cleanup delegating to `cache.Cleanup(1 * time.Hour)`

5. **Refactor `internal/api/tenants.go`**:
   - Replace `registrationLimiter.entries` with `cache.Cache[string, *regEntry]`
   - Remove O(n) eviction in `allow()` (lines 120-132)
   - Keep background cleanup delegating to `cache.Cleanup(regWindow)`

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] New `internal/cache/lru_test.go` passes with race detector (`go test -race ./internal/cache/`)
- [ ] Zero O(n) eviction loops remain in `middleware/auth.go`, `middleware/ratelimit.go`, or `api/tenants.go`
- [ ] Auth cache invalidation (`InvalidateKey`) still works correctly
- [ ] Background cleanup goroutines still run on their existing intervals

---

## Prompt 3: Persist Deploy Failures to Database

### Context

When a service is created, the API handler spawns an async goroutine to deploy it. If the deploy fails, the error is only logged via `log.Printf` — it is never written back to the database. The user sees their service stuck in `"deploying"` status indefinitely (until the reconciler marks it `"failed"` after 10 minutes for stale deployments).

### Current Behavior

**`internal/api/services.go` lines 84-91**:
```go
go func(tid, sid string) {
    deployCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    if err := s.svcManager.Deploy(deployCtx, tid, sid); err != nil {
        log.Printf("deploy failed for service %s: %v", sid, err)
        return  // Error lost — not persisted to DB
    }
}(tenantID, svc.ID)

svc.Status = "deploying"
writeJSON(w, http.StatusCreated, svc)
```

The reconciler (`internal/reconciler/reconciler.go`) does catch stale deployments after 10 minutes and marks them failed, but the actual error message is lost.

### Why This Matters

- Users poll `GET /v1/services/{id}` and see `"deploying"` for up to 10 minutes with no error context
- The actual deploy error (e.g., "image pull failed", "port conflict", "out of memory") is only in server logs
- No programmatic way for agents/clients to detect and react to deploy failures quickly
- The reconciler's 10-minute stale detection is a safety net, not the primary error path

### Task

1. **Add a `last_error` column to the `services` table** — Create migration `internal/db/migrations/state_010_service_last_error.sql`:
   ```sql
   ALTER TABLE services ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
   ```
   Update `internal/db/db.go` to include this migration in the migration list.

2. **Add an `UpdateStatusWithError` method** to the service manager (`internal/services/services.go`):
   ```go
   func (m *Manager) UpdateStatusWithError(ctx context.Context, tenantID, serviceID, status, lastError string) error
   ```
   This should update both `status` and `last_error` in a single UPDATE statement. Clear `last_error` when status is `"running"`.

3. **Update the async deploy goroutine** in `internal/api/services.go` (lines 84-91):
   - On deploy failure: call `UpdateStatusWithError(ctx, tid, sid, "failed", err.Error())`
   - On deploy success: call `UpdateStatusWithError(ctx, tid, sid, "running", "")` (clear any previous error)

4. **Include `last_error` in API responses** — Update the `Service` struct (likely in `internal/services/services.go` or a models file) to include `LastError string \`json:"last_error,omitempty"\`` and ensure it's scanned from DB queries.

5. **Update the reconciler** (`internal/reconciler/reconciler.go`) to also set `last_error` when it marks services as failed:
   - Stale deployment: `last_error = "deployment timed out (>10 minutes)"`
   - Circuit breaker open: `last_error = "circuit breaker opened: too many crashes"`
   - Container exited: `last_error = "container exited unexpectedly"`

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] New migration `state_010` applies cleanly
- [ ] `GET /v1/services/{id}` includes `"last_error"` field when non-empty
- [ ] Deploy failure within the goroutine immediately sets status to `"failed"` with error message (not after 10-minute reconciler delay)
- [ ] Successful deploy clears `last_error`
- [ ] Reconciler sets meaningful `last_error` for each failure mode
- [ ] `last_error` is omitted from JSON when empty (using `omitempty`)

---

## Prompt 4: Add Port Validation at API and Deploy Layers

### Context

Service port numbers are not validated at the API layer. The `svc.Port` value from the database is used directly in container creation. Only the `PORT` environment variable override has bounds checking (1-65535). An invalid port (0, negative, or >65535) from the API request flows through to Docker, which rejects it with a cryptic error.

### Current Behavior

**`internal/services/deploy_image.go` lines 79-88**:
```go
port := svc.Port
if port <= 0 {
    port = 8000  // Fallback, but no validation that svc.Port is in valid range
}
if p, ok := envVars["PORT"]; ok {
    var parsed int
    if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
        port = parsed  // Only the env override is validated
    }
}
```

The API handler in `internal/api/services.go` `handleServiceCreate` accepts a port from the request body but does not validate its range before storing to DB.

### Task

1. **Add port validation in `handleServiceCreate`** (`internal/api/services.go`):
   - After parsing the request body, validate that `req.Port` is either 0 (meaning "use default") or in range 1-65535
   - Return `400 Bad Request` with message `"port must be between 1 and 65535"` if invalid
   - Do the same in `handleServiceUpdate` if port can be updated

2. **Add port validation in `DeployImage`** (`internal/services/deploy_image.go`):
   - After resolving the final port (from svc.Port, default, or PORT env), validate 1-65535
   - Return a typed error (use `apierr.ErrValidation` if Prompt 1 is done, otherwise `fmt.Errorf("invalid port %d: must be 1-65535", port)`)
   - Also reject privileged ports (< 1024) unless there's a specific reason to allow them — services run in unprivileged containers and should use high ports

3. **Add a `ValidatePort` helper** in an appropriate location (e.g., `internal/services/validate.go`):
   ```go
   func ValidatePort(port int) error {
       if port < 1 || port > 65535 {
           return fmt.Errorf("port must be 1-65535, got %d", port)
       }
       return nil
   }
   ```

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] API rejects port=0 (unless explicitly meaning "use default"), port=-1, port=70000 with 400
- [ ] API accepts port=8080, port=3000, port=65535
- [ ] `DeployImage` validates the final resolved port before passing to Docker
- [ ] Port validation has a unit test

---

## Prompt 5: Add Test Coverage for Untested Critical Packages

### Context

The project has the following test coverage situation:
- `internal/builder/` — **0% coverage**, no test file. Contains the Nixpacks build pipeline (git clone, build, push).
- `internal/gc/` — **0% coverage**, no test file. Contains the garbage collector (orphaned containers, volumes, build dirs).
- `internal/diskcheck/` — **0% coverage**, no test file. Contains disk watermark checks.
- `internal/docker/` — **5.8% coverage**. Has a test file but covers almost nothing.
- `internal/services/` — **2.3% coverage**. Has a test file but covers almost nothing.

The project uses `internal/testutil/` for shared test helpers including in-memory SQLite and mock Docker client. Tests use `github.com/stretchr/testify` for assertions.

### Existing Test Infrastructure

The project already has:
- `internal/testutil/` with mock Docker client and in-memory SQLite helpers
- testify as a dependency
- Existing test patterns in `internal/reconciler/reconciler_test.go`, `internal/middleware/auth_test.go`, `internal/db/db_test.go`

### Task

1. **`internal/gc/gc_test.go`** — Test the garbage collector:
   - Test `cleanOrphanedServiceContainers`: mock Docker to return containers not in DB → verify they get removed
   - Test `cleanOrphanedDatabaseContainers`: same pattern for database containers
   - Test `cleanOrphanedVolumes`: mock Docker to return volumes not associated with any service
   - Test `cleanOldBuildDirs`: create temp dirs with old timestamps → verify cleanup
   - Test `cleanDanglingImages`: mock Docker to return untagged images → verify removal
   - Test that containers younger than 10 minutes are NOT cleaned (grace period)
   - Use the mock Docker client from `internal/testutil/`

2. **`internal/diskcheck/diskcheck_test.go`** — Test disk watermark checks:
   - Test `Check()` with a real temp directory (it uses `syscall.Statfs`)
   - Test that warning threshold triggers warning error
   - Test that block threshold triggers block error
   - Test `CheckAll()` with mix of existing and non-existing paths
   - Test that non-existing paths are skipped (not errored)

3. **`internal/builder/builder_test.go`** — Test the build pipeline:
   - Test `NewBuilder()`: valid workDir + nixpacks path → success; invalid paths → error
   - Test input validation: empty git URL, empty build ID, invalid ref characters
   - Test SSRF protection: private IPs, localhost, link-local addresses should be rejected
   - Test ref sanitization: shell metacharacters in git ref should be rejected
   - Mock external commands (git, nixpacks, docker) — don't actually run them
   - Focus on the validation and error paths, not the actual build execution

4. **Improve `internal/docker/client_test.go`** — Increase from 5.8%:
   - Test `RunContainer` with various configurations (port mappings, env vars, memory limits)
   - Test `StopContainer` / `RemoveContainer` with mock client
   - Test network creation/deletion
   - Test error paths (Docker daemon unavailable, container not found)

5. **Improve `internal/services/services_test.go`** — Increase from 2.3%:
   - Test `Create` with valid input → service created in DB
   - Test `Create` hitting quota limits → appropriate error
   - Test `Get` / `List` / `Delete` basic CRUD
   - Test `UpdateStatus` state transitions
   - Use in-memory SQLite from testutil

### Acceptance Criteria

- [ ] `go test ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `internal/gc/` coverage > 60%
- [ ] `internal/diskcheck/` coverage > 70%
- [ ] `internal/builder/` coverage > 40% (validation/error paths)
- [ ] `internal/docker/` coverage > 30%
- [ ] `internal/services/` coverage > 40%
- [ ] All new tests use testify assertions
- [ ] All new tests use mock Docker client (no real Docker required)
- [ ] No test depends on `/var/lib/ah/` or any production path

---

## Prompt 6: Consolidate N+1 Queries in Tenant Usage Endpoint

### Context

The `handleTenantUsage` handler in `internal/api/tenants.go` makes 4 separate database queries to build the usage response. Since this is SQLite (single-writer), these sequential queries hold the connection longer than necessary and create unnecessary overhead.

### Current Behavior

**`internal/api/tenants.go` lines 317-360**:
```go
// Query 1: Get quotas
err = s.store.StateDB.QueryRow(
    `SELECT max_services, max_databases, max_memory_mb, max_cpu_cores, max_disk_gb, api_rate_limit
     FROM tenant_quotas WHERE tenant_id = ?`, tenantID).Scan(...)

// Query 2: Count services
err = s.store.StateDB.QueryRow(
    `SELECT COUNT(*) FROM services WHERE tenant_id = ?`, tenantID).Scan(&usage.ServicesUsed)

// Query 3: Count databases
err = s.store.StateDB.QueryRow(
    `SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'`, tenantID).Scan(&usage.DatabasesUsed)

// Query 4: Count API keys
err = s.store.StateDB.QueryRow(
    `SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL`, tenantID).Scan(&usage.APIKeysUsed)
```

Each query has its own error handling block. Under concurrent usage (100 tenants polling), this becomes 400 queries.

### Task

1. **Combine into a single query** using scalar subqueries:
   ```sql
   SELECT
       q.max_services, q.max_databases, q.max_memory_mb, q.max_cpu_cores, q.max_disk_gb, q.api_rate_limit,
       (SELECT COUNT(*) FROM services WHERE tenant_id = ?),
       (SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'),
       (SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL)
   FROM tenant_quotas q
   WHERE q.tenant_id = ?
   ```

2. **Update `handleTenantUsage`** to use the single query with one `Scan()` call and one error check.

3. **Handle the case where tenant has no quotas row** — the current code likely returns a "row not found" error. The combined query should handle this gracefully (e.g., `LEFT JOIN` or check for `sql.ErrNoRows`).

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] `handleTenantUsage` makes exactly 1 database query instead of 4
- [ ] Response body is identical to the current implementation
- [ ] Error handling for missing quota row still works correctly
- [ ] Add a test for the usage endpoint in the API test suite

---

## Prompt 7: Add Pagination to Service and Database List Endpoints

### Context

The builds list endpoint (`GET /v1/services/{id}/builds`) already supports `?limit=` with a default of 100 and max of 200. But the service list (`GET /v1/services`) and database list (`GET /v1/databases`) return ALL records with no pagination. This is inconsistent and will cause problems as tenants accumulate services.

### Current Behavior

**`internal/api/services.go` lines 97-112** — `handleServiceList`:
```go
svcs, err := s.svcManager.List(r.Context(), tenantID)
// Returns ALL services, no limit
```

**`internal/api/databases.go` lines 48-61** — `handleDatabaseList`:
```go
dbs, err := s.dbManager.List(r.Context(), tenantID)
// Returns ALL databases, no limit
```

**`internal/api/builds.go` lines 65-90** — `handleBuildListAll` (reference pattern):
```go
limit := 100
if raw := r.URL.Query().Get("limit"); raw != "" {
    value, err := strconv.Atoi(raw)
    if err != nil || value < 1 || value > 200 {
        writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
        return
    }
    limit = value
}
```

Also note: `handleServiceList` converts nil result to empty slice (`if svcs == nil { svcs = []*services.Service{} }`), but `handleDatabaseList` does NOT — another inconsistency to fix.

### Task

1. **Add `limit` and `offset` query parameters** to both `handleServiceList` and `handleDatabaseList`:
   - Default `limit`: 100
   - Max `limit`: 200
   - Default `offset`: 0
   - Validate: `limit` must be 1-200, `offset` must be >= 0
   - Return 400 with clear message on invalid values

2. **Add `ListPaginated` methods** to the service and database managers:
   ```go
   func (m *Manager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*Service, error)
   ```
   These should use `LIMIT ? OFFSET ?` in the SQL query.

3. **Keep the existing `List` methods** for internal use (reconciler, GC) but have the API handlers call `ListPaginated`.

4. **Fix the nil-to-empty-slice inconsistency** — `handleDatabaseList` should convert nil results to empty slice `[]`, matching what `handleServiceList` already does. This ensures clients always get `[]` not `null` in JSON.

5. **Add response metadata** (optional but recommended):
   ```json
   {
       "data": [...],
       "pagination": {
           "limit": 100,
           "offset": 0,
           "count": 42
       }
   }
   ```
   If this changes the response shape too much, just add `X-Total-Count` header instead.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] `GET /v1/services?limit=10&offset=5` returns at most 10 services starting from the 6th
- [ ] `GET /v1/databases?limit=10` returns at most 10 databases
- [ ] Invalid limit/offset returns 400 with clear error message
- [ ] Empty results return `[]` (not `null`) for both endpoints
- [ ] Default behavior (no params) returns first 100 results

---

## Prompt 8: Log Docker Cleanup Errors Instead of Silently Discarding

### Context

Several locations in the codebase discard Docker cleanup errors using `_ =`. When container stop/remove fails (e.g., Docker daemon unresponsive, container already removed by another process), the error disappears. This makes debugging resource leaks impossible.

### Current Behavior

**`internal/services/deploy_image.go` lines 104-105**:
```go
_ = m.docker.StopContainer(ctx, containerID)
_ = m.docker.RemoveContainer(ctx, containerID)
```

**`internal/services/deploy_image.go` lines 110-111**:
```go
_ = m.docker.StopContainer(ctx, containerID)
_ = m.docker.RemoveContainer(ctx, containerID)
```

These are in the deploy path — when replacing an old container with a new one, or cleaning up after a failed deploy.

### Why This Matters

- Orphaned containers consume memory and CPU
- The GC runs every 5 minutes and catches orphans, but without knowing WHY they're orphaned
- Docker daemon issues go unnoticed until resource exhaustion
- Operators cannot distinguish "container was already gone" (benign) from "Docker daemon refused" (critical)

### Task

1. **Replace all `_ = m.docker.StopContainer(...)` and `_ = m.docker.RemoveContainer(...)`** with logged errors:
   ```go
   if err := m.docker.StopContainer(ctx, containerID); err != nil {
       log.Printf("WARNING: failed to stop container %s during deploy: %v", containerID[:12], err)
   }
   if err := m.docker.RemoveContainer(ctx, containerID); err != nil {
       log.Printf("WARNING: failed to remove container %s during deploy: %v", containerID[:12], err)
   }
   ```

2. **Search the entire codebase** for other `_ = ` assignments on Docker client methods and apply the same treatment. Check:
   - `internal/services/services.go` — Delete/Stop operations
   - `internal/reconciler/reconciler.go` — cleanup operations
   - `internal/gc/gc.go` — orphan removal

3. **Do NOT change the control flow** — these are best-effort cleanup operations. Log the error but continue execution. Do not return errors or change function signatures.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] Zero `_ = m.docker.StopContainer(...)` or `_ = m.docker.RemoveContainer(...)` remain
- [ ] Zero `_ = m.docker.RemoveVolume(...)` remain
- [ ] All cleanup errors are logged with WARNING level and the container/volume ID
- [ ] Control flow is unchanged — cleanup failures do not abort the parent operation

---

## Prompt 9: Extract Hardcoded Paths into Injectable Configuration

### Context

Filesystem paths are hardcoded in multiple files, making it impossible to test without the production directory structure and inflexible for alternative deployments (Docker, different Linux distros, macOS development).

### Current Behavior

**`cmd/ah/main.go`**:
```go
line 31:  dbPath := "/var/lib/ah/ah.db"
line 45:  dbPath := flag.String("db-path", "/var/lib/ah/ah.db", "...")
line 46:  masterKeyPath := flag.String("master-key-path", "/var/lib/ah/master.key", "...")
line 124: nixBuilder, err := builder.NewBuilder("/var/lib/ah/builds", "/usr/local/bin/nixpacks")
```

**`internal/gc/gc.go`**:
```go
line 114: buildDirsCleaned := g.cleanOldBuildDirs("/var/lib/ah/builds", 1*time.Hour)
```

Note: `main.go` already uses `flag.String` for some paths (lines 45-46), but then also hardcodes `/var/lib/ah/builds` and `/usr/local/bin/nixpacks` when constructing the builder (line 124). The GC separately hardcodes the same build dir path.

### Task

1. **Create `internal/config/config.go`** with a `Config` struct:
   ```go
   type Config struct {
       DataDir       string // Base directory, default /var/lib/ah
       DBPath        string // State DB, default $DataDir/ah.db
       MeteringDBPath string // Metering DB, default $DataDir/ah-metering.db
       MasterKeyPath string // Master key, default $DataDir/master.key
       BuildsDir     string // Build working directory, default $DataDir/builds
       NixpacksPath  string // Nixpacks binary, default /usr/local/bin/nixpacks
       ListenAddr    string // HTTP listen address, default :9090
   }

   func DefaultConfig() *Config { ... }
   func (c *Config) Resolve() { /* fill in defaults from DataDir */ }
   ```

2. **Update `cmd/ah/main.go`** to:
   - Parse flags into a `Config` struct
   - Call `config.Resolve()` to fill derived paths
   - Pass config (or individual paths) to all constructors

3. **Update `internal/gc/gc.go`**:
   - Accept `buildsDir string` as a constructor parameter instead of hardcoding
   - The GC is constructed in `main.go` — pass the config value there

4. **Do NOT change the default values** — `/var/lib/ah/` should remain the default. The goal is making paths overridable, not changing them.

5. **Update the builder constructor call** in `main.go` line 124 to use config values instead of hardcoded strings.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] Zero hardcoded `/var/lib/ah/` paths remain outside of `config.go` defaults
- [ ] All paths are overridable via flags or config
- [ ] Default behavior is identical to current (same paths)
- [ ] `internal/config/config_test.go` tests default resolution and custom overrides

---

## Prompt 10: Add Structured Logging with log/slog

### Context

The entire codebase uses `log.Printf()` for all logging. There are no log levels (info/warn/error), no structured fields, and no JSON output option. This makes production log analysis, filtering, and alerting extremely difficult.

### Current State

Examples from across the codebase:
```go
// internal/api/services.go
log.Printf("deploy failed for service %s: %v", sid, err)

// internal/reconciler/reconciler.go
log.Printf("reconciler: error: %v", err)

// internal/gc/gc.go
log.Printf("gc: removing orphaned container %s", id[:12])

// internal/builder/builder.go
log.Printf("build %s: clone failed: %v", buildID, err)
```

No log levels, inconsistent prefixes ("reconciler:", "gc:", "build %s:"), no structured fields for filtering.

### Task

1. **Add a `*slog.Logger` parameter** to all major component constructors:
   - `api.NewServer(...)`
   - `reconciler.New(...)`
   - `gc.New(...)`
   - `builder.NewBuilder(...)`
   - `services.NewManager(...)`
   - `docker.NewClient(...)`

2. **Create a logger in `cmd/ah/main.go`**:
   ```go
   var handler slog.Handler
   if jsonLogs {
       handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
   } else {
       handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
   }
   logger := slog.New(handler)
   ```
   Add `--log-format` flag (text/json, default text) and `--log-level` flag (debug/info/warn/error, default info).

3. **Replace `log.Printf` calls** with appropriate slog levels:
   - `log.Printf("error: ...")` → `logger.Error("...", "key", value)`
   - `log.Printf("warning: ...")` → `logger.Warn("...", "key", value)`
   - `log.Printf("gc: ...")` → `logger.Info("...", "component", "gc")`
   - `log.Printf("reconciler: ...")` → `logger.Info("...", "component", "reconciler")`

4. **Use structured fields** instead of `%s`/`%v` formatting:
   ```go
   // Before:
   log.Printf("deploy failed for service %s: %v", sid, err)

   // After:
   logger.Error("deploy failed", "service_id", sid, "error", err)
   ```

5. **Add component context** using `logger.With()`:
   ```go
   // In reconciler constructor:
   r.logger = logger.With("component", "reconciler")

   // Then in reconciler methods:
   r.logger.Info("cycle complete", "duration_ms", elapsed.Milliseconds())
   ```

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] Zero `log.Printf` calls remain in `internal/` (only in `cmd/` if needed for fatal startup errors)
- [ ] All log calls use `slog.Info`, `slog.Warn`, or `slog.Error` with structured fields
- [ ] `--log-format json` produces valid JSON lines
- [ ] `--log-level error` suppresses info/warn messages
- [ ] Each component's logs include a `"component"` field

---

## Prompt 11: Add Rate Limit Response Headers

### Context

When a client hits the rate limit, they get `429 Too Many Requests` with a JSON body `{"error": "rate limit exceeded"}` but no headers telling them their limit, remaining quota, or when to retry. This makes it impossible for AI agents (the primary users of this PaaS) to implement smart retry logic.

### Current Behavior

**`internal/middleware/ratelimit.go`** — The middleware uses `golang.org/x/time/rate` token bucket limiters per tenant. When a request is rejected:
```go
if !limiter.Allow() {
    writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
    return
}
```

No `Retry-After`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, or `X-RateLimit-Reset` headers.

The rate limit configuration comes from `tenant_quotas.api_rate_limit` (per-tenant) and a global limit of 500 req/sec.

### Task

1. **Add standard rate limit headers** to ALL responses (not just 429s):
   ```
   X-RateLimit-Limit: 100          # requests per second allowed
   X-RateLimit-Remaining: 42       # approximate tokens remaining
   X-RateLimit-Reset: 1710000000   # Unix timestamp when bucket refills
   ```

2. **Add `Retry-After` header** on 429 responses:
   ```
   Retry-After: 1                  # seconds until a token is available
   ```

3. **Implementation approach**:
   - `rate.Limiter` exposes `Tokens() float64` for remaining tokens and `Limit()` for the rate
   - Use `Reserve()` instead of `Allow()` to get timing info: `r := limiter.Reserve(); if !r.OK() || r.Delay() > 0 { ... }`
   - Calculate reset time: `time.Now().Add(r.Delay()).Unix()`

4. **Distinguish tenant vs global rate limits** in the error message:
   - Tenant limit hit: `"tenant rate limit exceeded"`
   - Global limit hit: `"global rate limit exceeded"`

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] Every authenticated response includes `X-RateLimit-Limit` and `X-RateLimit-Remaining` headers
- [ ] 429 responses include `Retry-After` header with seconds until retry
- [ ] Rate limit header values are accurate (not hardcoded)
- [ ] Test that verifies headers are present on both successful and rate-limited responses

---

## Prompt 12: Standardize Error Response Format and HTTP Status Codes

### Context

The API has inconsistent error response handling:
- Some errors return 400 when they should return 409 (conflict) or 422 (validation)
- Error responses are always `{"error": "message"}` with no error code for programmatic handling
- The `isUserError()` function in `databases.go` uses fragile string prefix matching

### Current Inconsistencies

1. **Conflicting resources** sometimes return 400, sometimes 409:
   - Build "already running" → 409 (correct)
   - Service name conflict → 400 (should be 409)

2. **Validation errors** inconsistently use 400 vs 422:
   - Invalid port → 400
   - Invalid env var → 400
   - These are all validation errors that should be 422 or consistently 400

3. **Error response body** has no machine-readable error code:
   ```json
   {"error": "service limit reached"}  // Current — client must parse string
   ```

### Task

1. **Standardize error response format** — Update the `writeError` helper (likely in `internal/api/` or a shared `httpx` package):
   ```json
   {
       "error": {
           "code": "quota_exceeded",
           "message": "Service limit reached (max 10)"
       }
   }
   ```
   Define error codes: `not_found`, `conflict`, `validation_error`, `quota_exceeded`, `rate_limited`, `unauthorized`, `internal_error`.

2. **Standardize status code usage** across all handlers:
   - 400: Malformed request (bad JSON, missing required field)
   - 404: Resource not found
   - 409: Conflict (resource already exists, build already running)
   - 422: Validation error (valid JSON but invalid values — bad port, name too long)
   - 403: Quota exceeded or tenant isolation violation
   - 429: Rate limited
   - 500: Internal server error

3. **Update all handlers** to use the correct status codes. Key changes:
   - Service name already exists → 409 (not 400)
   - Invalid service name format → 422 (not 400)
   - Database quota exceeded → 403 (not 400)

4. **Maintain backward compatibility** — The `"error"` field should still be present at the top level for clients that check `response.error`. You can nest it:
   ```json
   {"error": {"code": "not_found", "message": "service not found"}}
   ```

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass (update assertions for new response format)
- [ ] Every error response includes `error.code` and `error.message`
- [ ] 409 used for all conflict situations (duplicate names, already running)
- [ ] 403 used for all quota/permission situations
- [ ] `isUserError()` in databases.go is deleted (replaced by typed errors)
- [ ] Error codes are documented in a comment or constant block

---

## Prompt 13: Remove Dead Code in diskcheck Package

### Context

The `diskcheck.Check()` function is a no-op wrapper that adds no logic.

### Current Code

**`internal/diskcheck/diskcheck.go` lines 14-19**:
```go
func Check(path string, warnPct, blockPct float64) error {
    if err := checkPath(path, warnPct, blockPct); err != nil {
        return err
    }
    return nil
}
```

This is equivalent to `return checkPath(path, warnPct, blockPct)`. The `CheckAll()` function (lines 24-35) already calls `checkPath` directly.

### Task

1. **Option A (preferred)**: Inline `Check()` — replace its body with `return checkPath(path, warnPct, blockPct)` and update any callers to call `checkPath` directly if `Check` was only used for the public API surface. If `Check` is the exported function and `checkPath` is unexported, just simplify the body.

2. **Option B**: If `Check()` is the public API and callers depend on it, simplify to:
   ```go
   func Check(path string, warnPct, blockPct float64) error {
       return checkPath(path, warnPct, blockPct)
   }
   ```

3. **Find all callers** of `Check()` — update them if the signature changes.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] No unnecessary `if err != nil { return err } return nil` pattern in diskcheck

---

## Prompt 14: Inconsistent Error Wrapping (%v vs %w)

### Context

Go's `fmt.Errorf` supports `%w` for wrapping errors (preserving the error chain for `errors.Is()` / `errors.As()`) and `%v` for formatting (which breaks the chain). The codebase mixes both patterns, meaning some wrapped errors can't be type-checked by callers.

### Task

1. **Search the entire codebase** for `fmt.Errorf` calls using `%v` with an `error` argument:
   ```
   grep -rn 'Errorf.*%v.*err' internal/
   ```

2. **Replace `%v` with `%w`** for every `fmt.Errorf` that wraps an error and is returned to a caller. Example:
   ```go
   // Before:
   return fmt.Errorf("failed to create service: %v", err)

   // After:
   return fmt.Errorf("failed to create service: %w", err)
   ```

3. **Do NOT change `%v` to `%w`** in log statements or when the error is used only for display (e.g., `log.Printf("error: %v", err)`).

4. **Do NOT change cases where wrapping would be incorrect** — e.g., if you're constructing a new error that shouldn't expose internal details to callers.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] All `fmt.Errorf` calls that return errors use `%w` for error arguments
- [ ] `log.Printf` and display-only formatting still uses `%v`
- [ ] Run `go vet ./...` — no new warnings

---

## Prompt 15: Add Env Var Value Length Validation

### Context

Environment variable keys and values are stored encrypted (AES-256-GCM) in the `service_env` table. While there's a count limit (max 100 env vars per service), there's no validation on individual key length or value length. A malicious tenant could store arbitrarily large values, bloating the database and encryption overhead.

### Current Behavior

**`internal/api/services.go`** — env var bulk set validates count but not size:
```go
if len(vars) == 0 {
    writeError(w, http.StatusBadRequest, "no environment variables provided")
    return
}
if len(vars) > 100 {
    writeError(w, http.StatusBadRequest, "too many environment variables (max 100)")
    return
}
// No validation on key/value lengths
```

### Task

1. **Add validation constants**:
   ```go
   const (
       maxEnvVarKey   = 256    // bytes
       maxEnvVarValue = 32768  // 32KB
   )
   ```

2. **Add key/value validation** in the env var set handler(s):
   - Key must be 1-256 bytes
   - Key must match `^[A-Za-z_][A-Za-z0-9_]*$` (valid shell variable name)
   - Value must be 0-32768 bytes
   - Return 400 with specific message identifying which key is invalid

3. **Apply to both** the bulk set endpoint and single key set endpoint.

### Acceptance Criteria

- [ ] `go build ./...` passes
- [ ] All existing tests pass
- [ ] Keys > 256 bytes rejected with 400
- [ ] Values > 32KB rejected with 400
- [ ] Invalid key names (starting with digit, containing special chars) rejected
- [ ] Empty key rejected
- [ ] Empty value allowed (useful for unsetting)
- [ ] Unit test for validation function

---

## Prompt 16: Document API Versioning Strategy

### Context

All API endpoints use the `/v1/` prefix but there's no documented policy for when/how the version would change, how deprecation works, or what constitutes a breaking change.

### Task

1. **Add an "API Versioning" section to `CLAUDE.md`**:
   ```markdown
   ## API Versioning

   - All endpoints are prefixed with `/v1/`
   - Breaking changes (removing fields, changing types, removing endpoints) require a version bump to `/v2/`
   - Additive changes (new fields, new endpoints, new optional parameters) are NOT breaking
   - Deprecated endpoints get a `Deprecation: true` response header
   - Minimum deprecation period: 90 days before removal
   ```

2. **Add a `Deprecation` middleware helper** (optional, for future use):
   ```go
   func Deprecated(sunset time.Time, successor string) func(http.Handler) http.Handler
   ```
   This adds `Deprecation: true`, `Sunset: <date>`, and `Link: <successor>; rel="successor-version"` headers.

### Acceptance Criteria

- [ ] `CLAUDE.md` has a clear API versioning section
- [ ] Deprecation middleware compiles (if implemented)
- [ ] No existing endpoints are modified
