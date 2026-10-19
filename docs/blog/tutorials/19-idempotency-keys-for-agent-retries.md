---
title: "Make agent retries safe with idempotency keys"
description: "Add an Idempotency-Key header so an agent that retries POST /v1/databases after a timeout does not create a second database when the first attempt succeeded."
slug: idempotency-keys-for-agent-retries
tags: [idempotency, agents, reliability, api, curl]
cover_diagram: diagrams/19-idempotency-keys-for-agent-retries.png
sources:
  - internal/middleware/idempotency.go
  - internal/api/server.go
  - internal/api/databases.go
  - internal/databases/databases.go
  - internal/middleware/cors.go
  - cmd/ah/server.go
  - docs-site/src/content/docs/api.md
  - docs-site/src/content/docs/databases.md
  - docs-site/src/content/docs/rate-limits.md
  - skills/agentic-hosting/SKILL.md
  - AGENT_RUNBOOK.md
  - website/dashboard/app.js
  - README.md
  - CHANGELOG.md
  - cmd/ahc/
---

Agents retry. I think that is the right behavior, and it is also how they end up creating things twice.

Your agent calls `POST /v1/databases`. Provisioning a database takes a while, because the server may have to pull an image first. Your HTTP client gives up after 30 seconds and the agent retries. The first request may still be running, or it may have been cancelled when the connection dropped, since database creation uses the request's context. Either way, a plain retry can leave you with two databases, or with one half-created.

By the end of this post you will have a create call you can retry as many times as you like, and you will know which routes the protection covers and which it does not.

![Two-lane sequence diagram. Attempt 1: the client sends POST /v1/databases with an Idempotency-Key to ah, the handler runs, and the 201 response is stored under the key. Attempt 2 after a timeout: ah finds the key in the cache and replays the same response without running the handler. A third lane shows the same key with a different body returning 409.](diagrams/19-idempotency-keys-for-agent-retries.png)

## Prerequisites

- A running agentic-hosting server and a tenant API key. If you do not have those yet, see the earlier posts on installing and registering a tenant.
- `curl` and `uuidgen`.

The hosted API base is `https://api.agentic.hosting/v1/`. If you self-host, use your own host.

```bash
export AH_URL=https://api.agentic.hosting
export AH_API_KEY=ah_xxxxxxxx   # your tenant key
```

I wrote these commands from the code. I did not run them for this post, so the output below shows the shape of each response, not captured output.

## Step 1: See the problem

Create a database with a short client timeout:

```bash
curl -sS --max-time 30 -X POST "$AH_URL/v1/databases" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"orders-db","type":"postgres"}'
```

The server gives this route a six-minute limit (`databaseCreateTimeout` in `internal/api/databases.go`). Your client's 30 seconds is much shorter. If curl exits with a timeout, you do not know whether the database exists. Running the same command again, without a key, is a second create request.

## Step 2: Add an Idempotency-Key and retry

Generate one key for this one intended action, and send it with the request:

```bash
KEY=$(uuidgen)

curl -sS -i --max-time 30 -X POST "$AH_URL/v1/databases" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d '{"name":"orders-db","type":"postgres"}'
```

If it times out, send the exact same command again, same `$KEY`, same body. The server does one of two things:

- If the first request already finished with a 2xx, you get the stored response back, with the header `Idempotency-Replayed: true`. The handler does not run again.
- If the first request is still running, your retry waits for it and then gets that response (see step 5).

Only 2xx responses are stored. If the first attempt fails or is cancelled, the key is released and your retry runs the handler again, which can happen after a provisioning row was already inserted. The key protects against duplicate successes, not against every partial failure, so check `GET /v1/databases` after an error.

A replayed create looks like this:

```text
HTTP/1.1 201 Created
Idempotency-Replayed: true
Content-Type: application/json

{"id":"...","name":"orders-db","type":"postgres","status":"...","connection_string":"...", ...}
```

The create response includes the `connection_string`, and the replay includes it too, so a retry gives your agent the same credentials the first attempt would have.

## Step 3: Change the body, get a 409

A key is a promise that the request is the same one. If you reuse it with a different body, the server refuses:

```bash
curl -sS -i -X POST "$AH_URL/v1/databases" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d '{"name":"orders-db","type":"redis"}'
```

```text
HTTP/1.1 409 Conflict
{"error":"idempotency key reused with different request body"}
```

The server compares a SHA-256 hash of the request body. Nothing runs, so you cannot accidentally create a Redis instance under a Postgres key. Treat this as a bug in your agent: it generated one key for two different actions.

## Step 4: Know the exact rules

These come from `internal/middleware/idempotency.go` and the route wiring in `internal/api/server.go`.

- **Methods:** `POST` and `PUT` only. Other methods skip the middleware.
- **Scope:** tenant, method, request URI (path plus any query string), and the key. Two tenants can use the same key without colliding. The same key on a different path is a separate entry.
- **Body check:** the body hash is stored with the entry. A different hash under the same scope returns 409. Bodies over 1 MB are rejected with 413.
- **Key length:** at most 128 characters, otherwise 400 `idempotency key too long`.
- **TTL:** 10 minutes. After that the entry expires and the key can be used again.
- **What is stored:** only 2xx responses whose body is at most 8 KB. Error responses are not stored, so a failed attempt can be retried and run again.
- **Capacity:** 50 keys per tenant. When you hit the limit, the oldest completed entry for your tenant is evicted. There is also a server-wide cap of 500.
- **Storage:** in memory. If the server restarts, every key is forgotten. A retry across a restart can run the handler again.
- **Auth first:** the check runs after authentication, so you need a valid key to benefit from it.

Browsers calling the API from an allowed origin can send the header too; `Idempotency-Key` is in the CORS allow list.

## Step 5: Concurrent identical requests

Retries do not always wait for a timeout. An agent can fire the same request twice at once:

```bash
KEY=$(uuidgen)
BODY='{"name":"cache-1","type":"redis"}'

for i in 1 2; do
  curl -sS -i -X POST "$AH_URL/v1/databases" \
    -H "Authorization: Bearer $AH_API_KEY" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: $KEY" \
    -d "$BODY" > "resp-$i.txt" &
done
wait
```

The first request to arrive takes a pending entry and runs the handler. The second sees the pending entry, waits for it to finish, and then replays the stored response. If the first one produced no cacheable response (for example it returned an error), the waiting request runs the handler itself. If the waiting client disconnects, the server stops waiting and returns 409 `a request with this idempotency key is still in progress`.

Expect one `resp` file without `Idempotency-Replayed` and one with it, assuming the first succeeded.

## Step 6: What is not covered

A key does not protect these, so do not rely on it for them:

- `DELETE` and `PATCH`. They skip the middleware entirely.
- `POST /v1/auth/keys`. Any path starting with `/v1/auth/keys` is skipped, because the response contains a secret that should be shown once.
- `POST /v1/environments/{id}/exec`. It is registered in a route group without the idempotency middleware, on purpose: running a command twice should run it twice.
- `GET` requests. They are not mutating, so there is nothing to protect.

Also note that `ahc`, the CLI, has no idempotency flag in its source. If you want this behavior from a script, use `curl` or your own HTTP client.

## How it works

Follow the diagram from the top.

In the first lane, the request reaches `ah`, passes auth and rate limits, and enters the idempotency middleware. The middleware builds the lookup string from tenant, method, request URI, and key, hashes the body, and finds nothing. It reserves a pending entry, then calls `handleDatabaseCreate`. The response is recorded as it is written. If it is a 2xx and small enough, it is saved under the key.

In the second lane, the client timed out and retried. The middleware finds the saved entry, sees the body hash matches, and writes the saved status, body, and `Idempotency-Replayed: true`. The handler never runs, so there is no second database.

In the third lane, the key matches but the hash does not. The answer is 409 and nothing runs.

## The pattern to use

1. Generate the key once, when your agent decides to take an action. Not when it sends the request.
2. Store it next to the action in your agent's state, so a restarted agent process can reuse it.
3. Send the same key and the same body on every retry of that action.
4. Make a new key for a new action, even if the body looks similar.
5. Retry within 10 minutes. After that, check with `GET /v1/databases` before creating again, because the key will have expired.
6. If the server may have restarted, check first too, since the in-memory store would be empty.

## What to try next

- [Postgres and Redis](https://agentic.hosting/docs/databases/) covers the database endpoints this post used.
- [API reference](https://agentic.hosting/docs/api/) lists headers, status codes, and error shapes.
- [Rate limits](https://agentic.hosting/docs/rate-limits/) explains why a 429 is a different problem from a duplicate.
- [For agents](https://agentic.hosting/docs/for-agents/) collects the conventions agents should follow.
- The source is on GitHub: [dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).
