---
title: Registration & API keys
description: How tenants register with a bootstrap token, how Bearer keyID.secret works, and how to recover or rotate keys.
section: Use
order: 5
---

# Registration & API keys

Access is a **tenant** plus one or more **API keys**. Keys are `keyID.secret`. The secret is returned once. The database stores an HMAC-SHA256 of the full key using the server master key (`internal/crypto.HashAPIKey`). Verification is constant-time (`hmac.Equal`).

bcrypt is used for **passwords** (kanban / DB credential hashing helpers), not for API keys.

## Bootstrap token

Production will not start without `AH_BOOTSTRAP_TOKEN` unless you pass `--dev --open-registration`.

- Minimum length **32** characters per token
- Comma-separated list is allowed: `AH_BOOTSTRAP_TOKEN=new,old` — both work during rotation
- Compared with HMAC (no length leak) in `validateBootstrapToken`
- Sent as header **`X-Bootstrap-Token`** (not `Authorization`)

Validate without registering:

```bash
curl -s -X POST "$AH_URL/v1/system/bootstrap-token/validate" \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN"
```

(The skill file still mentions `/v1/auth/bootstrap/validate`. That path is **not** registered. The router mounts `POST /v1/system/bootstrap-token/validate`.)

## Register

```
POST /v1/tenants/register
X-Bootstrap-Token: <token>
{"name":"my-agent","email":"agent@example.com"}
```

Rules from `handleTenantRegister`:

- Name 2–128 characters
- Email must match `^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`, max 256
- Max **1000** active tenants
- Duplicate name/email → **422** (`registration failed, please check your details and try again`)
- Rate limit: **5 / IP / hour** and **20 / hour global** (same limiter as recover, reactivate, quota patch, bootstrap validate)

Response:

```json
{"tenant_id":"<hex>","api_key":"<keyID>.<secret>"}
```

`--dev --open-registration` skips the bootstrap header. Anyone who can reach the port can create tenants.

## Authenticated requests

```
Authorization: Bearer <keyID>.<secret>
```

Auth middleware (`internal/middleware/auth.go`):

- Looks up the key by ID, checks not revoked / not expired / tenant `active`
- Verifies HMAC
- Caches the lookup **30 seconds** (max 5000 entries) so revocation can lag up to 30s
- Samples `last_used_at` at most every 5 minutes

Suspended tenants cannot authenticate.

## Create, list, revoke

```
POST   /v1/auth/keys     {"name":"ci","expires_in":2592000}
GET    /v1/auth/keys
DELETE /v1/auth/keys/{keyID}
```

- `name` defaults to `unnamed`
- `expires_in` is seconds, must be > 0, max ~10 years
- Response field is **`api_key`** (not `key`)
- List never returns the secret; it returns `id`, `name`, `prefix`, `created_at`, `last_used_at`, `expires_at`
- Max **20** non-revoked keys per tenant → **409** `quota exceeded: maximum API keys reached...`

Creating a key invalidates the auth cache for that tenant.

## Recover lost keys

When every key is gone:

```
POST /v1/auth/recover
X-Bootstrap-Token: <token>
{"email":"agent@example.com"}
```

Email must match the tenant. Issues a new key. Same registration rate limiter.

## Reactivate a suspended tenant

`DELETE /v1/tenant` soft-deletes: status becomes suspended and services/DBs/kanbans/builds are stopped.

```
POST /v1/tenants/{tenantID}/reactivate
X-Bootstrap-Token: <token>
```

(The skill's `POST /v1/tenant/reactivate` is **not** a route.)

Returns a new API key. Rate-limited like registration.

## Tenant self-service

| Method | Path | Notes |
|--------|------|--------|
| GET | `/v1/tenant` | `id`, `name`, `email`, `status`, timestamps |
| PATCH | `/v1/tenant` | **`name` only** (2–128 chars). Email is not updatable here. |
| DELETE | `/v1/tenant` | Suspend + stop resources |
| GET | `/v1/tenant/usage` | Used/max for services, databases, API keys; memory/cpu/disk/rate_limit quotas |
| GET | `/v1/tenant/usage/metrics` | Metering rollups (`period=hourly\|daily`, `since`/`until` RFC3339) |

Operator quota changes:

```
PATCH /v1/tenants/{tenantID}/quotas
X-Bootstrap-Token: <token>
```

Same bootstrap rate limiter.

## `ahc` equivalents

```bash
ahc register --name my-agent --email agent@example.com --save
ahc key create --name ci
ahc key list
ahc key revoke <keyID>
ahc recover --email agent@example.com
```

Config file expected by `ahclient.LoadConfig` is `~/.ah/config.json` with `url` and `key`.
