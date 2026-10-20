# Per-database networks (#164)

Services reach managed Postgres and Redis on a dedicated internal Docker network, not via `127.0.0.1`.

## Topology

- Network: `ah-dbnet-<tenant>-<db>` — `Internal=true`, `ICC=true`, name ≤63 characters (tenant ID is shortened first).
- DNS alias on that network only: `ah-db-<dbID>` (≤63 characters).
- Connection string: `postgres://ah:<pass>@ah-db-<dbID>:5432/ah?sslmode=disable` or `redis://:<pass>@ah-db-<dbID>:6379/0`.
- Host publish stays `127.0.0.1:<allocatedPort>` for `ah` health probes. Never `0.0.0.0`.
- Kanban (`RunDatabase` without `NetworkName`) stays loopback-only.

## Attach

On service deploy, image deploy, restart, and start, the platform attaches the container to each of **this tenant's** database networks whose alias appears in the service env. Another tenant cannot join even if it copies the URL.

Two services that share one database can reach each other on that ICC=true network.

## Migration

`NewManager` rewrites stored `127.0.0.1:<hostPort>` addresses in `databases.connection_string_encrypted` and matching `service_env` values. Running containers still have the old env until they are restarted.

## GC

Orphan `ah-dbnet-*` networks older than 15 minutes are removed unless a non-failed database row owns them.

## Live check still needed

This change is proven in the mock-Docker harness. Confirm on the production Docker + gVisor host that a service container can resolve `ah-db-<id>` and open TCP to 5432/6379, that host probes still work on loopback, and that a second tenant's container cannot join.
