# Issue #164 — Managed database connection strings use 127.0.0.1

**Issue**: [#164](https://github.com/dennisonbertram/agentic-hosting/issues/164)
**Date**: 2026-10-20
**Status**: Root cause confirmed in code and in the mock-Docker test harness. Not reproduced with live Docker/gVisor in this environment. **No production fix in this change** — the networking work is not a small patch.

---

## 1. Verdict

The reported hunch is **confirmed**. A service cannot reach a database created by the same tenant using the connection string the API returns.

There are **two independent blockers**, not one:

1. **Wrong address in the URL.** Create stores and returns `127.0.0.1` as the host. Inside a service container that address is the container itself, not the host and not the database.
2. **No shared data-plane network, and ICC would still block one.** Database containers are not attached to `ah-tenant-<tenantID>`. That tenant bridge is `Internal=true` and `ICC=false`, so even attaching the database there would not let a service open a TCP connection to it. Service-to-database traffic needs the same pattern already used for Traefik: a dedicated internal network with ICC enabled, plus a DNS name in the connection string.

Changing only the hostname, or only the Docker `HostIP`, is not enough.

---

## 2. Path: create → connection string → inject into a service

### Create and encrypt

`Manager.Create` allocates a host port, builds the URL, encrypts it, and inserts the row:

```208:229:internal/databases/databases.go
		switch req.Type {
		case "postgres":
			dbName = "ah"
			username = "ah"
			connStr = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", username, password, port, dbName)
		case "redis":
			connStr = fmt.Sprintf("redis://:%s@127.0.0.1:%d/0", password, port)
		}
		// ...
			 SELECT ?, ?, ?, ?, 'provisioning', '127.0.0.1', ?, ?, ?, ?, ?, ?, ?, ?
```

The create response repeats that host (`Host: "127.0.0.1"` at line 323). `GET /v1/databases/{id}/connection-string` decrypts the stored blob; it does not rewrite the host (`GetConnectionString`, lines 400–417). `POST /v1/services/{id}/env` encrypts whatever the client sent. Deploy/restart decrypts `service_env` and passes the map unchanged into `RunContainer`.

The platform never injects `DATABASE_URL` / `REDIS_URL` itself. Skills, `ahc db`, and `/ah-db` fetch the API string and set env, then restart. That restart recreates the service with the loopback URL.

### Database container network

`runPostgres` / `runRedis` call `docker.RunDatabase`. `RunDatabaseConfig` has no network field. `databaseHostConfig` publishes the engine port on host loopback only and does not set `NetworkMode`:

```799:807:internal/docker/client.go
func databaseHostConfig(cfg RunDatabaseConfig, portStr string) *container.HostConfig {
	return &container.HostConfig{
		Runtime: "runsc",
		PortBindings: nat.PortMap{
			nat.Port(portStr): []nat.PortBinding{
				{HostIP: "127.0.0.1", HostPort: fmt.Sprintf("%d", cfg.HostPort)},
			},
		},
```

`ContainerCreate` is called with a nil networking config (line 882), so the container joins Docker's **default bridge**. Health checks from the `ah` process (`waitForPostgres` / `waitForRedis`) dial `127.0.0.1:<hostPort>`. That is why a host-side `SELECT 1` smoke test can pass and still miss this bug.

Create never calls `EnsureNetwork`, `ConnectNetwork`, or `ConnectNetworkAlias`. `internal/databases` has no references to those APIs.

### Service container network (gVisor + isolation)

`RunContainer` places the service on the tenant bridge (`NetworkMode: ah-tenant-<tenantID>`) and then attaches a **per-service** ingress bridge so Traefik can reach it:

```307:357:internal/docker/client.go
//   - Container is placed on a per-tenant internal Docker network (Internal=true, ICC=false).
//   - A separate internal network connects only this service and Traefik for ingress.
		NetworkMode:    container.NetworkMode(tenantNet),
```

`bridgeNetworkCreateOptions(false)` sets `Internal: true` and `enable_icc=false`. `Internal=true` means no default gateway to the host, so the service cannot reach host-published ports (including `127.0.0.1` on the host, which it would not see anyway). `ICC=false` drops container-to-container packets on that bridge.

gVisor (`Runtime: "runsc"`) is used for both services and databases. It supplies a userspace network stack inside the sandbox; it does not make host loopback appear inside the container. Port publish still works via docker-proxy on the host, which is what the control-plane probes use.

Ingress networks (`ah-ingress-<tenant>-service-<id>`) are ICC=true but contain only `paas-traefik` and that one service. Databases are not on them.

### Default-bridge side effect (extra isolation note)

Database containers share Docker's default bridge (ICC on, not internal). Host bind `127.0.0.1` stops *published-port* access from other host interfaces. It does **not** stop another container on `docker0` from connecting to `containerIP:5432` / `:6379` if it can learn that address. Passwords are the real cross-DB isolation today. Tenant *services* are not on `docker0`, so they still cannot connect — the user-facing bug — but databases are weaker than the tenant-network story in `docs/ux-paths/topics/database-provisioning.md` (which claims both service and database sit on `ah-tenant-X`).

---

## 3. Reproduction

Live Docker + gVisor was not available in this investigation environment. The existing manager tests already stand up a fake engine on `127.0.0.1:<allocatedPort>` so `waitForPostgres` succeeds — the same host path as the production smoke test.

Focused characterization tests (pass on current code; a real fix must replace them):

| Test | What it locks in |
|------|------------------|
| `TestIssue164_DatabasePublishedOnHostLoopbackOnly` (`internal/docker/database_config_test.go`) | `HostIP=127.0.0.1`, empty `NetworkMode` |
| `TestIssue164_CreateConnectionStringUsesHostLoopbackAndSkipsTenantNetwork` (`internal/databases/databases_test.go`) | Postgres and Redis URLs contain `@127.0.0.1:`; row `host` is `127.0.0.1`; Create does not call `EnsureNetwork` / `ConnectNetwork` |
| `TestBridgeNetworkCreateOptionsKeepTenantIsolationAndAllowIngressPair` (existing) | Tenant/pool nets stay `Internal` + ICC=false; ingress pair nets allow ICC |

Together: the URL the API returns cannot be used from a service namespace, and there is no network path even if the hostname were fixed in isolation.

---

## 4. Proposed fix (do not implement here)

### Recommended: per-database internal network, same idea as ingress

Mirror `EnsureIngressNetwork` / `ConnectNetworkAlias`:

1. Name: `ah-dbnet-<tenantID>-<dbID>` (`Internal=true`, `ICC=true`).
2. DNS alias on that network only, e.g. `ah-db-<dbID>` (keep ≤63 characters, same constraint as `ServiceIngressAlias`).
3. Attach the database container to that network (primary `NetworkMode` or `ConnectNetworkAlias`). Listen on the engine port inside the container (`5432` / `6379`), not the host-mapped port.
4. Connection string: `postgres://ah:<pass>@ah-db-<dbID>:5432/ah?sslmode=disable` (and `redis://:<pass>@ah-db-<dbID>:6379/0`).
5. On service (and environment) create/start/restart, attach that tenant’s running workloads to each of its database networks — or, stricter, only workloads that have `DATABASE_URL` / `REDIS_URL` for that database.
6. **Keep** `HostIP=127.0.0.1` publish for control-plane health probes from `ah` on the host. Do not bind `0.0.0.0`.
7. GC must learn `ah-dbnet-*` the way it already knows `ah-tenant-*` / `ah-ingress-*`.
8. Existing rows: decrypt `connection_string_encrypted`, rewrite host (and port if switching to the container port), re-encrypt. Stored `service_env` copies are independent; document that tenants must re-GET and re-SET env, or rewrite matching values.

**Isolation**

- Other tenants never join `ah-dbnet-<other>-*`. Cross-tenant isolation stays intact.
- `Internal=true` keeps the engine off the internet and off the host’s public NICs.
- **Tradeoff:** every workload attached to the same DB network can talk to every other endpoint on that network (ICC=true). Two services that both use the same database can reach each other through it. That is weaker than today’s ICC=false tenant bridge, and it is the same class of tradeoff as the Traefik ingress pair (proxy + one workload).
- Attaching *all* tenant services to *all* tenant DB networks makes every pair of those services mutually reachable on each shared DB net. Prefer attach-on-env-link, or accept same-tenant lateral movement and say so in security docs.

### Rejected or weaker options

| Option | Why not (or when) |
|--------|-------------------|
| Enable ICC on `ah-tenant-*` and put DBs there | Simplest code. Breaks the documented “no container-to-container communication” property for *all* tenant workloads, including environments. Traefik is no longer on that bridge, so this is not required for HTTP. |
| One shared `ah-data-<tenantID>` ICC=true net | Fewer networks than per-DB. All tenant services that join it can talk to each other. Hits the same lateral-movement issue as “attach everyone to every DB net”. |
| `host.docker.internal` / host-gateway + bind `0.0.0.0` | Tenant nets are `Internal=true` (no route to the host). Binding all interfaces exposes engines on public NICs unless the host firewall is perfect. |
| Per-(service, database) pair nets | Strongest isolation (like ingress). Network count is `services × databases`; health already degrades above 200 tenant nets. |
| Hostname-only change, keep current publish | Service still has no route to host loopback. |

### Impact on existing databases

- Encrypted URLs and the `host` column are written at create time. A rolling rewrite on reconcile/start is required or old URLs keep failing.
- `service_env` may already hold the loopback URL. Restarting after a server-side rewrite of `databases` is not enough unless env is updated.
- Host port uniqueness (`state_006`, ranges 5432–6000 / 6379–7000) can stay for health probes even if clients use 5432/6379 on the overlay.

### Impact on docs / skills

These currently teach “fetch connection string, set `DATABASE_URL`/`REDIS_URL`, restart” and show `127.0.0.1` as a working host:

- `skills/agentic-hosting/SKILL.md` §4 and `.claude/skills/agentic-hosting/SKILL.md`
- `.claude/commands/ah-db.md`
- `docs-site/src/content/docs/databases.md` (“Host bind is loopback-oriented… tenants reach DBs via the connection string”)
- `llms-full.txt` / generated docs copies
- Tutorials 08, 09, 23 (already `publish: false` / on hold for this issue)
- `docs/ux-paths/topics/database-provisioning.md` (incorrectly claims DBs are on the tenant net)

After a fix: show the DNS alias and container port; state that the URL is for **containers on that tenant’s DB network**, not for `curl` from the host; keep host-loopback health as an operator detail. Do not tell agents to print the full URL.

Kanban (`RunDatabase` for Vikunja) uses the same publish helper. It is reached via HTTP/Traefik, not via these connection strings. Leave it on loopback unless a later change needs container-to-kanban TCP.

---

## 5. Why this is not a small PR

A correct patch touches `internal/docker` (config, create, GC prefixes, alias helpers), `internal/databases` (URL, attach, restart, existing-row rewrite), `internal/services` and likely `internal/environments` (join DB nets on start), tests, and the docs list above. Shipping a hostname change without the network would still leave services unable to connect.
