---
title: Postgres & Redis
description: Provision managed Postgres 15 or Redis 7 containers, fetch the decrypted connection string, and delete volumes.
section: Use
order: 7
---

# Managed Postgres and Redis

`POST /v1/databases` provisions one engine per type: **Postgres** or **Redis**. There is no MySQL, no custom image, no HA.

```json
{"name":"mydb","type":"postgres"}
```

`type` must be `postgres` or `redis`. Names are DNS-safe (alnum + hyphen, start/end alnum). Default quota: **3** databases per tenant (non-`failed` rows count).

Creation is a long route (no 30s chi timeout). Use an `Idempotency-Key`. Disk check 80/90 runs first.

## Endpoints

| Method | Path | Result |
|--------|------|--------|
| POST | `/v1/databases` | Provision; connection string included on create |
| GET | `/v1/databases` | Paginated list |
| GET | `/v1/databases/{dbID}` | Record without forcing a decrypt |
| GET | `/v1/databases/{dbID}/connection-string` | `{"connection_string":"..."}` — **AUDIT** logged |
| DELETE | `/v1/databases/{dbID}` | Container + volume wiped |

Passwords and connection strings are AES-256-GCM encrypted at rest with the **server master key**. There is no per-tenant key separation yet (see [Security model](./security/)).

## Wiring a service

The platform does **not** auto-inject `DATABASE_URL`. You set env and restart:

```bash
CONN=$(curl -s -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/databases/$DB_ID/connection-string" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["connection_string"])')

curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\":\"$CONN\"}"

curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $AH_KEY"
```

Restart **recreates** the container so new env vars take effect, and attaches that service to the database network when the env contains the `ah-db-<id>` alias.

The connection string is for **containers on that tenant's database network**, not for `curl` from the host. Host is `ah-db-<databaseID>`; port is the engine port (`5432` / `6379`). Control-plane health probes still use a host-loopback publish (`127.0.0.1`, never `0.0.0.0`).

`/ah-db` and `ahc db` do this pair of calls.

## Runtime facts

- Containers use gVisor (`runsc`)
- Each database gets `ah-dbnet-<tenant>-<db>` (`Internal=true`, `ICC=true`). Other tenants are never attached
- Connection string host is the DNS alias `ah-db-<databaseID>` on the container port
- Host bind stays loopback-only for `ah` health probes; do not treat that address as the service URL
- Two services that both use the same database can reach each other on that ICC=true network
- Ports in the `databases.port` column stay unique (migration `state_006`) for probes
- Readiness is protocol-level (Postgres/Redis), not “TCP accepted”
- Tenant suspend/delete stops database containers
- DELETE wipes volume data before removal and removes `ah-dbnet-*`
- GC looks for `ah-db-*` volumes and `ah-dbnet-*` networks not referenced in the DB (age > 10 / 15 minutes)
- On startup, stored `127.0.0.1` connection strings and matching `service_env` copies are rewritten; **running containers still need a restart** to pick up the new URL and join the network

Stale `provisioning` rows are marked `failed` and cleaned on manager startup (`ReconcileStale`).

## CLI

```bash
ahc db create --name mydb --type postgres
ahc db list
ahc db connection-string <id>
ahc db delete <id>
```
