# Docs vs code fact-check (2026-10-19)

Code is the source of truth. This pass updated live operator docs and agent skills only.

| Wrong claim | Live behavior | Proof |
|-------------|---------------|--------|
| Disk ≥90% → 503 | Sync paths return 500 `{"error":"internal error"}`. Create returns 201; async deploy sets `failed` + `last_error`. | `internal/diskcheck/diskcheck.go` (`fmt.Errorf`); `internal/apierr/errors.go` `WriteAPIError` fallback; `internal/api/services.go` create+async deploy |
| Reset then `/start` | Reset stops **and removes** the container and clears `container_id`. `/start` is 409; use `/redeploy`. | `internal/services/services.go` `ResetCircuitBreaker`, `Start` |
| Poll `status == "circuit_open"` | `status` stays `crashed`; `circuit_open` is a boolean. | `internal/services/services.go` Service JSON; `internal/reconciler/reconciler.go` sets `status='crashed'` |
| Idempotency on DELETE / all mutating routes | POST and PUT only. No PUT routes. `POST /v1/auth/keys` excluded. | `internal/middleware/idempotency.go:146` |
| Default 512 MB / 1 CPU | New tenants: 2048 MB / 2 CPUs from `tenant_quotas`. 512/1 is the fallback if the quota row cannot be loaded. Bounded by the host. | `internal/db/migrations/state_001_init.sql`; `getResourceLimits`; `internal/docker/client.go` |
| Quickstart `nginx:alpine` | ReadonlyRootfs + tmpfs only on `/tmp` `/var/run` `/var/tmp` `/run`. nginx cannot `mkdir /var/cache/nginx`. Use `nginxinc/nginx-unprivileged` on 8080. | `internal/docker/client.go` HostConfig |
| Tenant-derived encryption keys | AES-256-GCM with the server master key. No per-tenant HKDF. | `internal/crypto`; `docs/security/hkdf-key-separation-plan.md` |
| `/logs` while circuit open | 409 `service has no container` (container already removed). | `internal/services/services.go` Logs |
| Unhealthy → stop; auto-recovery restarts | Unhealthy containers are **restarted**. Auto-recovery leaves status `stopped` with no container. | `internal/reconciler/reconciler.go` |
| HTTP limiter ignores `api_rate_limit` | Auth reads the column; limiter uses it with burst 2×. | `internal/middleware/auth.go`, `ratelimit.go` |
| Reconciler every 60s | 30s | `cmd/ah/main.go` |

## Incorrect assumptions this pass found

- “Disk block” was documented as HTTP 503 because that is the status for a missing Docker/build manager. Diskcheck is an untyped error, so the JSON body is the generic 500 `internal error`.
- “Reset then start” was copied from an older lifecycle where the container was only stopped. Reset now removes it.
- Default 512 MB / 1 CPU is a **fallback in `RunContainer`**, not the tenant quota default applied on a normal deploy.
