---
title: "Rate limits, quotas and usage: what an agent will hit and how to react"
description: "Read the rate-limit headers, honor Retry-After on 429, tell 409 quota errors apart, and check tenant usage and quotas."
slug: rate-limits-quotas-and-usage
tags: [rate-limits, quotas, api, agents, operations]
cover_diagram: diagrams/18-rate-limits-quotas-and-usage.png
sources:
  - internal/middleware/ratelimit.go
  - internal/middleware/limits.go
  - internal/middleware/idempotency.go
  - internal/api/tenants.go
  - internal/api/auth.go
  - internal/api/metering.go
  - internal/api/server.go
  - internal/metering/collector.go
  - internal/metering/metering.go
  - internal/apierr/errors.go
  - internal/services/services.go
  - internal/environments/environments.go
  - internal/databases/databases.go
  - internal/db/migrations/state_001_init.sql
  - internal/db/migrations/state_014_quota_columns.sql
  - docs-site/src/content/docs/rate-limits.md
---

# Rate limits, quotas and usage: what an agent will hit and how to react

If your agent calls the API in a loop, it will eventually get an error that says "no". I think the most useful thing an agent can do with that error is work out which kind of "no" it got, because the right reaction differs. A 429 means wait and retry. A 409 quota error means retrying will never work until something is deleted or the quota is raised.

This post gives you a small client that reads the rate-limit headers and waits on `Retry-After`, a way to tell quota errors from rate limits, and the calls a tenant owner uses to see usage. It also shows the operator call that raises quotas.

![Diagram: a request passes authMW (401), authRateLimiter (429 with Retry-After), globalRateLimiter (429), idempotencyStore (400 or 409) and then the handler, where the quota check inside the INSERT returns 409. Below, the metering collector samples every 60 seconds and feeds GET /v1/tenant/usage/metrics.](diagrams/18-rate-limits-quotas-and-usage.png)

## Prerequisites

- A running agentic-hosting server and an API key in `$AH_API_KEY`. The public API is `https://api.agentic.hosting/v1/`. Self-hosters use their own host.
- `curl` and `jq`.
- For the operator step only, the admin token in `$AH_ADMIN_TOKEN`.

I checked these commands against the code. I did not run them for this post, so treat the output samples as shapes, not captured logs.

```bash
export AH_API=https://api.agentic.hosting/v1
```

## 1. Know the layers

Authenticated routes pass through these limits, in this order (see `internal/api/server.go`):

1. **Auth.** A bad key gets 401.
2. **Per-tenant limiter.** A token bucket keyed by tenant. Default 100 requests per second, burst 200.
3. **Global limiter.** One bucket for the whole server: 500 requests per second, burst 1000. It exists so many tenants cannot add up to an overload.
4. **Idempotency.** Only applies to POST and PUT requests with an `Idempotency-Key` header. It is skipped for `POST /v1/auth/keys` and for `POST /v1/environments/{envID}/exec`, because running the same command twice should run it twice.

Registration is separate. `POST /v1/tenants/register` and a few operator routes use a limiter of 5 requests per IP per hour (and 20 per hour across all callers). A normal agent never sees it.

The per-tenant rate comes from the tenant's `api_rate_limit` quota, with burst set to twice that value.

## 2. Read the headers

Every response from the per-tenant limiter carries two headers:

```bash
curl -si -H "Authorization: Bearer $AH_API_KEY" $AH_API/tenant | grep -i '^x-ratelimit'
```

```
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 199
```

`Limit` is the refill rate in requests per second. `Remaining` is the number of tokens left in the bucket, which can be up to the burst size (200), so it can read higher than `Limit`.

When the bucket is empty you get a 429 with `Retry-After` in whole seconds (minimum 1, capped at 60):

```
HTTP/2 429
Retry-After: 1
{"error":"rate limit exceeded"}
```

The global limiter answers the same way with `{"error":"global rate limit exceeded"}`. Note that the global limiter runs after the per-tenant one and sets no `X-RateLimit-*` headers. Treat any 429 the same way.

A few long-running routes (exec, log follow, provisioning) also have a per-tenant cap on concurrent requests. Over it, you get a 429 with `Retry-After: 5` and a message about too many concurrent long-running requests.

## 3. Write a retry helper

This helper waits for `Retry-After` on a 429 and gives up on anything else. It never retries a 409.

```bash
ah() {  # usage: ah METHOD PATH [curl args...]
  local method=$1 path=$2; shift 2
  for attempt in 1 2 3 4 5; do
    code=$(curl -s -o /tmp/ah.body -D /tmp/ah.hdr -w '%{http_code}' \
      -X "$method" -H "Authorization: Bearer $AH_API_KEY" "$@" "$AH_API$path")
    if [ "$code" = 429 ]; then
      wait=$(awk 'tolower($1)=="retry-after:"{print $2+0}' /tmp/ah.hdr)
      sleep "${wait:-1}"
      continue
    fi
    cat /tmp/ah.body; [ "$code" -lt 400 ]; return
  done
  echo "still rate limited after 5 tries" >&2; return 1
}
ah GET /tenant/usage | jq .
```

If you retry a POST or PUT, send an `Idempotency-Key` so a retry cannot create a second copy. The key has no effect on `POST /v1/auth/keys` or the exec route. The server remembers keys for 10 minutes. Reusing a key with a different body returns 409.

## 4. Treat quota errors as 409

Quotas limit how much you own, and rate limits limit how fast you ask. The code deliberately returns **409 Conflict**, not 429, when you hit a quota. The messages are:

- `quota exceeded: maximum N services reached`
- `quota exceeded: maximum N databases reached`
- `quota exceeded: maximum API keys reached, revoke unused keys first` (the key cap is 20 per tenant)
- `quota exceeded: maximum N environments reached`

```
HTTP/2 409
{"error":"quota exceeded: maximum 5 services reached"}
```

For services and environments, the check happens inside the `INSERT` statement itself (`... WHERE (SELECT COUNT(*) ...) < max`). Two concurrent creates cannot both slip past the limit. When your agent sees this error, it should stop creating, then either delete something or ask a human to raise the quota. Waiting does not help.

## 5. See usage

A tenant can read its own usage:

```bash
curl -s -H "Authorization: Bearer $AH_API_KEY" $AH_API/tenant/usage | jq .
```

```json
{
  "services":  {"used": 2, "max": 5},
  "databases": {"used": 1, "max": 3},
  "api_keys":  {"used": 1, "max": 20},
  "memory_mb": 2048,
  "cpu_cores": 2,
  "disk_gb": 20,
  "rate_limit": 100
}
```

The numbers are an example shape. The `max` values come from your tenant's quota row.

For resource consumption over time, use the metrics endpoints. Both take `period` (`hourly` or `daily`, default `daily`), `since` and `until` as RFC3339 timestamps (default: the last 24 hours). The tenant endpoint also takes an optional `service_id`.

```bash
curl -s -H "Authorization: Bearer $AH_API_KEY" \
  "$AH_API/tenant/usage/metrics?period=hourly" | jq .
curl -s -H "Authorization: Bearer $AH_API_KEY" \
  "$AH_API/services/$SERVICE_ID/metrics?period=daily" | jq .
```

Each row has `timestamp`, `cpu_seconds`, `memory_mb_avg`, `network_rx_bytes` and `network_tx_bytes` (plus `service_id` on the tenant view). Today the collector records only CPU and memory, so the two network fields are always 0. A bad `period` or timestamp returns 400, and a service that is not yours returns 404.

## 6. Raise quotas as the operator

Tenants cannot change their own quotas. The operator uses the admin token:

```bash
curl -s -X PATCH $AH_API/tenants/$TENANT_ID/quotas \
  -H "X-Admin-Token: $AH_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"max_services": 10, "max_databases": 5}'
```

You can set `max_services` (cap 100), `max_databases` (cap 50), `max_builds_concurrent` (cap 10) and `max_env_vars_per_service` (cap 500). Fields you leave out stay unchanged. Both are enforced. A build request is refused with 409 `quota exceeded: maximum N concurrent builds` once the tenant's pending and running builds reach `max_builds_concurrent`. Service create and env set are refused with 409 once the service would have more than `max_env_vars_per_service` keys (updating an existing key does not count). The builder still runs only one build per tenant at a time, so raising `max_builds_concurrent` above 1 adds no parallelism: an extra build is accepted and then fails with `a build is already running for this tenant`. Env updates are also limited to 100 keys per request. The response echoes all quotas, including `max_memory_mb`, `max_cpu_cores`, `max_disk_gb` and `api_rate_limit`. This route uses the admin limiter (5 per IP per hour, separate from registration's), so batch your changes.

## 7. Know the defaults

The schema gives a new tenant these quotas: 5 services, 3 databases, 3 concurrent builds and 50 env vars per service (both enforced, as above), 2048 MB memory, 2 CPUs, 20 GB disk, and an API rate of 100 requests per second. The PATCH route does not accept memory, CPU, disk or rate limit, so those change only by editing the database directly.

## How it works

Read the diagram left to right. Auth runs first, so an invalid key never touches a bucket. The per-tenant limiter caps one tenant's request rate, and the global limiter caps the whole server. Idempotency replays the stored response for a repeated POST or PUT that carries the same key. Finally the handler runs and the quota check happens inside the insert, which is why a quota failure is a 409 and not a rate limit.

The usage metrics come from a separate path. The metering collector wakes every 60 seconds, samples each running service, and stores a row. The metrics endpoints add those rows up by hour or day. A service that just started will not show data until the next sample.

## What to try next

- Add the helper above to your agent's tool wrapper and log every `X-RateLimit-Remaining` that drops below 20.
- Make your agent read `/tenant/usage` before a batch of creates and stop when `used` equals `max`.
- Read the [rate limits](https://agentic.hosting/docs/rate-limits/), [API](https://agentic.hosting/docs/api/) and [guards](https://agentic.hosting/docs/guards/) pages.
- Source and issues: [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).
