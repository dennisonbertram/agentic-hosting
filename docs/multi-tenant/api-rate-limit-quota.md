# Per-tenant API rate limit via quotas

## What changed

`PATCH /v1/tenants/{id}/quotas` accepts optional `api_rate_limit` (min 1, cap 1000). The value is already read per request and applied with burst 2×. It was stored but not settable through the API.

## Why

README “per-tenant rate limits” was only a per-tenant bucket at the default 100 rps. Operators who needed a lower or higher value had to UPDATE SQLite.

## Not changed

- Global limiter stays 500 rps / burst 1000.
- A tenant rate above 500 still hits the global cap.
- Auth cache still holds the last-read rate for up to 30s, so a quota patch can lag one TTL on already-cached keys.
