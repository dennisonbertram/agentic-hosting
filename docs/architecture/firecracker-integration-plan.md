# Firecracker Integration Plan — from gVisor to microVMs

> **Status**: Spike / Design Document
> **Issue**: #1
> **Date**: 2026-03-20

---

## 1. Current Docker/gVisor Assumptions

The platform currently runs every tenant workload as a Docker container under the gVisor (`runsc`) runtime. The following assumptions are woven throughout the codebase:

### 1.1 Runtime & Isolation (`internal/docker/client.go`)

| Assumption | Where | Detail |
|---|---|---|
| gVisor is the only runtime | `RunContainer` hardcodes `Runtime: "runsc"` | Also enforced at startup by `VerifyGVisorRuntime` |
| Docker Engine API is the sole orchestration surface | `DockerClient` wraps `client.Client` from `github.com/docker/docker` | Every container operation (create, start, stop, remove, inspect, logs) goes through the Docker socket |
| OCI images are the only artifact format | `PullImage`, `TagImage`, `RemoveImage` | Nixpacks build pipeline also produces OCI images pushed to a local registry |
| Container naming is deterministic | `containerName()` returns `ah-<tenantID>-<serviceID>` | Used for stop-before-recreate and Traefik routing |
| Security posture is defense-in-depth | `CapDrop: ALL`, `no-new-privileges`, `ReadonlyRootfs`, `PidsLimit=256`, `MemorySwap=Memory` | gVisor intercepts syscalls; these flags add Linux-kernel-level constraints |

### 1.2 Networking (`internal/docker/client.go`, `internal/services/services.go`)

| Assumption | Where | Detail |
|---|---|---|
| Per-tenant Docker bridge networks | `EnsureNetwork` creates `ah-tenant-<id>` with `Internal=true`, `ICC=false` | Cross-tenant isolation via separate L2 bridges |
| Traefik is connected to each tenant network | `RunContainer` finds `paas-traefik` by name and calls `NetworkConnect` | Traefik gains L2 adjacency for ingress |
| Routing via Traefik file provider | `writeTraefikRoute` writes YAML to `/etc/traefik/dynamic/` | Routes use container name as upstream (`http://ah-<t>-<s>:<port>`) |
| No outbound internet for containers | `Internal: true` on all tenant networks | Containers cannot reach external hosts |

### 1.3 Service Lifecycle (`internal/services/services.go`)

| Operation | Docker API Calls | DB State Transition |
|---|---|---|
| **Create** | None (DB only) | `→ created` |
| **Deploy** | `PullImage` → `StopContainer` (old) → `RemoveContainer` (old) → `RunContainer` → `writeTraefikRoute` | `created/stopped/crashed/failed → deploying → running` |
| **Stop** | `StopContainer` | `running → stopped` |
| **Start** | `StartContainer` | `stopped → running` |
| **Restart** | `StopContainer` → `RemoveContainer` → `RunContainer` (fresh env vars) | `running → running` (atomic rotate) |
| **Delete** | `StopContainer` → `RemoveContainer` → `deleteTraefikRoute` | `any → (row deleted)` |
| **Logs** | `LogsContainer` (stdout+stderr, optional follow) | (read-only) |
| **DeployImage** | `RunContainer` (for builds pipeline) | `deploying → running` |

### 1.4 Reconciler (`internal/reconciler/reconciler.go`)

- Runs every **30 seconds**.
- Lists all `ah.tenant`-labeled containers via `ListContainersByLabel`.
- Compares container set against DB `services` rows with `status='running'`.
- Marks services `crashed` if their container is gone or exited/dead.
- Circuit breaker: 5 crashes in 10 minutes opens the circuit; escalating backoff (30m / 1h / 4h).
- Detects split-brain (container exists but DB points elsewhere) — logs warning, defers to GC.

### 1.5 Garbage Collector (`internal/gc/gc.go`)

- Runs every **5 minutes** (2-minute startup delay).
- Finds orphaned containers (`ah.service` label but no matching DB row) older than 10 minutes.
- Finds orphaned volumes (`ah-db-*` prefix but no matching DB row).
- Cleans stale build directories and prunes dangling images.

---

## 2. Recommended VM Networking Model

**Recommendation: TAP-device-per-microVM with a host-side bridge per tenant, fronted by Traefik.**

### Architecture

```
Internet
   │
   ▼
┌──────────────────────────────────────────┐
│             Traefik (host)               │
│  Listens :443, terminates TLS            │
│  File-provider routing (unchanged)       │
└────┬────────────────┬────────────────────┘
     │                │
  br-tenant-A      br-tenant-B          ◄── Linux bridge per tenant
     │                │
  ┌──┴──┐         ┌──┴──┐
  │tap0 │         │tap0 │               ◄── TAP device per microVM
  │ FC  │         │ FC  │
  │ VM  │         │ VM  │
  └─────┘         └─────┘
```

### Key decisions

1. **One Linux bridge per tenant** (`ah-tenant-<id>`), replacing Docker's per-tenant internal bridge. `iptables` rules replicate ICC=false and internal-only semantics.
2. **One TAP device per microVM**, attached to the tenant bridge. Firecracker's built-in virtio-net device connects the guest to the TAP.
3. **Static IP assignment** from a private range (e.g., `10.42.<tenant-hash>.0/24`). The `ah` binary maintains a small IP allocator backed by SQLite so IPs survive restarts.
4. **Traefik connects to each tenant bridge** via a veth pair (same pattern as today's `NetworkConnect`), routing to the guest IP:port.
5. **No outbound internet** by default: the bridge has no default route. A future "internet-enabled" tier could add masquerade rules via an opt-in flag.
6. **No CNI or Kubernetes networking stack** — the host manages bridges and TAP devices directly. This preserves the single-binary philosophy.

### Why not other models?

| Alternative | Reason Rejected |
|---|---|
| macvtap | Requires the host NIC to support promiscuous mode; many cloud providers block this |
| Firecracker's `mmds` for metadata only | Does not provide network connectivity; only metadata injection |
| Host-only networking with port-forward | Port exhaustion risk with many microVMs; Traefik would need per-port config |
| virtio-vsock | Good for control-plane RPCs but cannot carry HTTP traffic routable by Traefik |

---

## 3. Recommended Image / Rootfs Boot Flow

**Recommendation: Convert OCI images to ext4 rootfs at deploy time using a purpose-built converter.**

### Flow

```
 ┌──────────────────┐
 │  OCI Image       │  (from Nixpacks build or Docker Hub pull)
 │  (existing)      │
 └────────┬─────────┘
          │ deploy
          ▼
 ┌──────────────────┐
 │  oci2rootfs       │  Flattens layers → ext4 filesystem image
 │  converter       │  Sets up /sbin/init shim if needed
 └────────┬─────────┘
          │
          ▼
 ┌──────────────────┐
 │  rootfs.ext4     │  Stored at /var/lib/ah/rootfs/<svc-id>/rootfs.ext4
 │  + vmlinux       │  Shared kernel binary at /var/lib/ah/vmlinux
 └────────┬─────────┘
          │
          ▼
 ┌──────────────────┐
 │  Firecracker API │  PUT /boot-source (kernel)
 │                  │  PUT /drives (rootfs)
 │                  │  PUT /network-interfaces (TAP)
 │                  │  PUT /machine-config (vcpu, mem)
 │                  │  PUT /actions (InstanceStart)
 └──────────────────┘
```

### Details

1. **Kernel**: Ship a single stripped `vmlinux` binary (5.10 LTS or newer). Stored at a well-known path. Updated via platform releases, not per-tenant.
2. **OCI-to-ext4 converter** (`internal/rootfs/converter.go`): Uses `umoci` or `containers/image` to unpack OCI layers into a directory, then `mkfs.ext4` + `debugfs`/loop-mount to create a minimal ext4 image. The converter injects a tiny init shim (`/sbin/ah-init`) that execs the container's entrypoint.
3. **Caching**: The rootfs image is cached by image digest. Redeploying the same image skips conversion.
4. **Read-only rootfs**: The ext4 image is mounted read-only by Firecracker. A writable tmpfs overlay inside the guest provides `/tmp`, `/var/run`, etc. — same semantics as today's `ReadonlyRootfs: true` + `Tmpfs` mounts.
5. **Env vars**: Injected via Firecracker's boot args or a metadata endpoint inside the guest (MMDS). The init shim reads them and exports before exec.

---

## 4. Phased Rollout Plan

### Phase 0: Foundation (this issue + 1 follow-up)

- Produce this design document.
- Create `internal/firecracker/` package with a `Client` interface mirroring `docker.Client` method signatures.
- Implement a no-op/stub `Client` behind a build tag or feature flag (`AH_RUNTIME=firecracker`).
- Smallest follow-up ticket: **"Implement Firecracker stub client and rootfs converter prototype"** (see Section 9).

### Phase 1: Experimental Runtime (1-2 months)

- Implement `oci2rootfs` converter.
- Implement Firecracker VM lifecycle: create TAP, start microVM, stop microVM, destroy resources.
- Wire up to a new runtime backend selected by `AH_RUNTIME` env var.
- Reconciler and GC detect Firecracker VMs (via PID files or a local state table) alongside Docker containers.
- **Gate**: Firecracker runtime is opt-in, off by default. Only used in dev/test.

### Phase 2: Optional Runtime (2-4 months)

- Per-tenant `runtime` column in `tenant_quotas` table: `"docker"` (default) or `"firecracker"`.
- Traefik routing works for both runtimes (same file-provider YAML, different upstream addresses).
- Database containers remain Docker-only (Firecracker VMs are for stateless app workloads).
- Circuit breaker, crash detection, and GC work identically for both runtimes.
- Logging via virtio-console or serial → captured by `ah` host process, exposed via the same `/logs` API.
- **Gate**: Firecracker runtime promoted to "beta" when it passes the full integration test suite.

### Phase 3: Broader Use (4-6 months)

- Firecracker becomes the default for new tenants (existing tenants keep Docker unless migrated).
- Snapshot/restore support: Firecracker's built-in snapshot/resume for fast cold starts.
- Investigate replacing Traefik with a custom proxy that speaks vsock for lower overhead (optional, separate issue).
- gVisor/Docker runtime remains supported as a fallback for compatibility.

---

## 5. Service Lifecycle Mapping: Docker → Firecracker

| Action | Current (Docker/gVisor) | Firecracker Equivalent |
|---|---|---|
| **Create** | DB insert only | DB insert only (unchanged) |
| **Deploy — pull image** | `docker pull <image>` | `docker pull <image>` (reuse existing pull logic) |
| **Deploy — prepare rootfs** | (not needed; Docker uses image layers) | `oci2rootfs` converts OCI image → ext4; cache by digest |
| **Deploy — create network** | `EnsureNetwork` (Docker bridge) | `EnsureBridge` (Linux bridge) + `CreateTAP` (TAP device) |
| **Deploy — start workload** | `ContainerCreate` + `ContainerStart` (gVisor runtime) | Firecracker API: configure kernel, rootfs drive, network interface, machine config → `InstanceStart` |
| **Deploy — route traffic** | `writeTraefikRoute` (container name as upstream) | `writeTraefikRoute` (guest IP as upstream) |
| **Deploy — connect Traefik** | `NetworkConnect` (Traefik → tenant Docker network) | `ConnectTraefikToBridge` (veth pair: Traefik netns → tenant bridge) |
| **Stop** | `ContainerStop` (SIGTERM + 10s timeout) | Firecracker `PUT /actions {"action_type": "SendCtrlAltDel"}` or kill the Firecracker process (SIGTERM) |
| **Start** | `ContainerStart` | Start a new Firecracker process with the same rootfs (Firecracker VMs are not restartable; a fresh boot is required unless using snapshot/restore) |
| **Restart** | `Stop` + `Remove` + `RunContainer` (fresh env) | `Stop` (kill FC process) + `Start` new FC process with updated env vars in boot args |
| **Delete** | `StopContainer` + `RemoveContainer` + `deleteTraefikRoute` | Kill FC process + remove TAP device + remove rootfs file + `deleteTraefikRoute` |
| **Logs** | `ContainerLogs` (Docker log driver) | Read from virtio-serial/console device or a host-side log file that captures guest stdout/stderr |
| **Inspect** | `ContainerInspect` → status, health, exit code | Check if Firecracker process is alive (PID file); parse exit code from wait status; no built-in health check (must poll the guest's HTTP port) |
| **List by label** | `ListContainersByLabel` (Docker label filter) | Query local state table (SQLite) keyed by `ah.tenant` / `ah.service` |
| **Reconciler — detect crash** | Inspect container status (`exited`/`dead`) | Check PID liveness; if FC process exited, mark service `crashed` |
| **Reconciler — detect missing** | Container not found in Docker | PID file missing or process not running |
| **GC — orphaned workloads** | List `ah.service`-labeled containers not in DB | List running FC processes (via PID dir) not in DB; remove TAP, rootfs, kill process |
| **GC — orphaned volumes** | List `ah-db-*` Docker volumes not in DB | (N/A for Firecracker app workloads; database volumes remain Docker-managed) |
| **GC — dangling images** | `docker image prune` | Same (OCI images are still pulled via Docker); additionally prune orphaned rootfs ext4 files |
| **Health check** | Docker HEALTHCHECK from image metadata | Periodic HTTP probe from host to guest IP:port (replicate same probe logic) |
| **Resource limits** | `Memory`, `NanoCPUs`, `PidsLimit` on container | `machine-config`: `vcpu_count`, `mem_size_mib` in Firecracker API; PID limits via guest kernel sysctl |
| **Read-only rootfs** | `ReadonlyRootfs: true` | Mount ext4 as read-only in Firecracker drive config; tmpfs overlay inside guest for writable paths |
| **Security caps** | `CapDrop: ALL`, `no-new-privileges` | Not applicable at the same layer; Firecracker's KVM-based isolation replaces userspace capability restrictions. Guest runs as a full VM — the isolation boundary is the VMM, not Linux capabilities. Jailer provides host-side sandboxing of the FC process. |

---

## 6. Risk List

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| 1 | **OCI-to-rootfs conversion adds deploy latency** | Medium | Cache rootfs by image digest; parallelize conversion; use ramdisk for build |
| 2 | **Firecracker VMs are not restartable** (unlike Docker containers) | Medium | Treat every "start" as a fresh boot; use snapshot/restore in Phase 3 for fast restarts |
| 3 | **Guest networking requires host-level iptables management** | High | Centralize all iptables rules in a single module; use `nftables` with atomic rule replacement; test with network namespaces in CI |
| 4 | **Kernel compatibility** — some OCI images assume kernel features not in our vmlinux | Medium | Ship a broadly-compatible kernel config (similar to what AWS Lambda uses); document unsupported syscalls |
| 5 | **Two runtime backends double the test surface** | Medium | Abstract behind a `Runtime` interface; share integration tests parameterized by runtime |
| 6 | **Log capture from guest is less mature than Docker log drivers** | Low | Use virtio-serial with a well-tested host-side reader; fall back to serial console |
| 7 | **MMDS or boot-arg env injection has size limits** | Low | MMDS supports up to 50KB; boot args are limited by kernel command line length (~4KB). For large env, use a plan9 shared directory or inject a file into the rootfs. Current max env is 32KB per var — may need a different injection mechanism. |
| 8 | **Firecracker jailer requires `root` or specific capabilities** | Low | Already running as root in production (Docker also requires root); jailer actually reduces privilege by chrooting the FC process |
| 9 | **IP address exhaustion for large tenant deployments** | Low | /24 per tenant gives 254 VMs; can expand to /16 if needed. Track allocations in SQLite. |

---

## 7. Explicit Non-Goals

The following are explicitly **out of scope** for this integration plan:

1. **Replacing Docker for database containers** — Databases (Postgres, MySQL, etc.) use Docker volumes and will remain on Docker. Firecracker is for stateless app workloads only.
2. **Multi-host / clustering** — This plan assumes a single host. Distributing Firecracker VMs across multiple nodes is a separate concern.
3. **GPU passthrough** — Firecracker does not support GPU passthrough. GPU workloads are out of scope.
4. **Windows guests** — Firecracker only supports Linux. Windows containers remain unsupported.
5. **Live migration of running VMs** — Firecracker snapshot/restore is for fast cold starts, not live migration.
6. **Custom kernel per tenant** — All tenants share the platform kernel. Custom kernels introduce too much surface area.
7. **Nested virtualization** — Running Docker or other VMs inside a Firecracker guest is not supported.
8. **Removing Docker entirely** — Docker remains for image pulling, building (Nixpacks), and database containers. The goal is to add Firecracker as an alternative runtime for app workloads, not to eliminate Docker.
9. **IPv6 support** — The TAP networking model uses IPv4 private ranges. IPv6 can be added later.
10. **Outbound internet for Firecracker VMs** — Same restriction as current Docker containers. Adding egress is a separate policy decision.

---

## 8. Open Questions That Need a Separate Issue

1. **Init system inside the guest** — Should `ah-init` be a static Go binary or a shell script? Static binary is more portable but adds build complexity. Need a spike.
2. **Health check mechanism** — Docker images can declare `HEALTHCHECK`. Firecracker has no equivalent. Should the host poll the guest's HTTP port, or should we inject a health-check sidecar into the rootfs? Needs design.
3. **Log rotation and storage** — Docker has configurable log drivers (json-file, journald). What is the Firecracker equivalent? Host-side log files need rotation. Needs design.
4. **Snapshot/restore semantics** — When should a snapshot be taken? On clean shutdown? On a schedule? How does snapshot interact with env var updates (which require a fresh rootfs)? Needs design.
5. **Firecracker version and update cadence** — Which Firecracker version to pin? How to handle security updates to the Firecracker binary? Needs ops plan.
6. **Jailer configuration** — What cgroup and seccomp profiles should the jailer use? Need to map current Docker security posture to jailer equivalents.
7. **MMDS vs boot-args vs shared-directory for env injection** — Current max env value is 32KB. Boot args are limited to ~4KB. MMDS is 50KB. Need to benchmark and decide.
8. **Bridge cleanup on tenant deletion** — When a tenant is deleted, the Linux bridge and iptables rules need cleanup. Should this be in GC or synchronous in the delete handler? Needs design.
9. **Metrics and observability** — Firecracker exposes metrics via a Unix socket. Should we scrape these and expose via the existing `/metrics` endpoint? Needs design.
10. **CI testing** — Firecracker requires `/dev/kvm`. CI runners (GitHub Actions) may not have KVM. Need a testing strategy (mock FC client for unit tests, dedicated KVM-enabled runner for integration tests).

---

## 9. Smallest Follow-Up Implementation Ticket

**Title**: `feat(runtime): implement Firecracker stub client and rootfs converter prototype`

**Scope**:

1. Create `internal/runtime/runtime.go` — define a `Runtime` interface that abstracts both Docker and Firecracker:
   ```go
   type Runtime interface {
       StartWorkload(ctx context.Context, cfg WorkloadConfig) (string, error)
       StopWorkload(ctx context.Context, id string) error
       RemoveWorkload(ctx context.Context, id string) error
       InspectWorkload(ctx context.Context, id string) (*WorkloadInfo, error)
       WorkloadLogs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error)
       ListWorkloads(ctx context.Context, labelKey, labelValue string) ([]string, error)
   }
   ```
2. Create `internal/runtime/docker.go` — adapter that wraps the existing `docker.Client` to satisfy the `Runtime` interface.
3. Create `internal/runtime/firecracker.go` — stub implementation that returns `ErrNotImplemented` for all methods. Gated behind `AH_RUNTIME=firecracker`.
4. Create `internal/rootfs/converter.go` — prototype that takes a local OCI image ref and produces an ext4 file. Does not need to be production-hardened.
5. Add tests: unit test for the Docker adapter (using existing `testutil` mocks), unit test for the stub Firecracker client.

**Acceptance criteria**:
- `go build ./...` passes with both `AH_RUNTIME=docker` (default) and `AH_RUNTIME=firecracker`.
- `go test ./...` passes.
- Existing behavior is unchanged when `AH_RUNTIME` is unset or `docker`.
- The `Runtime` interface is documented with godoc comments explaining the abstraction boundary.

**Estimated effort**: 2-3 days.
