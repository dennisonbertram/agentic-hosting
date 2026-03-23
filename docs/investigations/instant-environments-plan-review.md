# Instant Environments — Implementation Plan Review

**Date**: 2026-03-23  
**Status**: Critical review (gaps/risks/edge cases)

## Executive Summary

The plan is directionally right (make `environment` a first-class resource, `sleep infinity` + `exec`, lease/TTL, templates, warm pool, preview routing). The biggest risks are not “more endpoints” — they’re **state-machine correctness**, **security invariants for warm pools**, and **operational safety (timeouts/output limits/disk pressure)**.

If Phase 1 ships without addressing the CRITICAL items below, the most likely outcomes are:
- “Exec doesn’t work reliably” (timeouts / hung processes / unbounded output)
- “Warm pool leaks data across tenants” (volume + container reuse contamination)
- “Environments are unusable for real dev” (no egress + readonly FS + missing writable HOME/caches)
- “Resource leak / disk pressure incident” (workspace volumes + GC gaps)

---

## Top Blockers (Fix Before Phase 1)

### CRITICAL — 300s synchronous exec conflicts with current server timeouts

**Why this is a blocker**
- The plan assumes HTTP exec can run up to **300s**. Today the server is configured with:
  - `cmd/ah/main.go`: `http.Server{ WriteTimeout: 30 * time.Second }`
  - `internal/api/server.go`: most authenticated routes use `chimw.Timeout(30 * time.Second)`
- Even if you add an “exec group” with a longer chi timeout, the **server-level WriteTimeout will still kill the response** around 30s.

**Required design decision**
- Either:
  1) make exec asynchronous (create an exec job, poll logs/results), or
  2) keep exec synchronous but raise timeouts globally (or remove WriteTimeout), and add strict per-tenant/per-env concurrency + output caps.

**Mitigations to include in Phase 1**
- Explicit exec timeout semantics (what is killed? what persists?)
- Concurrency limits (per-tenant, per-env, global) to prevent goroutine exhaustion
- Output size limit (stdout+stderr) with truncation markers (prevent memory blowups)

---

### CRITICAL — Warm pool + `/workspace` volume persistence is underspecified and easy to get dangerously wrong

**Risk**
- Containers cannot have mounts “late-bound” after creation. If a pool container has a volume at `/workspace` and is reused across tenants/environments, you have a **cross-tenant data leak** unless you do an airtight wipe/reset.
- If pool containers are truly generic and later “re-labeled” and attached, you still have:
  - mount immutability
  - network attachment/alias correctness (for preview routing)
  - naming/identity expectations (`ah-env-tenantID-envID`)

**You must clarify**
- Are pool containers **one-time** (pre-created and then permanently “become” an environment), or **reused/returned** to pool?
  - One-time is far safer; reuse requires a true “factory reset” protocol (wipe volume, kill processes, reset metadata), which is hard to prove correct.

**Minimum safeguards**
- Pool acquisition must be atomic (single-winner) under concurrency (SQLite transaction / `UPDATE … WHERE state='free' LIMIT 1`-style pattern)
- If preview uses Traefik file-provider URLs like services (`http://{containerName}:{port}`), then pool acquisition must also ensure:
  - stable hostname/alias (container rename or network aliasing)
  - the container is attached to the correct tenant network

---

### CRITICAL — `sleep infinity` as PID 1 will leak zombies under real exec usage

**Why this matters**
- With exec, users will run commands that spawn short-lived children or background processes. PID 1 in Linux is responsible for reaping zombies; `sleep` doesn’t.
- Result: zombie accumulation → `PidsLimit` exhaustion → environment becomes “mysteriously broken”.

**Mitigation**
- Enable Docker init for environments (`HostConfig.Init=true`) or run a known init like `tini`.

---

### CRITICAL — Readonly rootfs + minimal tmpfs will break common dev workflows (or force unsafe changes later)

**Symptoms you’ll hit immediately**
- `apt-get` / package installs require writable `/var/lib` and `/var/cache`.
- Many tools write to `$HOME`, `~/.cache`, etc.
- If only `/workspace` is persistent and rootfs is readonly, you need to explicitly decide where these writes go.

**Mitigations**
- Define the writable surface area for environments explicitly:
  - set `HOME=/workspace` (or provide a separate `home` volume)
  - route tool caches into `/workspace` via env vars (Go, npm, pip, cargo, etc.)
  - if you want `apt`, either bake toolchains into templates or provide tmpfs mounts for required paths

---

### CRITICAL — Network egress policy is missing (but required for “real” worktrees)

**Problem**
- Tenant networks are created as Docker `Internal: true` (no internet). This is consistent with “secure runtime services”, but it makes dev environments unable to:
  - `go mod download`
  - `npm install`
  - `pip install`
  - fetch test fixtures, etc.

**If you enable egress, you open security exposure**
- You need an explicit egress posture for environments (likely different from services).
- There is already a security proposal for allowlisted egress in builds (`docs/security/build-egress-allowlist.md`). Environments need the same level of clarity.

**Required decision**
- “No egress” (and accept limited usability), or
- allowlisted proxy egress (recommended), or
- full egress (high risk; needs auditing + abuse controls).

---

## Security & Isolation Concerns

### HIGH — Warm pool contamination beyond files (processes, env vars, metadata)

Even if you solve volume reuse, container reuse can leak:
- background processes still running
- altered state in writable paths (tmpfs) that persists for the life of the container
- env vars inherited from acquisition step
- stray network connections

If you truly want reuse, you need a formal “reset contract” and tests that prove it.

---

### HIGH — Exec endpoint can become an abuse primitive without strict limits

Required controls:
- hard cap on exec runtime
- hard cap on output size
- rate limit on exec calls (existing API rate limits are request-rate based, not “CPU-time” based)
- optional command allowlist/denylist for obviously dangerous classes (fork bombs are mitigated by pids limit, but not CPU burn)

---

### HIGH — Idempotency caching should not apply to exec

The idempotency middleware applies to all POST/PUT and may cache responses up to 8KB (`internal/middleware/idempotency.go`). For exec this is surprising and can return stale outputs.

Recommendation: explicitly disable idempotency for environment exec routes (like auth key creation is excluded).

---

### MEDIUM — Tenant → Traefik reachability and cross-tenant Host routing risk expands with “interactive shells”

This risk exists today for services, but environments make it easier to exploit because an attacker has a convenient shell for crafting traffic.

Mitigation is operator/network policy (e.g., `DOCKER-USER` rules) and should be called out as a prerequisite for “preview routing” being safe.

---

## Architecture / State Machine Gaps

### HIGH — Missing explicit environment state model + transition rules

The plan lists CRUD/Start/Stop/Exec/ExtendLease/GC/Reconciler behaviors but doesn’t define:
- authoritative statuses (`creating`, `running`, `stopped`, `failed`, `deleting`, `expired`…)
- which transitions are allowed
- idempotency expectations (e.g., calling `/start` on running env should be 200 no-op or 409?)
- how to handle concurrent calls (`exec` while `stop`, `extendLease` while `expire`, etc.)

Recommendation: document a small state machine and enforce it in manager methods with per-env locks (pattern exists in services manager via striped mutexes).

---

### HIGH — Lease/TTL semantics are underspecified (data loss risk)

Questions Phase 1 must answer:
- On lease expiry: stop or delete? (delete destroys `/workspace` volume)
- Grace period / warning? (agents may assume workspace persists)
- What does `ExtendLease` do if env is stopped? failed?
- Is TTL per-env configurable? bounded by operator policy?

---

### MEDIUM — GC/Reconciler interaction needs clear ownership boundaries

Today GC has a 10-minute minimum-age gate to avoid racing provisioning (`internal/gc/gc.go`). If environments have short TTLs (minutes), GC age gating can delay cleanup and violate “instant” resource reclamation expectations.

Also ensure the reconciler never mutates DB state on transient Docker errors (existing reconciler has this pattern for services/databases).

---

## API Design Issues

### HIGH — List endpoints should be paginated and filterable from day one

There is already a pagination helper in `internal/api/server.go` (`limit/offset`). Environments can explode cardinality quickly; Phase 1 should include:
- `GET /v1/environments?limit=&offset=`
- optional filters: `status`, `template_id`, `name_prefix`, `sort`

---

### HIGH — Exec response schema needs “safety fields”

At minimum:
- `exit_code`
- `stdout` / `stderr` (possibly base64 if binary-safe)
- `truncated: bool` + `truncated_bytes`
- `duration_ms`
- `timed_out: bool`

Also define maximum size guarantees so clients can rely on bounded responses.

---

### MEDIUM — Phase 3 tarball upload is incompatible with current 1MB request body limit

Global middleware caps request bodies to 1MB (`internal/api/server.go`), and the idempotency middleware hashes bodies up to 1MB.

If you add file upload later, you’ll need:
- a separate route group with higher body size (and likely streaming)
- disabled idempotency on upload routes
- clear max upload size and checksum validation

---

## Operational Risks

### HIGH — Workspace volumes are an unbounded disk liability without guardrails

Needed in Phase 1:
- disk space check before provisioning volumes (pattern exists in builds/databases via `internal/diskcheck`)
- clear cleanup rules on delete and on stale-creating reconciliation

Nice-to-have soon:
- per-tenant disk usage accounting against `tenant_quotas.max_disk_gb` (currently not enforced)

---

### HIGH — Warm pool refill policy can cause resource spikes and is hard to tune

Questions:
- What’s the pool size per template? per host?
- What happens under sustained demand? (thundering herd + rapid container creation)
- What happens under memory pressure? (pool should shrink automatically)

Recommendation: demand-driven refill + backoff + hard resource caps, not a fixed 30s ticker-only refill.

---

### MEDIUM — Host reboot / process restart behavior needs explicit reconciliation

On restart you will have:
- environments in DB with leases that are already expired
- pool records whose containers might have been GC’d or never created
- env containers restarted by Docker restart policy but DB status stale

Phase 1 should include a startup reconciliation pass similar to databases/builds.

---

## Integration Gaps with Existing System

### HIGH — Quota update endpoint must include `max_environments`

Adding `tenant_quotas.max_environments` requires updating:
- operator quota PATCH endpoint (`/v1/tenants/{tenantID}/quotas`)
- tenant GET response structs
- quota tests (`internal/api/quotas_test.go`)

This is easy to miss and will create “quota silently ignored” drift.

---

### HIGH — Tenant suspension/deletion should have an explicit environments policy

`handleTenantDelete` suspends the tenant and stops/removes containers labeled `ah.tenant` via services manager. Environment containers will be removed, but **workspace volumes may remain** unless explicitly handled.

Phase 1 should decide:
- keep env rows/volumes for reactivation, or
- delete envs/volumes on suspension, or
- stop envs but keep volumes (and reconcile status)

---

### MEDIUM — Preview routing will stress Traefik config reload behavior

Services write one dynamic config file per service and rely on Traefik hot reload (`internal/services/services.go`). If environments create many preview routes, you will multiply:
- file count
- reload churn
- operator debugging complexity

Consider aggregating env preview routes per-tenant or per-environment into fewer files (or a single file) with atomic rewrite.

---

## Test Plan Gaps (Add These Explicitly)

### CRITICAL tests (must-have)
- Exec timeouts: verify behavior when command exceeds limit (and whether process is actually killed)
- Exec output limit: verify truncation and bounded memory
- Zombie reaping: run many short-lived exec commands; ensure PIDs don’t accumulate
- Warm pool acquisition race: concurrent create must never assign same container
- Pool contamination: prove new environment does not see prior env workspace contents (and/or prove pool is one-time-use)

### HIGH tests
- Lease expiry vs concurrent exec/extendLease/start/stop
- Stuck `creating` reconciliation: process crash mid-create → cleanup and state update
- GC safety: ensure GC never deletes a live env container/volume (age gating + labels)
- API timeouts: confirm end-to-end that exec can run for intended duration under real server settings

### MEDIUM tests
- Pagination/filters on list endpoints
- Quota enforcement under concurrent creates (SQLite transaction strategy)

---

## Suggested Phase 1 Rescope (Pragmatic)

If the goal is “instant shell” safely:
1. Ship environments with a single template image (Ubuntu) and **no warm pool yet**
2. Make exec work reliably (timeouts, output caps, init reaping, concurrency)
3. Add lease/TTL with “stop on expiry” (delete as an explicit action) to avoid surprise data loss
4. Add GC/reconciler + tenant suspension policy for volumes
5. Only then introduce warm pools once correctness/security invariants are locked in

