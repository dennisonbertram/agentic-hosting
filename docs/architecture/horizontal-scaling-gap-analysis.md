# Horizontal Scaling Gap Analysis

**Issue**: #2 — spike: inventory single-host assumptions blocking horizontal scaling
**Date**: 2026-03-20
**Status**: Draft / spike

---

## 1. Executive Summary

The `ah` binary is purpose-built as a single-host PaaS. Every subsystem -- database, builds, routing, reconciliation, rate limiting, crypto -- assumes it runs on a single machine talking to a single Docker daemon. This document catalogs each assumption, explains why it breaks at 2+ control-plane nodes, and proposes a phased migration that preserves the current MVP until scale demands change.

---

## 2. Assumption Inventory

### 2.1 State & Storage

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| S1 | **SQLite is the sole state DB** (WAL mode, file at `/var/lib/ah/ah.db`) | `internal/db/db.go` -- `openDB()` opens a local file with `?_journal_mode=WAL` | SQLite is a single-writer, file-level lock database. Two `ah` processes on different hosts cannot share a WAL file over NFS (WAL requires shared memory). Even on the same host, concurrent writers serialize on `_busy_timeout`. | Swap SQLite for PostgreSQL (or CockroachDB). All queries are standard SQL; the migration is mechanical. Keep SQLite as the dev/test backend behind a `db.Store` interface. | Yes -- single-node is fine for hundreds of tenants. Migrate when request rate exceeds one writer or when HA is required. |
| S2 | **Metering DB is a second local SQLite file** (`ah-metering.db`) | `internal/db/db.go` -- `Open()` derives metering path from state DB path | Same as S1. Metering inserts are append-only and high-frequency; a single SQLite writer becomes a bottleneck first. | Move metering writes to PostgreSQL (or a time-series DB like TimescaleDB). Alternatively, buffer metering events locally and flush to a central store. | Yes -- metering is not on the critical path for serving requests. |
| S3 | **Single master encryption key on local filesystem** (`/var/lib/ah/master.key`) | `cmd/ah/main.go` lines 97-114 -- reads key from `*masterKeyPath` | Every node needs the same 32-byte AES-256-GCM key to decrypt env vars and DB passwords. Copying a key file to N hosts is an operational hazard (drift, exposure). | Read master key from an environment variable or a secrets manager (Vault, AWS KMS envelope encryption). All nodes receive the same key through config management. | Yes -- a single key file works fine on one host. Fix before adding a second node. |
| S4 | **Backup writes to local filesystem** (`VACUUM INTO` + gzip) | `cmd/ah/backup.go` -- `runBackup()` writes to `{dbDir}/backups/` | Backups are only captured on the node running the backup subcommand. With PostgreSQL, `pg_dump` or WAL streaming replaces this entirely. | For SQLite phase: push backups to S3/object storage. For PostgreSQL phase: use native `pg_basebackup` or logical replication. | Yes -- current backup works. Add off-host push before going multi-node. |
| S5 | **Snapshot/restore operates on local Docker images** | `internal/services/services.go` -- snapshot tags images in `127.0.0.1:5000` registry | Restoring a snapshot on node B requires the image to exist in a registry accessible from node B. The current `127.0.0.1:5000` registry is loopback-only. | Replace loopback registry with a shared registry (e.g., `registry.internal:5000` on a dedicated host, or a cloud registry). All nodes push/pull from the same endpoint. | Yes -- snapshots are a convenience feature today. |

### 2.2 Routing & TLS

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| R1 | **Traefik runs as a single container on the same host** (`paas-traefik`) | `internal/docker/client.go` -- `findTraefikContainer()` looks for a container named `paas-traefik` on the local Docker daemon | A second control-plane node has its own Traefik. DNS still points to the original IP. There is no load balancer distributing traffic across nodes. | Put a TCP/L4 load balancer (HAProxy, cloud LB, or DNS round-robin) in front of N Traefik instances. Each node runs its own Traefik pointed at its local Docker. | Yes -- single Traefik handles thousands of concurrent connections. |
| R2 | **Traefik file-provider config is written to local filesystem** (`/etc/traefik/dynamic/{serviceID}.yml`) | `internal/services/services.go` -- `writeTraefikRoute()` writes YAML files atomically | If a service deploys on node A, the route file exists only on node A. Node B's Traefik has no route for that service. | (a) Use Traefik's Docker provider with Swarm labels instead of file provider, or (b) use a shared KV store (Consul/etcd) as the Traefik provider, or (c) replicate route files via a config sync daemon. | No -- this must be solved before a second node can route traffic. |
| R3 | **Let's Encrypt ACME state stored in a single file** (`/etc/traefik/certs/acme.json`) | `deploy/traefik/traefik.yml` -- `storage: /etc/traefik/certs/acme.json` | Each Traefik instance would independently attempt ACME challenges, potentially exhausting rate limits or failing challenges served by the wrong node. | Use Traefik's distributed ACME storage (Consul/etcd/Redis KV backend) so all nodes share certificate state. | No -- duplicate ACME attempts will cause certificate issuance failures within days. |
| R4 | **Service containers connect to a per-tenant Docker bridge network on one host** | `internal/docker/client.go` -- `RunContainer()` places containers on `ah-tenant-{id}` network; connects Traefik | Docker bridge networks are host-local. A container on node A cannot be reached via a bridge network on node B. | Use Docker Swarm overlay networks, or move to a CNI-based network (Flannel/Calico) with Kubernetes, or simply keep all tenant containers on one node and scale control-plane only. | Yes -- if containers stay on one node and only the API scales out. |

### 2.3 Work Queues & Background Jobs

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| W1 | **Reconciler runs as an in-process goroutine on every node** | `internal/reconciler/reconciler.go` -- `Run()` is a `go` goroutine; `cmd/ah/main.go` starts it unconditionally | Two nodes each run a reconciler. Both query the same DB and inspect the same Docker daemon (if shared) or different daemons (if separate hosts). Conflicting state mutations ensue: node A marks a service crashed while node B tries to restart it. | (a) Leader election (etcd lease, PostgreSQL advisory lock) so only one reconciler runs at a time, or (b) scope each reconciler to containers on its own host using a `node_id` column in the services table. | No -- running two reconcilers against the same state DB will cause data corruption within minutes. |
| W2 | **GC runs as an in-process goroutine, scans local Docker and local filesystem** | `internal/gc/gc.go` -- `Run()` scans local Docker containers, volumes, and `/var/lib/ah/builds` | Same dual-execution problem as W1. Two GC loops could race: one GC removes a container that the other GC's node just provisioned. Build dir cleanup is host-local and harmless in multi-node, but container/volume GC is dangerous. | Scope GC to local Docker daemon (safe if each node owns its containers). Add `node_id` awareness to avoid cross-node interference. | Partially -- GC against local Docker is safe if containers are node-affine. DB-driven GC needs coordination. |
| W3 | **Build queue is an in-memory Go channel** (`make(chan struct{}, 10)`) | `internal/builds/builds.go` -- `buildQueue` channel; `internal/builder/builder.go` -- `buildSem` channel (cap 3) | Two nodes each allow 10 queued + 3 concurrent builds independently, doubling the intended global limit. Per-tenant concurrency (`tenantBuilds` map) is process-local and will not prevent a tenant from running builds on both nodes simultaneously. | Use a distributed work queue (PostgreSQL `SKIP LOCKED` pattern, Redis queue, or NATS JetStream). The simplest: `SELECT ... FOR UPDATE SKIP LOCKED` on a `builds` table with `claimed_by` column. | Yes -- builds are relatively infrequent. Single-node queue is fine until build volume justifies distribution. |
| W4 | **Build log streaming uses in-memory subscriber channels** | `internal/builds/builds.go` -- `logSubs map[string][]chan string` | A `follow=true` log stream connected to node B will never receive lines from a build running on node A. | Publish log lines to a shared pub/sub (Redis Pub/Sub, NATS, or PostgreSQL LISTEN/NOTIFY). Subscribers receive lines regardless of which node runs the build. | Yes -- users can poll instead of streaming. Fix when UX matters. |
| W5 | **Stale build reconciliation on startup marks all pending/running builds as failed** | `internal/builds/builds.go` -- `reconcileStaleBuilds()` runs unconditionally at `NewManager()` | If node A restarts while node B is running a build, node A's startup will mark node B's in-progress build as failed. | Add a `node_id` column to builds. Only reconcile builds claimed by the restarting node. | No -- this will kill in-progress builds on other nodes on every restart. |

### 2.4 In-Memory Caches & State

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| C1 | **Auth cache is process-local** (LRU, 5000 entries, 30s TTL) | `internal/middleware/auth.go` -- `authCache` with LRU | Revoking a key on node A invalidates the local cache. Node B still serves from its stale cache for up to 30 seconds. Acceptable for revocation latency but the `InvalidateTenant()` call only reaches one node. | (a) Accept 30s revocation latency (already the design contract), or (b) use Redis as a shared cache, or (c) broadcast invalidation via pub/sub. | Yes -- 30s stale window is already the documented guarantee. |
| C2 | **Rate limiter is process-local** (in-memory token bucket per tenant + global) | `internal/middleware/ratelimit.go` -- `RateLimiter` with LRU; `GlobalRateLimiter` with single `rate.Limiter` | Each node independently allows 100 rps per tenant. With 2 nodes behind a load balancer, a tenant effectively gets 200 rps. The global limiter (500 rps) similarly doubles. | Use a shared rate limiter (Redis + sliding window, or a sidecar like Envoy with global rate limiting). Alternatively, divide limits by node count. | Yes -- 2x headroom is tolerable for the current tenant base. Fix when abuse prevention matters. |
| C3 | **Idempotency cache is process-local** (`sync.RWMutex` + map, 10min TTL) | `internal/middleware/idempotency.go` -- `IdempotencyStore` | A retried POST that lands on a different node will not find the cached response. The operation executes twice, breaking idempotency guarantees. | Store idempotency entries in the shared database (PostgreSQL) or Redis with TTL. | No -- broken idempotency causes duplicate resource creation (services, databases, builds). |
| C4 | **Deploy concurrency controlled by in-memory semaphore** | `internal/services/services.go` -- `deploySem` (cap 5) and `deployQueue` (cap 20) channels | Same as W3: each node independently allows 5 concurrent + 20 queued deploys, doubling the intended limits. | Use a distributed lock or `SKIP LOCKED` pattern for deploy slot acquisition. | Yes -- deploy contention is rare in practice. |

### 2.5 Identity & Secrets

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| I1 | **Bootstrap token is a single environment variable** (`AH_BOOTSTRAP_TOKEN`) | `cmd/ah/main.go` -- reads from `os.Getenv` | All nodes must share the same bootstrap token. This is already externalized (env var), so it works if deployment tooling propagates the value. | No code change needed. Ensure deployment tooling (Ansible, Terraform, systemd `EnvironmentFile`) sets the same token on all nodes. | N/A -- already works. |
| I2 | **Master key is read from a local file path** | `cmd/ah/main.go` -- `os.ReadFile(*masterKeyPath)` | See S3 above. The key must be identical on all nodes. File distribution is error-prone. | Accept from env var (`AH_MASTER_KEY`), falling back to file. | Yes -- operational concern, not a code blocker. |

### 2.6 Docker Daemon & Container Runtime

| # | Current Assumption | Where in Code | Why It Breaks at 2+ Nodes | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|---|
| D1 | **All containers run on the local Docker daemon** | `internal/docker/client.go` -- `NewClient()` uses `client.FromEnv` (default: local socket) | Node A cannot manage containers on node B's Docker daemon. A service created via the API on node A exists only on node A's Docker. | (a) Keep containers node-affine: add `node_id` to services table, route API requests to the owning node. (b) Use Docker Swarm mode for cross-node container scheduling. (c) Migrate to Kubernetes. | Partially -- option (a) is the lightest. Swarm/K8s is a larger commitment. |
| D2 | **Private registry is localhost-only** (`127.0.0.1:5000`) | `internal/builds/builds.go` -- `ImageTag()` hardcodes `127.0.0.1:5000`; `internal/builder/builder.go` pushes to it | Images built on node A are only available on node A. Deploying a service on node B from a build on node A fails because node B cannot pull from node A's loopback registry. | Run a shared registry on a stable internal hostname/IP accessible from all nodes. Update `ImageTag()` to use a configurable registry address. | No -- if builds and deploys can happen on different nodes, the registry must be shared. |
| D3 | **gVisor runtime assumed present on the local Docker daemon** | `cmd/ah/main.go` -- `VerifyGVisorRuntime()` checks local daemon | Each node must have gVisor installed. This is an operational requirement, not a code assumption. | Add gVisor to the base image/provisioning script for all nodes. | N/A -- operational, already works if provisioned correctly. |
| D4 | **Disk space checks reference local mount points** (`/var/lib/ah`, `/var/lib/docker`) | `internal/diskcheck/diskcheck.go`; called from builds, deploys, backups | Disk checks on node A say nothing about node B. If builds route to a node with a full disk, they fail. | Check disk on the node that will execute the work. If builds are node-affine, local checks remain correct. | Yes -- as long as work is node-affine. |

---

## 3. Recommended Phase-1 Architecture

### Goal

Support 2 control-plane nodes for API high availability without re-architecting the container runtime or build pipeline. Containers and builds remain single-node (node-affine).

### Architecture

```
                   +-----------+
                   |  Cloud LB |  (TCP passthrough or DNS round-robin)
                   +-----+-----+
                         |
              +----------+----------+
              |                     |
        +-----+-----+        +-----+-----+
        |  Traefik A |        |  Traefik B |
        +-----+-----+        +-----+-----+
              |                     |
        +-----+-----+        +-----+-----+
        |   ah (A)   |        |   ah (B)   |
        +-----+-----+        +-----+-----+
              |                     |
        +-----+-----+        +-----+-----+
        | Docker (A) |        | Docker (B) |
        +-----+-----+        +-----+-----+
              |                     |
              +----------+----------+
                         |
                  +------+------+
                  | PostgreSQL  |  (shared state DB, can be managed or self-hosted)
                  |  + Redis    |  (caches, rate limits, pub/sub)
                  +-------------+
                         |
                  +------+------+
                  | Shared      |  (registry.internal:5000 or cloud registry)
                  | Registry    |
                  +-------------+
```

### What Changes

1. **PostgreSQL replaces SQLite** for state and metering databases.
2. **Redis** provides shared rate limiting, idempotency cache, and auth cache invalidation pub/sub.
3. **Shared container registry** replaces `127.0.0.1:5000`.
4. **Traefik uses Consul/Redis KV** for dynamic route configuration and ACME certificate storage (replacing file provider and local `acme.json`).
5. **Leader election** (PostgreSQL advisory lock or Redis Redlock) gates the reconciler and GC so only one instance runs at a time.
6. **`node_id` column** added to `services`, `builds`, `databases` tables to track which host owns each container.
7. **Build stale-reconciliation scoped by `node_id`** to avoid killing builds on other nodes.

### What Stays the Same

- Single Go binary (`ah`), same API surface.
- Docker + gVisor container runtime (no Swarm, no Kubernetes).
- Nixpacks build pipeline.
- Per-tenant network isolation (bridge networks remain host-local; containers are node-affine).
- Master key distribution (env var on all nodes).
- Bootstrap token (env var on all nodes).

---

## 4. Migration Sequence

The migration is designed to be incremental. Each step is independently deployable and testable. The MVP continues to work at every intermediate state.

### Step 0: Interface Abstractions (no behavior change)

- Extract a `StateStore` interface from direct `*sql.DB` usage (or keep using `database/sql` since both SQLite and PostgreSQL speak SQL).
- Extract a `CacheStore` interface for rate limiting, idempotency, and auth caching.
- Extract a `RoutePublisher` interface from `writeTraefikRoute()`.
- Add a `RegistryAddr` config field (default `127.0.0.1:5000` for backward compatibility).

### Step 1: PostgreSQL Backend

- Add PostgreSQL connection support behind the existing `db.Open()` call (detect `postgres://` DSN vs file path).
- Migrate schema from SQLite DDL to PostgreSQL DDL (minor syntax differences: `INTEGER` -> `BIGINT` for timestamps, `AUTOINCREMENT` -> `SERIAL`, etc.).
- Add `node_id` column to `services`, `builds`, `databases`.
- Test with PostgreSQL in CI; keep SQLite as the dev/single-node default.

### Step 2: Shared Caches

- Implement Redis-backed `RateLimiter`, `IdempotencyStore`, and auth cache invalidation.
- Feature-flag: `AH_CACHE_BACKEND=memory|redis`. Default `memory` preserves MVP behavior.

### Step 3: Distributed Routing

- Implement a `RoutePublisher` that writes to Consul/etcd/Redis KV instead of local YAML files.
- Configure Traefik to read from the KV provider.
- Migrate ACME storage to the same KV backend.

### Step 4: Shared Registry

- Replace `127.0.0.1:5000` with a configurable `AH_REGISTRY_ADDR`.
- Deploy a shared registry instance accessible from all nodes.
- Update `ImageTag()` and builder push logic.

### Step 5: Leader Election for Background Jobs

- Implement leader election (PostgreSQL `pg_advisory_lock` or Redis Redlock).
- Gate reconciler and GC behind leader status.
- Scope stale build reconciliation to `node_id`.

### Step 6: Multi-Node Deployment

- Deploy second `ah` node with same config (PostgreSQL DSN, Redis addr, registry addr, master key).
- Put a load balancer in front of both Traefik instances.
- Verify: API requests route to either node; reconciler/GC run on one node only; builds/deploys are node-affine.

---

## 5. Full Gap Table (sorted by priority)

| # | Current Assumption | Why It Breaks | Smallest Viable Fix | Can Defer? |
|---|---|---|---|---|
| W1 | Reconciler is an in-process goroutine on every node | Two reconcilers mutate the same DB state concurrently | Leader election (PostgreSQL advisory lock) | **No** |
| C3 | Idempotency cache is process-local | Retries on a different node bypass dedup, creating duplicate resources | Redis-backed or PostgreSQL-backed idempotency store | **No** |
| R2 | Traefik routes are local YAML files | Routes only exist on the node that created them | KV-backed Traefik provider (Consul/etcd/Redis) | **No** |
| R3 | ACME cert state is a local file | Duplicate ACME challenges exhaust rate limits | Traefik distributed ACME storage | **No** |
| W5 | Stale build reconciliation kills all pending/running builds on startup | Node restart kills in-progress builds on other nodes | Scope reconciliation by `node_id` | **No** |
| D2 | Private registry is loopback-only | Node B cannot pull images built on node A | Shared registry on stable internal address | **No** |
| S1 | SQLite is the sole state DB | Single-writer file lock; no cross-host sharing | Swap to PostgreSQL | **No** (prerequisite for most other fixes) |
| S2 | Metering DB is a second local SQLite file | Same as S1 | Move to PostgreSQL or TimescaleDB | Yes |
| W2 | GC scans local Docker and local filesystem | Dual GC loops race on container removal | Scope to local Docker; add `node_id` awareness | Partially |
| W3 | Build queue is an in-memory channel | Global concurrency limits are per-node, not global | `SKIP LOCKED` pattern in PostgreSQL | Yes |
| W4 | Build log streaming uses in-memory channels | Log followers miss lines from other nodes | Redis Pub/Sub or NATS for log fan-out | Yes |
| C1 | Auth cache is process-local (30s TTL) | Key revocation is not propagated across nodes | Accept 30s stale window (already the contract) | Yes |
| C2 | Rate limiter is process-local | Effective rate doubles with 2 nodes | Redis sliding window or divide limits by node count | Yes |
| C4 | Deploy semaphore is in-memory | Concurrent deploy limits are per-node | Distributed lock or `SKIP LOCKED` | Yes |
| S3 | Master key is a local file | All nodes need the same key | Read from env var or secrets manager | Yes |
| S4 | Backups write to local filesystem | Only one node's backup is captured | Push to S3/object storage | Yes |
| S5 | Snapshot/restore uses loopback registry | Same as D2 | Same fix as D2 | Yes |
| R1 | Single Traefik container on one host | No traffic distribution | L4 load balancer in front of N Traefik instances | Yes |
| R4 | Tenant Docker networks are host-local | Containers on node A unreachable from node B's network | Keep containers node-affine; Traefik routes to the correct node | Yes |
| D1 | All containers on local Docker daemon | Cannot manage containers across hosts | `node_id` column; node-affine scheduling | Yes |
| D4 | Disk checks reference local mounts | Checks on node A say nothing about node B | Keep checks local if work is node-affine | Yes |
| I1 | Bootstrap token is an env var | Already works if propagated to all nodes | No code change | N/A |
| I2 | Master key is read from local file | Same as S3 | Same fix as S3 | Yes |
| D3 | gVisor assumed on local daemon | Operational, not code | Provision gVisor on all nodes | N/A |

---

## 6. Follow-Up Tickets

### Ticket 1: `feat(db): add PostgreSQL backend behind db.Store interface`
**Why**: PostgreSQL is the prerequisite for every multi-node fix. SQLite cannot be shared across hosts.
**Scope**: Add `postgres://` DSN detection in `db.Open()`. Translate SQLite-specific DDL (e.g., `AUTOINCREMENT`, `INTEGER` timestamps) to PostgreSQL equivalents. Add `node_id TEXT` column to `services`, `builds`, `databases` tables. Keep SQLite as the default for `--dev` mode and tests. Verify all existing queries work against both backends.
**Size**: M (1-2 days)

### Ticket 2: `feat(middleware): Redis-backed idempotency and rate limiting`
**Why**: Idempotency is broken the moment a second node serves traffic (duplicate resource creation). Rate limits double per node.
**Scope**: Implement `RedisIdempotencyStore` and `RedisRateLimiter` behind feature flags (`AH_CACHE_BACKEND=redis`). Add Redis connection management. Keep in-memory implementations as the default for single-node. Include integration tests using a test Redis instance.
**Size**: M (1-2 days)

### Ticket 3: `feat(routing): KV-backed Traefik provider and distributed ACME`
**Why**: Traefik file-provider routes and ACME state are purely local. A second node cannot serve traffic for services created on the first node.
**Scope**: Replace `writeTraefikRoute()` with a `RoutePublisher` that writes to Consul/etcd/Redis KV. Configure Traefik to use the KV provider. Migrate ACME storage to the same KV backend. Add `deleteTraefikRoute()` equivalent for the KV backend.
**Size**: M (1-2 days)

### Ticket 4: `feat(reconciler): leader election for background jobs`
**Why**: Two reconciler or GC loops running simultaneously against the same database will corrupt state (double crash counts, premature circuit opens, orphan removal of active containers).
**Scope**: Implement leader election using PostgreSQL `pg_advisory_lock` (preferred, no new dependency) or Redis Redlock. Gate reconciler and GC `Run()` behind leader status. Release leadership on graceful shutdown. Add `node_id` scoping to `reconcileStaleBuilds()` so restarts only affect local builds.
**Size**: S (0.5-1 day)

### Ticket 5: `feat(registry): configurable shared container registry`
**Why**: `127.0.0.1:5000` is unreachable from other nodes. Builds on node A produce images that node B cannot deploy.
**Scope**: Add `AH_REGISTRY_ADDR` config (default `127.0.0.1:5000` for backward compatibility). Update `ImageTag()` in `internal/builds/builds.go` and push logic in `internal/builder/builder.go`. Deploy a shared registry instance. Document TLS configuration for inter-node registry traffic.
**Size**: S (0.5 day)

### Ticket 6: `chore(infra): multi-node deployment playbook and CI verification`
**Why**: All the code changes above need a tested deployment path. Without a playbook, the second node is a manual, error-prone operation.
**Scope**: Ansible/Terraform playbook that provisions 2 nodes with shared PostgreSQL, Redis, registry, and load balancer. CI job that stands up a 2-node cluster (docker-compose or Kind) and runs integration tests to verify: API requests route to either node, reconciler runs on one node only, builds complete successfully, idempotency works across nodes.
**Size**: L (2-3 days)

---

## 7. Appendix: Files Referenced

| File | Single-host assumptions found |
|---|---|
| `cmd/ah/main.go` | Master key from local file, reconciler/GC started unconditionally, single Docker client |
| `cmd/ah/backup.go` | Backup to local filesystem, `VACUUM INTO` is SQLite-specific |
| `internal/db/db.go` | SQLite file path, WAL mode, `MaxOpenConns=4` |
| `internal/reconciler/reconciler.go` | Queries single DB + single Docker daemon, no leader election |
| `internal/gc/gc.go` | Scans local Docker, local build dirs, single DB |
| `internal/builds/builds.go` | In-memory build queue/subs, `127.0.0.1:5000` registry, startup stale reconciliation |
| `internal/builder/builder.go` | Local build dir, `systemd-run` scopes, push to loopback registry |
| `internal/middleware/auth.go` | Process-local LRU auth cache |
| `internal/middleware/ratelimit.go` | Process-local token bucket rate limiter |
| `internal/middleware/idempotency.go` | Process-local idempotency map |
| `internal/services/services.go` | Local Traefik YAML file writer, in-memory deploy semaphore, local disk checks |
| `internal/services/deploy_image.go` | Local Docker deploy, local disk checks, local Traefik route |
| `internal/docker/client.go` | Local Docker socket, loopback registry, host-local bridge networks |
| `internal/config/config.go` | All paths reference local filesystem |
| `internal/crypto/crypto.go` | No assumption (pure functions), but master key distribution is the concern |
| `deploy/traefik/traefik.yml` | Local file provider, local ACME JSON storage |
