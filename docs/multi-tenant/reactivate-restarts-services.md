# Reactivate restarts services

## What changed

`POST /v1/tenants/{id}/reactivate` now redeploys the tenant's stopped services in the same background job that restarts databases and kanban.

`services.StartAllForTenant` selects rows that are `stopped` or `failed`, have no open circuit, and have a real image (not empty / `pending`), then calls `Deploy` for each. `Start` cannot be used: suspension removes the container and clears `container_id`.

## Why

After `DELETE /v1/tenant`, apps stayed `stopped` until someone hit `/redeploy` on each service. DBs and kanban already came back.

## Not changed

- Environments are still not auto-started (records stay so they can be started later).
- Circuit-open services stay down until `/reset` then `/redeploy`.
- The 200 response still returns before background work finishes.
