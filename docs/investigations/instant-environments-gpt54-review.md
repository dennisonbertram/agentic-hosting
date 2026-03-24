# Instant Environments Review

Reviewed:

- `internal/environments/environments.go`
- `internal/environments/pool.go`
- `internal/docker/client.go`
- `internal/api/environments.go`
- `internal/api/server.go`
- `internal/reconciler/reconciler.go`
- `internal/gc/gc.go`
- `internal/db/migrations/state_016_environments.sql`
- `internal/db/migrations/state_017_env_templates_seed.sql`
- `internal/db/migrations/state_018_warm_pool.sql`

I also checked the current targeted tests in `internal/environments/*_test.go`, `internal/reconciler/reconciler_test.go`, and `internal/gc/gc_test.go`.

## Findings

### 1. HIGH: `Create` still has a quota race, so concurrent creates can exceed `max_environments`

- Files: `internal/environments/environments.go:134-173`, `internal/db/migrations/state_016_environments.sql:37`
- The code takes a transaction to read `tenant_quotas.max_environments` and `COUNT(*)`, but it commits that transaction at line 160 before inserting the new environment row at lines 167-173.
- Two concurrent `Create` calls can both pass the count check, both commit, and then both insert rows. The comment says the transaction prevents concurrent creates, but the critical write is outside the transaction.
- This is both a race-condition bug and a resource-exhaustion issue, because the quota is the main guardrail on tenant environment count.

Suggested fix:

- Keep the quota check and the `INSERT INTO environments ... status='creating'` in the same transaction.
- Make the insert conditional on the current count, or re-check after taking the write lock and before commit.

### 2. HIGH: exec timeouts are not actually enforced, and timed-out execs are reported as generic failures

- Files: `internal/environments/environments.go:421-435`, `internal/docker/client.go:808-835`, `internal/api/environments.go:145-151`
- `Manager.Exec` expects `req.Timeout` to cap execution time, but `ExecRun` only wraps the attach/inspect calls in `context.WithTimeout`. Canceling that context does not terminate the process already running inside the container.
- On timeout, `ExecRun` typically falls through to `ContainerExecInspect` with an already-expired context and returns an error. `Manager.Exec` then checks `ctx.Err()` on the parent request context at lines 428-434, not the derived timeout context, so it misclassifies the timeout and returns an error instead of a partial `TimedOut=true` response.
- The practical result is:
  - clients see a generic 500-ish failure path instead of a deterministic timeout result
  - the command may keep running inside the environment after the request has "timed out"
  - a tenant can use long-running execs to consume the environment's CPU/PID budget until someone manually stops the container

Suggested fix:

- Return a typed/sentinel timeout error from `ExecRun` and map it explicitly in `Manager.Exec`.
- Enforce execution limits inside the container or through a killable supervisor path; context-canceling the attach session is not enough.

### 3. HIGH: warm-pool containers are mislabeled as normal environments, so GC can delete the pool and leak its volumes

- Files: `internal/environments/pool.go:168-180`, `internal/docker/client.go:714-723`, `internal/gc/gc.go:246-269`, `internal/gc/gc.go:329-349`
- `PoolManager.refill` tries to create pool containers with `ah.type=warm-pool`, but `DockerClient.RunEnvironment` overwrites `ah.type` to `"environment"` at line 721.
- GC then treats those pool containers as ordinary environment containers and checks them against the `environments` table at `internal/gc/gc.go:246-269`. Warm-pool containers are not in that table, so after `minResourceAge` they are considered orphaned and removed.
- GC does not scan `ah-pool-*` volumes at all; `findOrphanedVolumes` only knows `ah-db-*`, `ah-env-*`, and `ah-kanban-*` prefixes at `internal/gc/gc.go:303-375`.
- That leaves behind:
  - deleted pool containers
  - still-present `warm_pool` rows
  - leaked `ah-pool-*` volumes
- Because `PoolManager.refill` and `Stats` count rows, not live Docker state, the pool can look healthy in SQL while every claimed container is already gone.

Suggested fix:

- Do not overwrite caller-supplied `ah.type` in `RunEnvironment`, or add a dedicated warm-pool container creation path.
- Teach GC to understand `warm_pool` rows and `ah-pool-*` volumes.

### 4. HIGH: reconciliation moves missing/exited environments into a `stopped` state that `Start` cannot recover from

- Files: `internal/reconciler/reconciler.go:468-518`, `internal/environments/environments.go:337-361`, `internal/environments/environments.go:494-519`
- Both the reconciler and `Manager.ReconcileStale` convert a running environment with a missing/dead container into `status='stopped'`.
- `Manager.Start` assumes `stopped` means "there is a valid stopped container to restart" and blindly calls `docker.StartContainer(e.ContainerID)` at lines 350-351.
- If the container was deleted or is otherwise gone, the environment is now stuck:
  - `Get`/`List` still show a normal stopped environment
  - `Start` cannot recreate it
  - `ExtendLease` semantics still operate on the record
  - the only escape hatch is delete-and-recreate
- This is a state-machine bug: `stopped` is being used for both "restartable" and "container missing".

Suggested fix:

- Distinguish restartable stopped environments from missing/failed ones.
- Alternatively, clear `container_id` and let `Start` reprovision from the saved volume/template when the container is gone.

### 5. MEDIUM: failed create after network setup leaks the workspace volume and pins it with a failed DB row

- Files: `internal/environments/environments.go:181-192`, `internal/gc/gc.go:329-349`
- After the environment row is inserted, `Create` makes the volume first and then calls `EnsureNetwork`.
- If `EnsureNetwork` fails, the code marks the environment as failed and returns, but it does not remove the volume it just created.
- Because the failed row still contains `volume_name`, GC will not remove the leaked `ah-env-*` volume; `findOrphanedVolumes` checks only whether some environment row still references the volume.
- This creates a persistent leak path on transient Docker/network failures.

Suggested fix:

- Remove the volume on the `EnsureNetwork` failure path as well.
- Consider deleting the failed row entirely when container provisioning never started, or at least clearing `volume_name` after cleanup.

### 6. MEDIUM: the warm-pool fast path is effectively dead code

- Files: `internal/environments/environments.go:96-99`, `internal/environments/environments.go:101-240`, `internal/environments/pool.go:212-255`, `internal/docker/client.go:780-800`
- `Manager` stores a pool via `SetPool`, but `Create` never reads `m.pool`, never calls `Acquire`, and never uses the Docker helpers that would be required to make a claimed container usable (`RenameContainer`, network reconnect/disconnect, etc.).
- Right now the pool only burns resources in the background; it does not reduce create latency.
- This is also why none of the pool integration bugs surface through the normal environment create path today: the path is never exercised.

Suggested fix:

- Either wire `Create` to `Acquire` plus the rename/network handoff flow, or remove/disable the pool until that path is complete.

### 7. LOW: environment list pagination is inconsistent between the API layer and the manager

- Files: `internal/api/server.go:368-381`, `internal/api/environments.go:54-68`, `internal/environments/environments.go:267-280`
- `parsePagination` allows `limit` up to 200, but `Manager.List` resets any `limit > 100` to 50.
- That means `GET /v1/environments?limit=200` silently becomes `LIMIT 50`, which is both surprising and harder to debug from the client side.

Suggested fix:

- Keep one source of truth for page-size validation.
- If 100 is the real cap, reject values above 100 at the HTTP layer instead of silently shrinking them to 50.

## Test Gaps

### Coverage gap A: no test exercises the concurrent quota race

- Current coverage: `internal/environments/environments_test.go:72-87` only checks the quota path sequentially.
- Missing coverage: two or more concurrent `Create` calls against a tenant with `max_environments=1`.

### Coverage gap B: exec timeout behavior is untested

- Current coverage: `internal/environments/environments_test.go:287-339` only covers success, not-running, and empty-command cases.
- Missing coverage:
  - timeout response mapping
  - partial-output-on-timeout behavior
  - prevention of runaway execs after timeout

### Coverage gap C: environment reconciliation is basically untested

- Current coverage: `internal/reconciler/reconciler_test.go:15-259` is service-focused; it does not cover environment rows at all.
- Missing coverage:
  - missing environment container -> resulting DB state
  - exited/dead environment container -> resulting DB state
  - expired lease -> container stop + DB transition
  - restart behavior after reconciliation has changed state

### Coverage gap D: warm-pool and GC are only tested in isolation, not together

- Current pool coverage: `internal/environments/pool_test.go:107-157`
- Current GC volume coverage: `internal/gc/gc_test.go:95-107`
- Missing coverage:
  - label assertions for warm-pool containers
  - GC behavior against warm-pool containers/rows/volumes
  - stale `warm_pool` rows with missing containers
  - `Acquire` behavior when the claimed container no longer exists

### Coverage gap E: the environment API handlers have no direct route tests

- Route wiring exists at `internal/api/server.go:242-272`, but I could not find corresponding `/v1/environments` coverage under `internal/api/*_test.go`.
- Missing coverage:
  - status-code mapping for create/get/list/delete/start/stop/exec/lease
  - request validation
  - pagination behavior
  - timeout/error mapping for exec

## Verification Notes

- `go test ./internal/environments ./internal/reconciler ./internal/gc` passed during this review.
- `go test ./internal/api/...` is currently red for unrelated existing failures in `TestCreateService_PortValidation`, so I did not treat the API package as a clean signal for environment coverage.
