# Dev Environments MVP

**Status**: Proposed
**Issue**: #41
**Date**: 2026-03-20

---

## Problem

AI agents using agentic-hosting can deploy services and provision databases, but they have no interactive workspace. An agent that needs to inspect files, run ad-hoc commands, debug a deployment, or prototype code has no way to do so within the platform. Dev environments fill this gap: a long-lived, writable container the agent can shell into.

## Design Principles

1. **Smallest possible surface.** Ship the fewest endpoints, the simplest container config, and the least new infrastructure that lets an agent create a container, run a command in it, and delete it.
2. **Reuse existing patterns.** Follow the same resource lifecycle as services and databases (SQLite row + Docker container + reconciler awareness).
3. **Defer everything optional.** If the first demo works without it, it is not in the MVP.

---

## MVP Scope

### One Base Image

The MVP supports exactly one base image: **`ubuntu:24.04`**.

Why Ubuntu over a language-specific image:
- Agents work in many languages; picking `node:20` or `python:3.12` forces a choice that does not generalize.
- Ubuntu 24.04 includes `apt`, so agents can install whatever toolchain they need inside the running container.
- It avoids maintaining an allowlist of language images and the version-matrix complexity that entails.

The image is hardcoded in the environment manager. A future follow-up can add a `base_image` field with an allowlist.

### One Persistence Model

Each environment gets a single Docker named volume mounted at **`/workspace`**. This is the only writable, persistent path. The rest of the filesystem is read-only (matching service container security posture) except for standard tmpfs mounts (`/tmp`, `/var/run`, `/var/tmp`, `/run`).

Volume naming convention: `ah-env-{environmentID}` (parallels `ah-db-{databaseID}` for databases).

Data lifecycle: the volume is destroyed when the environment is deleted. There is no backup, snapshot, or export mechanism in the MVP.

### CRUD Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/environments` | Create environment |
| GET | `/v1/environments` | List environments (paginated) |
| GET | `/v1/environments/{id}` | Get environment |
| DELETE | `/v1/environments/{id}` | Delete environment (stops container, removes volume) |
| POST | `/v1/environments/{id}/start` | Start a stopped environment |
| POST | `/v1/environments/{id}/stop` | Stop a running environment |

**Request body for POST /v1/environments:**

```json
{
  "name": "my-workspace"
}
```

No `image`, `port`, or `env` fields in the MVP. The image is always `ubuntu:24.04`. Environment variables can be added as a follow-up.

**Response body (all endpoints returning an environment):**

```json
{
  "id": "env_abc123",
  "tenant_id": "t_xyz",
  "name": "my-workspace",
  "status": "running",
  "container_id": "sha256:...",
  "created_at": 1711000000,
  "updated_at": 1711000000
}
```

Status values: `creating`, `running`, `stopped`, `failed`.

### One Exec Model

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/environments/{id}/exec` | Execute a command, return output |

The MVP uses a **synchronous HTTP exec**, not WebSockets. The agent sends a command, the server runs it inside the container via the Docker exec API, and returns stdout/stderr when the command completes (or times out).

**Request:**

```json
{
  "command": ["ls", "-la", "/workspace"]
}
```

**Response:**

```json
{
  "exit_code": 0,
  "stdout": "total 4\ndrwxr-xr-x 2 root root 4096 ...\n",
  "stderr": ""
}
```

Timeout: 60 seconds. Commands that exceed this are killed and return exit code 137.

Why synchronous HTTP instead of WebSockets:
- No Traefik websocket routing work required.
- No new client-side dependencies (agents already speak HTTP).
- Sufficient for the demo: agents issue discrete commands, not interactive terminal sessions.
- WebSocket exec is a follow-up for interactive use cases.

**Docker implementation:** Uses `ContainerExecCreate` + `ContainerExecAttach` + `ContainerExecInspect` from the Docker Engine API. Two new methods on the `docker.Client` interface:

```go
ExecCreate(ctx context.Context, containerID string, cmd []string) (string, error)
ExecRun(ctx context.Context, execID string) (stdout []byte, stderr []byte, exitCode int, err error)
```

---

## How Environments Differ from Services

| Dimension | Service | Environment |
|-----------|---------|-------------|
| **Purpose** | Run a long-lived network service (web app, API) | Interactive workspace for an agent |
| **Image source** | User-specified (Docker Hub, Nixpacks build) | Platform-fixed (`ubuntu:24.04`) |
| **Network exposure** | Routed via Traefik (public HTTPS) | No Traefik routing, no public URL |
| **Filesystem** | ReadonlyRootfs, no persistent volume | ReadonlyRootfs + persistent `/workspace` volume |
| **Interaction model** | HTTP requests to the running service | Exec commands into the container |
| **Lifecycle** | Managed by reconciler (crash recovery, circuit breaker) | Simple start/stop, no circuit breaker |
| **Build pipeline** | Nixpacks or pre-built image | No build step; image is pre-pulled |

Key architectural decision: environments are **not** services with extra features. They are a separate resource type with their own DB table, manager, and API routes. This avoids polluting the service model with workspace-specific concerns (writable volumes, exec, no routing).

However, environments share the same underlying Docker infrastructure:
- gVisor (runsc) runtime for isolation
- Per-tenant Docker network (so environments can reach tenant databases/services on the internal network)
- Same resource limit enforcement from `tenant_quotas`
- Same GC awareness (orphan detection via `ah.environment` label)

---

## Schema

### Migration: `state_013_environments.sql`

```sql
CREATE TABLE IF NOT EXISTS environments (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'creating',
    container_id TEXT,
    volume_name TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_environments_tenant ON environments(tenant_id);
```

Notes:
- No `image` column in MVP (hardcoded). Add it when the allowlist follow-up ships.
- No `port` column (environments are not network-routable).
- No `idle_since` or `idle_timeout` columns (idle auto-stop is deferred).
- `volume_name` stores `ah-env-{id}` for GC to identify orphaned volumes.

### Quota extension

Add `max_environments` to `tenant_quotas` (default: 2). This can be a separate migration or bundled:

```sql
ALTER TABLE tenant_quotas ADD COLUMN max_environments INTEGER NOT NULL DEFAULT 2;
```

---

## Container Configuration

```go
hostCfg := &container.HostConfig{
    Runtime: "runsc",
    Resources: container.Resources{
        Memory:     512 * 1024 * 1024,  // 512 MB
        NanoCPUs:   1_000_000_000,      // 1 CPU
        PidsLimit:  int64Ptr(256),
        MemorySwap: 512 * 1024 * 1024,  // no swap
    },
    RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
    NetworkMode:    container.NetworkMode(tenantNet),
    CapDrop:        []string{"ALL"},
    SecurityOpt:    []string{"no-new-privileges"},
    ReadonlyRootfs: true,
    Tmpfs: map[string]string{
        "/tmp":     "rw,noexec,nosuid,size=64m",
        "/var/run": "rw,noexec,nosuid,size=16m",
        "/var/tmp": "rw,noexec,nosuid,size=16m",
        "/run":     "rw,noexec,nosuid,size=16m",
    },
    Mounts: []mount.Mount{
        {
            Type:   mount.TypeVolume,
            Source: "ah-env-{id}",
            Target: "/workspace",
        },
    },
}

containerCfg := &container.Config{
    Image:  "ubuntu:24.04",
    Cmd:    []string{"sleep", "infinity"},  // keep container alive
    Labels: map[string]string{
        "ah.tenant":      tenantID,
        "ah.environment": envID,
        "ah.type":        "environment",
        "traefik.enable": "false",
    },
    WorkingDir: "/workspace",
}
```

The container runs `sleep infinity` to stay alive. Agents interact via exec. This is the simplest possible entrypoint that keeps the container in a running state.

---

## Reconciler and GC Integration

### Reconciler

Minimal integration for MVP:
- If the DB says an environment is `running` but the container is gone, set status to `stopped`.
- If the DB says an environment is `creating` for more than 5 minutes, set status to `failed`.
- No crash counting or circuit breaker for environments (they run `sleep infinity`; they do not crash under normal operation).

### GC

- Detect orphaned environment containers via `ah.type=environment` label with no matching DB row.
- Detect orphaned volumes with `ah-env-` prefix not referenced by any environment row.

---

## First End-to-End Demo

The demo proves that an AI agent can use the platform as a scratchpad:

```bash
# 1. Create an environment
curl -X POST https://api.agentic.hosting/v1/environments \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "scratch"}'
# Returns: {"id": "env_abc123", "status": "creating", ...}

# 2. Wait for it to be running (poll GET)
curl https://api.agentic.hosting/v1/environments/env_abc123 \
  -H "Authorization: Bearer $TOKEN"
# Returns: {"status": "running", ...}

# 3. Run a command
curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["echo", "hello from workspace"]}'
# Returns: {"exit_code": 0, "stdout": "hello from workspace\n", "stderr": ""}

# 4. Install a tool and use it
curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["apt-get", "update"]}'

curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["apt-get", "install", "-y", "python3"]}'

curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["python3", "-c", "print(sum(range(100)))"]}'
# Returns: {"exit_code": 0, "stdout": "4950\n", "stderr": ""}

# 5. Verify persistence
curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["sh", "-c", "echo hello > /workspace/test.txt && cat /workspace/test.txt"]}'
# Returns: {"exit_code": 0, "stdout": "hello\n", "stderr": ""}

# 6. Stop and restart — data persists
curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/stop \
  -H "Authorization: Bearer $TOKEN"

curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/start \
  -H "Authorization: Bearer $TOKEN"

curl -X POST https://api.agentic.hosting/v1/environments/env_abc123/exec \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"command": ["cat", "/workspace/test.txt"]}'
# Returns: {"exit_code": 0, "stdout": "hello\n", "stderr": ""}

# 7. Clean up
curl -X DELETE https://api.agentic.hosting/v1/environments/env_abc123 \
  -H "Authorization: Bearer $TOKEN"
```

**Success criteria for the demo:**
1. Create returns a running environment within 30 seconds.
2. Exec returns correct stdout/stderr for simple commands.
3. Files written to `/workspace` survive stop/start.
4. Delete removes the container and volume.
5. The environment can reach tenant databases on the internal network.

---

## Explicitly Deferred

These features are **not** in the MVP. Each is a separate follow-up issue.

| Feature | Why Deferred | Follow-up Trigger |
|---------|-------------|-------------------|
| **File upload/download API** | Agents can use exec with `cat`/`base64` for small files. A proper multipart upload API adds complexity. | When agents need to transfer files larger than ~64KB reliably. |
| **Idle auto-stop** | Requires an idle-detection goroutine, `idle_since` column, and timer management. The MVP has at most 2 environments per tenant. | When environment resource consumption becomes a concern. |
| **Multiple language templates** | One image is sufficient for the demo. Allowlist adds validation, documentation, and image-pull complexity. | When agents consistently need pre-installed toolchains. |
| **WebSocket exec** | Requires Traefik websocket routing configuration and a bidirectional stream handler. Synchronous exec covers the primary use case. | When agents need interactive terminal sessions or long-running streaming output. |
| **Snapshot/restore** | Already exists for services; adapting it for environments is straightforward but not needed for the demo. | When agents need to checkpoint and rollback workspace state. |
| **Environment variables** | Agents can export vars in exec commands. A dedicated env var API duplicates service_env infrastructure for minimal gain. | When agents need persistent env vars that survive container recreation. |
| **Custom resource limits** | MVP uses fixed 512MB/1CPU. Per-environment limits add quota calculation complexity. | When tenants need differently-sized environments. |

---

## Implementation Checklist

Ordered by dependency:

1. **`internal/db/migrations/state_013_environments.sql`** -- create `environments` table and quota column.
2. **`internal/docker/client.go`** -- add `ExecCreate` and `ExecRun` methods to the `Client` interface and `DockerClient` implementation.
3. **`internal/environments/environments.go`** -- environment manager (Create, Get, List, Delete, Start, Stop, Exec).
4. **`internal/api/environments.go`** -- HTTP handlers wired into the chi router.
5. **`internal/reconciler/reconciler.go`** -- add environment stale/missing-container checks.
6. **`internal/gc/gc.go`** -- add orphaned environment container and volume cleanup.
7. **`internal/testutil/`** -- mock the new Docker exec methods.
8. **Tests** -- `internal/environments/environments_test.go`, `internal/api/environments_test.go`.

Estimated size: ~500-700 lines of new Go code, ~200 lines of tests.
