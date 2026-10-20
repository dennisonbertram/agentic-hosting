# agentic-hosting Security Review
Date: 2026-10-20

Scope: PR #168 (`fix/codebase-review-2026-10-20` vs `main`): auth, rate limiting, tenant isolation, reconciler/circuit breaker, container runtime selection, disposable sandboxes (TTL, quotas, egress gating, firewall script, disk limits) and code near the master key and bootstrap token paths.

Method: four read-only review passes (auth, injection/escape, secrets, DoS), run as Claude subagents reading the code directly. A separate verifier tried to refute every finding and re-rated its severity. No code was sent to external services. Nothing was tested against a live host.

## Summary
- CRITICAL: 0 issues
- HIGH: 0 issues
- MEDIUM: 2 issues (both fixed)
- LOW: 11 issues (8 fixed, 3 deferred with documentation)

One more finding (a build whose final status write fails keeps a concurrent-build slot) was rejected by the verifier: it needs a database failure, and an attacker cannot trigger it.

Fixes are in commit `4437f8c` unless noted.

## Critical & High Issues

None.

## Medium Issues

1. **Sandbox disk was unbounded by default.** `internal/environments/sandbox_hardening.go` (`resolveSandboxDisk`) and `internal/docker/client.go` (`workspaceMount`). Unless the operator opted into `AH_SANDBOX_WORKSPACE_TMPFS`, `/workspace` was an unbounded named volume, so any tenant could `dd` the host disk full through exec. **Fixed:**
   - `AH_SANDBOX_WORKSPACE_TMPFS` now defaults to `true`, so `/workspace` is a tmpfs of `disk_mb`.
   - The reaper has a disk-pressure watchdog (`DeleteSandboxesForDiskPressure`). When the data dir or Docker root reaches the 90% block threshold, it deletes running sandboxes, largest `disk_mb` first, and logs `AUDIT: action=sandbox.disk_pressure_deleted` for each.
   - The root filesystem is fully bounded only with `AH_SANDBOX_RUNTIME=runsc-sbx` (`setup-linux.sh --sandbox-hardening`).
2. **Sandbox image pulls had no size or concurrency limit.** `internal/environments/sandbox.go` (`CreateSandbox`). Disk was checked only before the pull, so several large pulls could all start at 79% and fill the disk. **Fixed:**
   - Pulls go through a host-wide semaphore (`AH_SANDBOX_MAX_CONCURRENT_PULLS`, default 2), after the quota INSERT.
   - User-chosen images over `AH_SANDBOX_MAX_IMAGE_MB` (default 4096) are rejected with 400 and removed if this create pulled them.
   - The disk check runs again after the pull and returns 503 if usage crossed the threshold.
   - Template images already present on the host are no longer pulled again.

## Low / Informational

Fixed:
- `internal/api/recovery.go`: concurrent recoveries could push a tenant past 20 keys (separate count, then INSERT). The key limit is now checked inside the INSERT, with bounded revoke-and-retry.
- `internal/api/recovery.go`: a successful key recovery wrote no AUDIT line. It now logs `AUDIT: action=recovery.key_created` with tenant, key ID (never the token), auth type, IP and revoked key.
- `cmd/ah/sandbox_hardening.go` `isPlainRule`: rules narrowed to one destination or to another interface could satisfy the egress firewall check. Each check now accepts only its own selector (`-i` for the jump and INPUT drops, `-d` for the range drops).
- `cmd/ah/main.go`: with `runc` in `--dev`, ah could listen on a non-loopback address. ah now refuses to start when the container or sandbox runtime is not gVisor and the listen address is not loopback.
- `scripts/setup-linux.sh`: the repair step ran `chown`/`chmod` as root on `/var/lib/ah/master.key`, which the ah user could replace with a symlink. The script now requires a regular, non-symlink file and uses `chown -h`.
- `scripts/sandbox.sh`, `cmd/ahc/recover.go`, `docs/runbooks/sandboxes-hardening.md`: secrets on argv. The OpenRouter key and demo token now reach python through the environment, the `--admin-token` help warns about shell history and `ps`, and runbook `curl` examples read the admin token from a process substitution.
- Sandbox create always pulled template images from the registry, so a tenant could use up the host's Docker Hub quota. Templates now use the local image when it is present. User-chosen images are still always pulled, so a tenant cannot run local-only images.

Deferred (documented):
- **Egress firewall checked only at startup** (`cmd/ah/sandbox_hardening.go`). A ufw/firewalld/fail2ban reload while ah runs is not detected. The runbook tells operators to `systemctl restart ah` after any firewall reload. A periodic re-check needs a root timer that refreshes the snapshot; that is follow-up work. Egress is off by default.
- **Host-wide sandbox cap has no per-tenant fairness** (`internal/environments/environments.go`). Four tenants at default quotas can hold all 20 host slots for up to 24h. The runbook now says that `max_sandboxes` × tenants should stay at or below `AH_MAX_SANDBOXES_TOTAL`.
- **Admin routes reveal whether a token is a bootstrap token** (`internal/api/tenants.go` `requireAdmin`: 403 vs 401), with a per-IP budget separate from registration. Not changed: it sits next to bootstrap-token auth, which CLAUDE.md says needs a dedicated security review before modification. Impact is negligible because tokens are at least 32 characters.

## Positive Findings

- Every new `/v1/sandboxes` route is behind auth and the per-tenant and global rate limiters.
- Every sandbox query is scoped by `tenant_id` and `kind`, and environment handlers by `kind='environment'`, so the two kinds cannot be confused.
- Snapshot and service sources are looked up with the caller's tenant ID. Every resolved image passes `ValidateImage`, which limits local-registry refs to the caller's own prefix.
- User-chosen images are always pulled, so a tenant cannot run local-only images by naming a bare tag.
- Runtime enforcement:
  - `AH_CONTAINER_RUNTIME` accepts only `runsc`/`runc`, and `runc` requires `--dev`.
  - `VerifyGVisorRuntime` and `IsGVisorRuntime` identify gVisor by binary path or shim type, never by name.
- Egress sandboxes attach only to the egress bridge.
- An existing egress network is reused only if it has exactly the expected settings (not internal, ICC off, IPv6 off, interface name, label, subnet).
- The firewall check is conservative. Any ACCEPT, RETURN or jump ahead of a required rule fails it. The root-written snapshot must be a regular root-owned file, not group- or world-writable, and at most 10 minutes old.
- Quotas (per tenant and host-wide) are enforced inside the INSERT, so concurrent creates cannot exceed them. Extend cannot pass `max_expires_at`. The reaper's claim re-checks its predicate, so it cannot race extend.
- Sandbox responses never contain env values, container IDs or volume names. `include_env` values are passed only to Docker create and are never persisted.
- `maskConnString` prints `***` for strings it cannot parse. `ahc configure` no longer writes `AH_URL`/`AH_KEY` from the environment into the config file.
- API keys are minted in one place:
  - The stored hash is an HMAC with a dedicated key.
  - The display prefix comes from the key ID.
  - The token is shown once.
  - Key create and recovery check the key limit inside the INSERT.
- Bootstrap and admin token comparison (`validateBootstrapToken`, `hmacEqual`) is unchanged and timing-safe across all configured tokens.
- The idempotency cache key includes the tenant ID, and in-flight entries no longer expire mid-request.
- Log tails are clamped. Follow streams stop on the first failed write. Exec has timeout and output caps.

## Still required before production use of sandbox egress or disk limits

Run the host checklist in `docs/runbooks/sandboxes-hardening.md` on the production host. It covers DNS under gVisor on the egress bridge, the firewall actually blocking metadata/RFC 1918/host ports, and tmpfs writes being charged to the memory cgroup.
