---
description: Provision a Postgres or Redis database and wire it to a service. Usage: /ah-db <service-name> <postgres|redis> [db-name]
argument-hint: <service-name> <postgres|redis> [db-name]
allowed-tools: Bash
---

Provision a `$2` database named `${3:-$1-db}` and wire it to service `$1` on agentic-hosting.

You are an agentic-hosting operator using `$AH_URL` and `$AH_KEY`.

Steps:
1. Find the service by name: `GET /v1/services` — filter by name == `$1`, get its ID. If not found, tell the user.
2. Create the database with an idempotency key (database creation can take up to 30s — be patient):
   ```
   POST /v1/databases {"name": "${3:-$1-db}", "type": "$2"}
   Idempotency-Key: <uuid>
   ```
3. Poll for database `ready` status every 5 seconds, up to 60 seconds
4. Get the connection string: `GET /v1/databases/{db-id}/connection-string`
   The host is `ah-db-<databaseID>` on port `5432` (postgres) or `6379` (redis). That name resolves only inside containers on `ah-dbnet-<tenant>-<db>`. Do not test it with curl from the host.
5. Set the appropriate env var on the service:
   - postgres → `DATABASE_URL`
   - redis → `REDIS_URL`
6. Restart the service: `POST /v1/services/{service-id}/restart` (recreates the container and attaches it to the database network)
7. Poll for service `running` status

Report: database ID, connection string (masked — show only the host:port part, e.g. `ah-db-…:5432`), service restart status.

**Never print the full connection string** — it contains credentials. Only show host:port. If env still has a `127.0.0.1` URL, re-GET the string, set env again, and restart.
