# Admin tenant suspend

## What changed

Operators can cut off a tenant without holding one of its API keys.

```
POST /v1/tenants/{tenantID}/suspend
X-Admin-Token: <admin-token>
```

Same durable effect as `DELETE /v1/tenant`:

- `tenants.status = suspended`
- all live API keys revoked (auth cache evicted)
- builds cancelled; environments, services, databases, kanban stopped
- rows kept for `POST /v1/tenants/{id}/reactivate`

Auth is the same as recover / reactivate / quotas: `X-Admin-Token`, `adminLimiter`, bootstrap fallback only when `AH_ADMIN_TOKEN` is unset.

`ahc suspend <tenant-id> --confirm` sends the admin token (`--admin-token` / `$AH_ADMIN_TOKEN`).

404 if the tenant is missing. 409 if it is already suspended. 204 after the DB commit; workload stop runs in the background so the 30s public-route timeout cannot hide a successful kill switch.

## Why

Recover, reactivate and quota patch already took the admin token. Suspend required `DELETE /v1/tenant` with a tenant Bearer key. An operator who never kept that key had to mint a new one via recover first.

## Not changed

- Tenant self-service `DELETE /v1/tenant` is unchanged (still waits for workload stop on the long-route group).
- There is no separate “revoke all keys but leave the tenant running” route. Suspend is the kill switch.
