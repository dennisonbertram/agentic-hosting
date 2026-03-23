# Instant Environments For AI Agent Worktrees

Date: 2026-03-22
Status: Investigation / architectural opinion

## Executive summary

My honest view:

- **Pre-warmed container pools are a useful tactic, but not the right primary abstraction** if the goal is "instant branch environments" in the general case.
- **They are a good fit only for a narrow thing**: a small pool of clean, homogeneous agent workspace sandboxes keyed by template and resource class.
- **They are a bad fit for full environment graphs** if "environment" means frontend + API + DB + preview URL + secrets + per-branch state, because Docker containers are mostly immutable after creation and the expensive parts are often not just `ContainerStart`.
- **The better product boundary is a first-class `environment` resource**, distinct from `service`, with explicit readiness phases: `shell_ready`, `preview_ready`, and `graph_ready`.
- **If "instant" means sub-second shell availability**, the current stack can get there incrementally.
- **If "instant" means sub-second fully healthy app graph**, Docker + Traefik + gVisor is probably the wrong long-term primitive unless you add snapshot/restore and volume cloning.

My recommendation is:

1. Build `environment` as a first-class resource, separate from `service`.
2. Define "instant" as `shell_ready`, not "everything is publicly healthy".
3. Use versioned **environment templates** plus a **small warm pool of clean workspace sandboxes**.
4. Make preview services and DB components **async child components** of an environment, not part of the fast-path readiness gate.
5. Fix the routing model for ephemeral previews before scaling this, because the current Traefik-per-tenant-network pattern will become the next bottleneck.
6. If this becomes a core product surface, invest next in **gVisor checkpoint/restore or runtime-level snapshots**, not in ever-larger idle container pools.
7. Only jump to Firecracker if the environments product becomes strategic enough to justify a new runtime and networking stack.

## What the repo actually does today

Some important reality checks from the codebase:

### 1. The system is a single-host Docker/gVisor PaaS, not an environment platform yet

`README.md` describes a single Go binary using Docker, gVisor, Traefik, Nixpacks, and SQLite on one host.

- `README.md`
- `internal/docker/client.go`
- `internal/services/services.go`

That matters because "instant environments" are closer to interactive compute than deploy-and-forget services. The repo already says that out loud in `docs/exploration/ah-vs-openshell.md`: `ah` is optimized for stateless services, while interactive sessions are a different problem class.

### 2. Current snapshots are deployment-state snapshots, not runtime snapshots

This is the most important semantic correction.

`internal/snapshots/snapshots.go` does **not** snapshot process memory, open sockets, tmpfs state, or a running container filesystem. It:

- tags the current image into the local registry,
- copies encrypted env vars,
- stores resource config and port.

That is valuable, but it is a **service template / deployment snapshot**, not a suspended execution snapshot.

Relevant code:

- `internal/snapshots/snapshots.go:1-140`
- `internal/api/services.go:99-180`

So `POST /v1/services?from_snapshot=...` is not "resume a frozen runtime". It is "create a new service using the saved image/env/port, then do a normal deploy."

### 3. Snapshot restore still pays most of the cold path

The service restore path in `internal/api/services.go` creates a new service from the snapshot, then launches the normal async deploy flow.

That deploy flow in `internal/services/services.go` still does:

1. ensure tenant network,
2. `PullImage`,
3. remove old container if needed,
4. `RunContainer`,
5. update DB,
6. write Traefik route.

Relevant code:

- `internal/api/services.go:31-180`
- `internal/services/services.go:567-724`

There is already a separate fast path for build outputs, `DeployImage()`, that skips `PullImage`.

- `internal/services/deploy_image.go:14-142`

That means there is at least one immediate optimization available: **snapshot-based restore should probably reuse a "local image already present" fast path instead of always going through the slower deploy path.**

### 4. "Running" is not the same as "ready"

For ordinary services, `agentic-hosting` marks a service `running` immediately after `ContainerStart`. There is no synchronous "wait until healthy" step in the normal service deploy path.

- `internal/services/services.go:686-724`
- `internal/docker/client.go:201-283`

By contrast:

- `internal/databases/databases.go` waits up to 30 seconds for Postgres/Redis health.
- `internal/kanbans/kanbans.go` has a 60 second health timeout and then setup work.

This is a useful precedent: the codebase already has the concept of **async readiness after initial allocation**.

### 5. `environment` is still only a proposal

There is no implemented `internal/environments/` package yet. The design lives in:

- `docs/architecture/dev-environments-mvp.md`

That doc already makes a key architectural decision I agree with:

- **environments should not be services with a writable flag**
- they should be a separate resource with their own lifecycle and API

That is the correct direction.

### 6. Current networking is the hidden scale wall

`RunContainer()` attaches service containers to per-tenant Docker networks and connects `paas-traefik` to each tenant network.

- `internal/docker/client.go:189-283`

The repo already contains an investigation showing this fanout is a real scaling limit:

- `docs/investigations/traefik-network-fanout.md`
- `internal/api/health.go:164-186`

That doc correctly points out that the practical limit is not just "tenants" but "every network Traefik has ever touched" unless cleanup is perfect. Ephemeral branch environments would multiply that cardinality.

## What "instant" should mean

The biggest product mistake here would be collapsing all readiness into one state.

For AI agent worktrees, there are really at least four milestones:

1. **Allocated**
   A resource slot exists and the scheduler has chosen a node.
2. **Shell ready**
   The agent can `exec`, read/write `/workspace`, and start working.
3. **Preview ready**
   The app process is reachable at a preview URL.
4. **Graph ready**
   All declared components (frontend, API, DB, migrations, seed jobs) are healthy.

If you optimize only for (4), you will miss the UX win.

For most coding agents, the valuable thing is:

- "give me a shell and a writable workspace now"

not:

- "block until every service in my branch stack has finished booting, migrating, seeding, and passing health checks"

My recommendation is to define the product promise as:

- **sub-second to low-single-digit-seconds for `shell_ready`**
- **best-effort async convergence for `preview_ready` and `graph_ready`**

## Is a pre-warmed container pool the right architecture?

### Short answer

**As the main abstraction: no.**

**As a tactical layer for clean workspace sandboxes: yes.**

### Why it helps

A warm pool can remove:

- image pull latency,
- Docker create/start overhead,
- some gVisor startup overhead,
- "install the same base tools every time" overhead,
- part of the perceived cold-start cost.

This is especially useful for:

- a fixed Ubuntu/Devbox-style workspace image,
- one or a few language templates,
- small pools on a single host,
- agent shells that start from `sleep infinity` and then use `exec`.

### Why it is the wrong primary abstraction

#### 1. Docker bakes too much in at create time

A pre-created container already has:

- mounts,
- env vars,
- network attachments,
- resource limits,
- hostname/container name,
- port bindings,
- labels.

Those are awkward or impossible to late-bind cleanly after creation.

This means a pool of "idle containers from base images" only works well if the containers are intentionally very generic. It does **not** generalize well to:

- per-branch secrets,
- per-branch mounts,
- per-branch preview ports,
- per-branch route wiring,
- environment graphs with multiple components.

#### 2. It does not solve application boot time

A warm empty shell is one thing.

A warm frontend + API + DB graph is another.

If the slow part is:

- app boot,
- migrations,
- package install,
- database recovery,
- health grace windows,
- route publication,

then a generic warm pool only removes a slice of the end-to-end latency.

#### 3. It creates a memory-pressure tax

Warm pools trade latency for resident memory and page cache pressure.

For AI environments that may install toolchains, clone repos, and hold caches, this can get expensive quickly. Pool count grows by:

- template version,
- resource class,
- architecture,
- possibly tenant or org policy,
- node count.

That explodes faster than people expect.

#### 4. Reusing a previously used warm container is a security bug

If a container has ever seen:

- tenant secrets,
- repo contents,
- SSH keys,
- shell history,
- package caches,
- tmpfs contents,
- language caches,

then returning it to a general pool is not safe unless you can prove complete scrubbing. In practice, the safe choice is:

- destroy it, or
- restore from a known-clean checkpoint/snapshot.

#### 5. It makes the current routing problem worse

If every ephemeral environment gets its own network and preview route, the current Traefik fanout model becomes the next bottleneck even if container startup becomes fast.

### My verdict

Use warm pools only for:

- **clean, generic workspace sandboxes**
- **small per-template pools**
- **fast shell allocation**

Do **not** use warm pools as the main primitive for full graph environments.

## A better architecture: templates + warm sandboxes + async graph hydration

If I were designing this in this repo, I would aim for this model:

### 1. First-class `environment` resource

Keep the separation proposed in `docs/architecture/dev-environments-mvp.md`.

An environment is:

- a workspace sandbox for the agent,
- optional child components,
- explicit TTL / lease semantics,
- explicit readiness phases.

It should not be modeled as "just another service".

### 2. Versioned environment templates

Templates should define:

- base workspace image or runtime profile,
- optional preinstalled toolchain layer,
- egress policy,
- preview component graph,
- secrets injection policy,
- resource class,
- optional seed or migration jobs.

Critically, the pool key should be **template-versioned**. If the template changes, the old pool should drain and a new pool should fill.

### 3. Small warm pools of clean workspace sandboxes

Keep a small per-node pool for hot templates.

Each pooled item should be:

- created from a versioned template,
- attached to a fresh empty workspace volume,
- contain no tenant secrets,
- contain no user code,
- be safe to assign to exactly one environment.

That gives you fast `shell_ready`.

### 4. Async workspace sync

The worktree problem is separate from the runtime problem.

If a local agent spawns a git worktree and expects a matching cloud runtime instantly, you still need to answer:

- how does code get there?
- how do uncommitted local changes get there?

Possible sync strategies:

- clone a remote branch/tag,
- upload a tarball of the worktree,
- upload only a Git patch / diff,
- sync incrementally after allocation.

My recommendation:

- allocate the environment first,
- make it `shell_ready`,
- then stream or sync the worktree asynchronously,
- then start preview components.

This keeps the control-plane latency low even when code sync is not zero-cost.

If you skip this design question, "instant environment" silently degrades into "instant clone of the last pushed branch", which is not the same thing as "matching cloud runtime for the current worktree."

### 5. Async child components for preview stacks

If the template includes frontend + API + DB, those should be child components of the environment instance with independent readiness.

The API should make it obvious when:

- the workspace is ready,
- the DB is ready,
- the API is ready,
- the preview URL is live.

That is a much better model than pretending one boolean status can represent all of it.

## Hard problems you should expect

## 1. Networking and ingress

This is the hardest operational problem in your current design.

The current service model does this:

- one tenant bridge network per tenant,
- `paas-traefik` attached to each network,
- file-provider route per service.

That is already called out as a scaling wall in `docs/investigations/traefik-network-fanout.md`.

Ephemeral worktree environments make this much worse because the cardinality is no longer "active tenants". It becomes closer to:

- active branches,
- active worktrees,
- active preview environments,
- plus cleanup lag.

### My opinion

For high-churn ephemeral environments, **do not use the current Traefik-per-network pattern as the main ingress primitive.**

Better options:

1. **Host-loopback port mapping + preview proxy in `ah`**
   The environment or preview component binds to a host loopback port. `ah` (or a small dedicated preview proxy) routes preview requests to that port.

2. **Traefik to host-loopback targets, not tenant networks**
   Still possible, but you avoid attaching Traefik to every ephemeral bridge.

3. **Shared preview bridge + L7 routing**
   Better than one bridge per environment, but weaker isolation than your current model.

For ephemeral previews, I would strongly prefer a proxy model that does **not** require Traefik to join every environment network.

## 2. Stateful component cloning

The moment templates include a DB, you have a second startup problem:

- cloning state quickly

Your current DB model provisions a dedicated Postgres/Redis container and waits for health:

- `internal/databases/databases.go:137-317`
- `internal/docker/client.go:547-607`

That is fine for ordinary managed databases. It is not a cheap primitive for "spawn 20 branch environments now".

### My opinion

For environment graphs, do not default to "new database container per branch".

Prefer, in order:

1. **logical clones** inside a shared database
   - branch schema,
   - branch database,
   - tenant-scoped namespace.

2. **filesystem-level volume snapshots**
   - LVM thin clones,
   - ZFS clones,
   - btrfs subvolume snapshots.

3. **full DB containers per environment only when unavoidable**

Docker named volumes by themselves are not a great instant-clone primitive.

## 3. Security and isolation

The current system is security-conscious:

- gVisor runtime,
- dropped capabilities,
- read-only rootfs,
- no-new-privileges,
- encrypted secrets.

That should stay.

But instant environments introduce new leak paths:

- warm pool residue,
- workspace volume reuse,
- cached package credentials,
- git credentials,
- SSH agent forwarding,
- long-lived secrets in pooled sandboxes,
- branch previews exposed on the public internet.

### Principles I would enforce

1. **No tenant secrets in the pool**
2. **No code in the pool**
3. **Every allocated workspace volume is single-tenant and single-environment**
4. **Returned environments are destroyed, not recycled**
5. **Secrets are injected only after allocation**
6. **TTL deletion wipes workspace state**

## 4. Outbound internet / egress policy

The current service networks are `Internal=true`, which means "no outbound internet" is part of the current design.

- `internal/docker/client.go:101-130`

That is reasonable for deployed services. It is probably a product non-starter for dev environments, because agents need to:

- `git fetch`,
- `npm install`,
- `pip install`,
- `apt-get`,
- hit docs or APIs.

The repo already has a good direction for build-time egress control in:

- `docs/security/build-egress-allowlist.md`

I would reuse the same idea for environments with per-template egress profiles:

- `none`
- `allowlisted`
- `full`

If you skip this, "instant environments" quickly become "instant exfiltration sandboxes."

## 5. gVisor filesystem and interactive workload behavior

The current platform is optimized for services, not for filesystem-heavy interactive workloads.

That matters because agent worktrees are very filesystem-heavy:

- `git status`,
- `npm install`,
- file watchers,
- compilers,
- lots of small file metadata operations.

This does not mean gVisor is wrong. It means you should benchmark the real dev loop, not just startup.

Official gVisor docs are relevant here:

- gVisor says container startups are fast and much of startup overhead is Docker itself.
- gVisor also documents rootfs overlay and snapshot features that can help preserve warmed filesystem state.

## How this interacts with gVisor startup characteristics

Two official gVisor facts matter a lot:

1. The gVisor site says containers "start up in milliseconds".
2. The gVisor performance guide says most startup overhead in the startup benchmark is associated with Docker itself.

Sources:

- https://gvisor.dev/
- https://gvisor.dev/docs/architecture_guide/performance/

That leads to an important conclusion:

**If your measured latency is tens of seconds, gVisor container boot is probably not the main problem.**

The bigger sources are likely:

- Docker object creation and orchestration,
- image pull,
- worktree sync,
- app boot,
- health gating,
- route publication,
- DB provisioning or migrations.

So the right question is not:

- "how do we make runsc boot faster?"

It is:

- "how do we remove slow steps from the synchronous path?"

### Practical implication

Do not over-invest in pool complexity until you have measured:

- `pull_image_ms`
- `container_create_ms`
- `container_start_ms`
- `shell_ready_ms`
- `preview_ready_ms`
- `graph_ready_ms`
- `route_publish_ms`
- `workspace_sync_ms`

The repo already has at least one easy improvement:

- snapshot restores can likely skip a full `PullImage` path more often.

## Better primitives than Docker container pools

## 1. Prebuilt template images

This is the lowest-risk improvement.

Instead of starting from `ubuntu:24.04` and then doing `apt-get` on the hot path, create versioned dev images:

- `ah-env/node-ts:v3`
- `ah-env/python:v5`
- `ah-env/go:v2`

Pre-pull them on each node. This eliminates most "install the same tools every time" delay without inventing a pool yet.

## 2. gVisor rootfs tar snapshots

This is more interesting than a generic warm pool.

gVisor documents "rootfs tar snapshots" that capture rootfs changes from one container and apply them to a new sandbox at creation time.

Source:

- https://gvisor.dev/docs/user_guide/rootfs_snapshot/

This is attractive because it preserves a warmed filesystem layer while still letting you create a **new** sandbox with fresh env vars, mounts, and networking.

That is often better than reusing an already-created container.

## 3. gVisor checkpoint/restore

This is the most relevant alternative if you want truly fast warm resume without adopting a new runtime.

gVisor explicitly supports checkpoint/restore, and its site even calls out branching interactive REPL sessions as a use case.

Sources:

- https://gvisor.dev/
- https://gvisor.dev/docs/user_guide/checkpoint_restore/

However, there is a catch that matters a lot for this codebase:

- Docker does not cleanly support restoration into new containers for migration-style flows.
- gVisor docs say the more flexible flow is via raw `runsc` commands.

So if you pursue this seriously, you probably need:

- raw `runsc`, or
- a containerd-based backend,

not just more Docker Engine API glue.

### My opinion

If instant environments becomes a major feature, **containerd + runsc** is a more compelling medium-term direction than "Docker + bigger pools", and much less disruptive than "rewrite everything around Firecracker right now."

## 4. Firecracker microVM snapshots

Firecracker is the strongest long-term alternative if you want:

- stronger isolation,
- fast resume via VM snapshots,
- more serverless-like branch environments.

But it is a major platform change. Your own `docs/architecture/firecracker-integration-plan.md` already captures most of that complexity.

Official snapshot docs add more caveats:

- restoring requires matching host resources and disk/TAP setup,
- clones from the same snapshot can duplicate entropy, identifiers, and tokens,
- you need to think about VMGenID, clock shift, and network identity.

Source:

- https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md

### My opinion

Firecracker is only worth it if:

- environments become a core product,
- you need a stronger isolation boundary than gVisor,
- you are willing to own the networking and snapshot stack,
- and you actually plan to use snapshot/restore.

If you are **not** using snapshot/restore, Firecracker by itself is unlikely to justify the integration cost over your current stack.

## 5. WASM

WASM is not the right primitive for this use case.

Why:

- arbitrary git worktrees expect a Linux userspace,
- package managers expect a Linux userspace,
- compilers and shells expect a Linux userspace,
- many frameworks assume normal processes, files, sockets, and subprocesses.

WASM can be excellent for narrow sandboxed execution. It is not a general replacement for "give an agent a real dev environment".

## What I would build first

### Phase 1: make "shell ready" fast on the current stack

1. Implement `environment` as a first-class resource, separate from services.
2. Add explicit states:
   - `allocating`
   - `shell_ready`
   - `workspace_syncing`
   - `components_starting`
   - `preview_ready`
   - `graph_ready`
   - `hibernated`
   - `expired`
   - `failed`
3. Ship versioned environment templates backed by prebuilt images.
4. Keep a small per-node warm pool only for clean workspace sandboxes.
5. Sync worktree contents after allocation.
6. Start preview components asynchronously.
7. Add TTL/lease semantics from day one.
8. Reuse existing idempotency patterns on create.

### Phase 2: fix the ingress model for ephemeral previews

1. Stop assuming Traefik should join every ephemeral network.
2. Add a preview proxy path that targets host-loopback ports or another non-fanout transport.
3. Use wildcard certs or shared preview domains. Do not issue one ACME flow per short-lived branch if you can avoid it.

### Phase 3: replace idle pools with snapshot-driven warm starts

If Phase 1 proves demand and latency still matters, investigate:

1. gVisor rootfs snapshot-based templates
2. gVisor checkpoint/restore
3. containerd as the environment runtime backend

That gives you a more robust "warm start" primitive than keeping many idle containers around forever.

### Phase 4: only then consider Firecracker

Move to Firecracker only if the data says:

- environment isolation is the new core value,
- gVisor + snapshots are insufficient,
- and you are ready to own a new runtime backend.

## API shape I would recommend

### Create from template

```http
POST /v1/environments
Idempotency-Key: ...
Content-Type: application/json

{
  "name": "feat-branch-123",
  "template_id": "tmpl_node-web_v3",
  "git": {
    "repo": "https://github.com/org/repo.git",
    "ref": "refs/heads/feature/branch",
    "patch_upload": false
  },
  "ttl_seconds": 3600,
  "idle_timeout_seconds": 900,
  "wait_for": "shell_ready"
}
```

Response:

```json
{
  "id": "env_abc123",
  "status": "shell_ready",
  "shell_ready": true,
  "preview_ready": false,
  "graph_ready": false,
  "workspace_path": "/workspace",
  "expires_at": 1770000000,
  "components": [
    {"name": "workspace", "status": "running"},
    {"name": "api", "status": "starting"},
    {"name": "db", "status": "starting"}
  ]
}
```

### Lease / TTL extension

```http
POST /v1/environments/{id}/lease
{
  "extend_by_seconds": 1800
}
```

### Workspace sync

```http
POST /v1/environments/{id}/workspace/sync
```

This can later support:

- tar upload,
- patch upload,
- rsync-like delta,
- clone-by-ref only.

### Exec

```http
POST /v1/environments/{id}/exec
{
  "command": ["bash", "-lc", "git status --short"]
}
```

### Preview info

```http
GET /v1/environments/{id}
```

Should return:

- shell readiness,
- preview URLs,
- child component status,
- expiry metadata.

## Operational gotchas at scale

## 1. Pool sizing

Do not size pools by gut feel.

Use:

- recent arrival rate per template,
- cold-start p95,
- memory budget,
- max pool caps per node.

Refill pools asynchronously with hysteresis. Never refill synchronously on every checkout allocation.

## 2. Thundering herd

Agent systems can stampede:

- swarm spin-up,
- CI fan-out,
- many branches after a rebase,
- restart storms after control-plane restart.

You need:

- per-tenant caps,
- per-template caps,
- fallback to cold path,
- explicit backpressure,
- jittered refill.

## 3. Memory pressure

Warm pools are effectively a latency cache paid for in RAM.

Track:

- pool occupancy,
- warm hit rate,
- eviction count,
- page cache pressure,
- swap pressure,
- OOM kills.

## 4. Disk pressure

Snapshots, rootfs tar layers, workspaces, and cloned DB state can become a DoS vector quickly.

Firecracker docs explicitly warn integrators to enforce disk quotas for snapshots. The same lesson applies here.

## 5. Template invalidation

Any change to:

- image digest,
- toolchain layer,
- runtime flags,
- security policy,
- egress policy,
- template graph,

must produce a new pool key.

Otherwise you get heisenbugs from half-stale warm sandboxes.

## 6. Secret rotation

Warm environments and hibernated environments must respect:

- credential rotation,
- API token revocation,
- env var updates,
- SSH key updates.

Never assume a long-lived warm sandbox can keep stale secrets safely.

## 7. Route churn

If every environment creates and deletes preview routes rapidly, Traefik file-provider churn itself becomes an operational factor even before networking fanout does.

That is another reason to prefer a dedicated preview routing layer for ephemeral envs.

## 8. Metrics cardinality

Environment IDs, branch names, and worktree paths can explode metric cardinality and log volume.

Emit:

- per-template metrics,
- per-node metrics,
- sampled environment metrics,

not only per-environment labels everywhere.

## Final opinion

The current stack can absolutely support a compelling "instant environments" feature, but only if you narrow the promise:

- **instant shell, async preview**

Trying to make "frontend + API + DB all fully healthy in sub-second time" the initial target will push you into over-complicated pooling and make the current networking model fall over.

So my blunt recommendation is:

- **Yes** to TTL auto-teardown. That is table stakes.
- **Yes** to environment templates. That is the right product primitive.
- **Yes** to a small warm pool, but only for clean workspace sandboxes.
- **No** to making "idle Docker containers from base images" the core architecture for full graph environments.
- **Yes** to measuring the synchronous path and removing slow steps before changing runtimes.
- **Yes** to considering gVisor snapshot primitives before a Firecracker rewrite.
- **Maybe later** to Firecracker, but only if environments become central enough to justify owning the complexity.

If I had to condense it to one sentence:

**Build `environment` as a first-class, lease-based, template-driven workspace resource, optimize for `shell_ready`, and treat snapshot/restore as the long-term answer - not bigger idle pools.**

## External references

- gVisor home: https://gvisor.dev/
- gVisor performance guide: https://gvisor.dev/docs/architecture_guide/performance/
- gVisor checkpoint/restore: https://gvisor.dev/docs/user_guide/checkpoint_restore/
- gVisor rootfs tar snapshots: https://gvisor.dev/docs/user_guide/rootfs_snapshot/
- Firecracker snapshot support: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
