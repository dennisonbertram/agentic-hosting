# API key roles

## What changed

`api_keys.role` is `owner` or `member` (migration `state_024_api_key_role.sql`).

| Action | owner | member |
|--------|-------|--------|
| Deploy, logs, DBs, env, sandboxes | yes | yes |
| `POST /v1/auth/keys` (creates `member`) | yes | yes |
| `POST /v1/auth/keys` with `role=owner` | yes | 403 |
| `DELETE /v1/auth/keys/{id}` | yes | 403 |
| `DELETE /v1/tenant` | yes | 403 |

Existing rows default to `owner` so current tenants do not lose admin. Registration, recover, and reactivate insert `owner`. Extra keys minted for agents default to `member`.

## Why

Any key on a tenant could revoke the operator's leftover registration key and suspend the tenant. The isolation boundary is still the tenant; this only stops a leaked agent key from taking over the tenant.

## Not changed

- No other scopes (read-only, deploy-only, etc.).
- Last-owner protection is not implemented: an owner can revoke the last owner key.
- Admin-token routes are unchanged.
