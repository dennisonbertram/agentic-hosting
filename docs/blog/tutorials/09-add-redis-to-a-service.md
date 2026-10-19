---
title: "Add Redis as a cache or job queue"
description: "Provision a managed, password-protected Redis with one command, wire REDIS_URL into your service, and learn what the platform does and does not guarantee."
slug: add-redis-to-a-service
tags: [redis, databases, env-vars, queues, tutorial]
cover_diagram: diagrams/09-add-redis-to-a-service.png
sources:
  - internal/databases/databases.go
  - internal/api/databases.go
  - internal/api/services.go
  - internal/api/server.go
  - internal/docker/client.go
  - internal/services/services.go
  - cmd/ahc/db.go
  - cmd/ahc/service_env.go
  - docs-site/src/content/docs/databases.md
  - docs-site/src/content/docs/env.md
  - docs/ops/fresh-install.md
  - CHANGELOG.md
publish: false
hold_reason: "Services cannot reach managed databases yet: https://github.com/dennisonbertram/agentic-hosting/issues/164"
---

# Add Redis as a cache or job queue

A cache, a rate limiter, and a job queue all want Redis, and I think an agent should be able to add one to a service with a single command. That is what this post shows. You have a web service running on agentic-hosting. By the end you will have a Redis container with a password and its connection string stored encrypted as `REDIS_URL` on your service.

Limitation: this does not work yet. The connection string uses `127.0.0.1`, and the database port is bound to the host's loopback interface only, so a service container cannot reach its database. This is tracked in [issue #164](https://github.com/dennisonbertram/agentic-hosting/issues/164). The steps below create Redis and set the variable correctly, but the SET and GET route in Step 3 will fail with a connection error until that issue is fixed.

The "How it works" section explains why. I did not run the commands in this post. I checked them against the source.

## Prerequisites

- A running service. This post calls it `my-app`.
- An API key from registering a tenant. The examples use `$AH_API_KEY`.
- The `ahc` CLI, or `curl`.
- The API host in `$AH_URL`, with no `/v1` suffix. The public one is `https://api.agentic.hosting`. If you self-host, use your own host. The examples add `/v1/...` themselves.

## Step 1: Provision with one command

```bash
ahc db provision my-app redis --name cache
```

This does five things in order. It creates the database, polls until its status is `ready`, fetches the connection string, sets `REDIS_URL` on `my-app`, and restarts `my-app`. The `--name` flag sets the database name. If you leave it off, the name defaults to `my-app-redis`.

Expected output shape:

```text
Provisioning redis database "cache"...
Database "cache" created (id: <32-char hex id>)
Waiting for database to become available.... done.
Setting REDIS_URL on service "my-app"...
Restarting service "my-app"...
Done. cache is ready. Connection host: redis://127.0.0.1:<port>
```

Now look at the env on the service:

```bash
ahc service env get my-app
```

```text
KEY        VALUE
REDIS_URL  ***
```

The value is masked. The API itself returns `********` for masked values, and the CLI prints `***`.

Provisioning can take a while on a cold host, because the `redis:7-alpine` image may need to be pulled. The create route allows up to 6 minutes for this.

## Step 2: The same thing with the raw API

The CLI is a thin wrapper. Here are the main calls it makes. The CLI also polls the database until it is ready.

Create the database. Send an `Idempotency-Key` so a retry does not create a second one:

```bash
curl -s -X POST "$AH_URL/v1/databases" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Idempotency-Key: cache-001" \
  -H "Content-Type: application/json" \
  -d '{"name":"cache","type":"redis"}'
```

The response is HTTP 201 with the database record, including `id`, `status: "ready"`, `host`, `port` and a `connection_string`. Fetch the string again later from `GET /v1/databases/{dbID}/connection-string`. That call is written to the server log as an audit event.

Look up the service ID and the connection string first. The list response is JSON; copy the `id` of `my-app` and the `connection_string` of the Redis database (use `python3` or `jq` to extract them; do not echo the string, it contains the password):

```bash
curl -s -H "Authorization: Bearer $AH_API_KEY" "$AH_URL/v1/services"
export SERVICE_ID="<id-of-my-app>"
export DB_ID="<id-from-the-create-response>"
CONN=$(curl -s -H "Authorization: Bearer $AH_API_KEY" \
  "$AH_URL/v1/databases/$DB_ID/connection-string" | python3 -c 'import sys,json; print(json.load(sys.stdin)["connection_string"])')
```

Set the env var. This merges into the existing env and does not replace it:

```bash
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"REDIS_URL\":\"$CONN\"}"
```

Then restart, which recreates the container so the new env is injected:

```bash
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $AH_API_KEY"
```

The platform does not inject `REDIS_URL` for you. Setting it and restarting is your job, or the CLI's.

## Step 3: A route that does SET and GET

Here is a small Node route. It reads `REDIS_URL` from the environment, writes a key, and reads it back.

```js
import http from "node:http";
import { createClient } from "redis";

const client = createClient({ url: process.env.REDIS_URL });
client.on("error", (e) => console.error("redis error:", e.message));
await client.connect();

http.createServer(async (req, res) => {
  if (req.url === "/redis-check") {
    await client.set("hello", "world", { EX: 60 });
    const value = await client.get("hello");
    res.end(JSON.stringify({ roundTrip: value }));
    return;
  }
  res.end("ok");
}).listen(process.env.PORT || 3000);
```

Deploy it the same way you deployed `my-app`. The [deploy guide](https://agentic.hosting/docs/deploy/) covers that. Then call the route:

```bash
curl https://<your-service-host>/redis-check
```

Once the service can reach Redis you should see `{"roundTrip":"world"}`. Until issue #164 is fixed, the log shows a connection error instead. Read it with `GET /v1/services/{id}/logs`. I have not run this route.

## Step 4: How readiness is checked

When you create a database, `ah` does not just wait for a port to open. A TCP accept only proves something is listening. For Redis, `ah` opens a connection, sends `AUTH <password>`, expects `+OK`, then sends `PING` and expects `+PONG`. It retries once a second for up to 30 seconds. If that never succeeds, the database is marked `failed`, and the container and volume are removed.

So a database that reports `ready` has already answered an authenticated request. The fresh-install notes in the repo record an authenticated `PING`/`PONG` against a Redis provisioned this way. The changelog also records that an earlier readiness check failed on Redis because it did not authenticate, and that this was fixed.

## Step 5: Reveal the value once, then hide it again

Masked is the default. To see the real value, ask for it:

```bash
ahc service env get my-app --reveal
# or
curl -s "$AH_URL/v1/services/$SERVICE_ID/env?reveal=true" \
  -H "Authorization: Bearer $AH_API_KEY"
```

The server logs an `AUDIT: action=env.revealed` line with your tenant, the service and the API key ID. Do this when you need to debug, and not in a script that runs on every deploy. To go back to masked, run the same command without `--reveal`. Revealing does not change the stored value.

## How it works

The diagram shows the pieces.

![Architecture diagram: ah probes a Redis container with AUTH and PING; REDIS_URL is stored encrypted in service_env and injected into the app container when it is created; the Redis data lives on an ah-db volume that survives restart and gets a best-effort wipe on delete.](diagrams/09-add-redis-to-a-service.png)

- `ah` creates a volume named `ah-db-<id>`, starts `redis:7-alpine` under gVisor (`runsc`) with `--requirepass`, and probes it with AUTH and PING.
- The password is random, and the connection string is encrypted with the server master key before it is stored.
- `REDIS_URL` goes into the service's encrypted env. It is injected when the container is created, which is why a restart is needed.

Here is why the connection fails. The Redis container is published on `127.0.0.1:<port>` on the host, and that is the address inside the connection string. In the code, service containers are attached to the tenant network `ah-tenant-<tenant id>`. I found no code that attaches the Redis container to that network, and none that rewrites the host in the connection string. Inside a container, `127.0.0.1` normally means the container itself, so a connection from your service fails with "connection refused" ([issue #164](https://github.com/dennisonbertram/agentic-hosting/issues/164)). The `/redis-check` route shows that error in your logs.

If you only need to confirm that the Redis itself works, you can do it on the host where `ah` runs. Reveal the connection string through the audited endpoint and use `redis-cli -u "$CONN" PING`. You should get `PONG`.

## Guardrails

- **One engine per type.** `type` must be `redis` or `postgres`. There is no custom image and no high availability. Redis runs `redis:7-alpine`.
- **Tenant quota.** The default is 3 databases per tenant, counting every database that is not `failed`. Going over returns a quota error.
- **Limits.** Database containers get 512 MB of memory, no swap, and a 256-process limit.
- **Names.** 1 to 63 characters, starting with a letter, with letters, digits, hyphens and underscores, and no trailing hyphen.
- **Delete attempts a wipe.** `DELETE /v1/databases/{dbID}` stops the container, tries to overwrite the volume's files with zeros, and removes the volume. The wipe is best effort: if it fails, the error is logged and deletion continues, so a 204 does not prove the data was overwritten. With the CLI you must pass `--confirm`.
- **A restart keeps data.** Restarting your service does not touch the Redis volume. Deleting the database does.

Before you delete, change or remove `REDIS_URL` on any service that uses it. Deleting does not do that for you.

## What to try next

- Read the [Postgres and Redis reference](https://agentic.hosting/docs/databases/) for the full endpoint list.
- Read [Env vars and secrets](https://agentic.hosting/docs/env/) for key limits and validation rules.
- Check [Troubleshooting](https://agentic.hosting/docs/troubleshooting/) if provisioning fails.
- Look at the [security model](https://agentic.hosting/docs/security/) for how secrets are encrypted.
- The source is on [GitHub](https://github.com/dennisonbertram/agentic-hosting). The reachability problem is tracked in [issue #164](https://github.com/dennisonbertram/agentic-hosting/issues/164).
