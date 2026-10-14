---
title: Rate limits
description: Per-tenant 100 rps / burst 200, global 500 / 1000, and the registration limiter of 5 per IP per hour.
section: Operate
order: 11
---

# Rate limits

Two layers sit on **authenticated** routes after auth middleware.

## Per tenant

`middleware.NewRateLimiter(100, 200)` — token bucket **100 requests/second**, burst **200**, keyed by tenant ID.

Headers on every authenticated response:

- `X-RateLimit-Limit`
- `X-RateLimit-Remaining`

On exceed: **429** `{"error":"rate limit exceeded"}` plus `Retry-After`.

If tenant ID is missing on an authenticated route, the limiter **fails closed** with 500 (should not happen).

LRU cap 10_000 tenant buckets; idle entries dropped after 1 hour (cleanup every 5 minutes).

## Global

`middleware.NewGlobalRateLimiter(500, 1000)` — **500 rps**, burst **1000**, process-wide.

Same 429 + `Retry-After` behavior.

## Registration / bootstrap

`registrationLimiter` in `internal/api/tenants.go`:

| Scope | Limit | Window |
|-------|--------|--------|
| Per client IP | 5 | 1 hour |
| Global | 20 | 1 hour |

Applied to:

- `POST /v1/tenants/register`
- `POST /v1/auth/recover`
- `POST /v1/tenants/{id}/reactivate`
- `PATCH /v1/tenants/{id}/quotas`
- `POST /v1/system/bootstrap-token/validate`

IP comes from `trustedRealIP`: `X-Real-Ip` **only** if `RemoteAddr` is loopback (Traefik). `X-Forwarded-For` is not trusted.

Invalid bootstrap tokens **still consume** the limiter (brute-force resistance).

## Quota vs 429

`apierr.QuotaExceeded` is **409 Conflict**, not 429. Hitting `max_services` is a resource conflict. Hammering the API is a rate limit.

The `tenant_quotas.api_rate_limit` column defaults to 100 and is **exposed** on `GET /v1/tenant/usage`, but the HTTP limiter is constructed with the constants above — it does not read that column per request.

## Idempotency is not a rate limit

`Idempotency-Key` dedups retries for **10 minutes**. Same key + same tenant + method + path + body → replayed response. Different body → **409**. Stored response cap 8 KB. Exec is excluded.
