# Runbook: operating a Linux install

This runbook is for a host set up with `scripts/setup-linux.sh` or `scripts/bootstrap-stack.sh`. The paths are the installer defaults:

| Path | Contents |
|---|---|
| `/usr/local/bin/ah`, `/usr/local/bin/ahc` | Server and CLI |
| `/var/lib/ah/ah.db`, `/var/lib/ah/ah-metering.db` | State and metering SQLite databases (WAL mode) |
| `/var/lib/ah/master.key` | Master key, 64 hex characters, owner `ah`, mode 0600 |
| `/var/lib/ah/backups/` | Output of `ah backup` |
| `/etc/default/ah` | `AH_BOOTSTRAP_TOKEN`, `AH_ADMIN_TOKEN`, `AH_BASE_DOMAIN`, … (root, 0600) |
| `/etc/traefik/dynamic/` | Traefik file-provider routes written by `ah` |
| `/etc/systemd/system/ah*.service` | `ah`, `ah-api-proxy`, `ah-build-broker`, `ah-build@` |

Commands below run as root (`sudo -i`) unless shown with `sudo -u ah`.

## Upgrade

1. Back up first (see [Backup](#backup)).
2. Update the checkout and preview:

   ```sh
   cd /path/to/agentic-hosting
   git fetch && git checkout <release-tag-or-commit>
   scripts/setup-linux.sh --dry-run
   ```

   The dry run should report `mode=existing`.
3. Apply:

   ```sh
   scripts/setup-linux.sh --yes
   ```

   For an existing install the script:
   - checks that Docker is active and lists the `runsc` runtime, and starts a stopped `paas-traefik`;
   - resets ownership and modes on `/var/lib/ah` (0750), `master.key` (0600, `ah:ah`) and `/etc/default/ah` (0600, root);
   - appends a generated `AH_ADMIN_TOKEN` if `/etc/default/ah` has none (the bootstrap token is never touched);
   - rebuilds `ah` and `ahc` and replaces `/usr/local/bin` copies only when they differ;
   - reinstalls changed unit files from `deploy/` and runs `systemctl daemon-reload`;
   - restarts `ah-build-broker` when the `ah` binary or its unit changed (it runs the same binary), and `ah-api-proxy` when its unit changed;
   - restarts `ah` only if the binary, units or env file changed, then checks `GET /v1/system/health`.

   **Behaviour change when the admin token is added:** once `AH_ADMIN_TOKEN` is set, bootstrap tokens no longer authorize recover, reactivate or quota update (they get 401/403). Use `AH_ADMIN_TOKEN` from `/etc/default/ah` for those operations. The script prints a warning when it appends the token.

   Database migrations run automatically when `ah` starts.
4. Verify:

   ```sh
   systemctl --no-pager status ah
   journalctl -u ah -n 50 --no-pager
   ```

**Rollback**: check out the previous commit and run `scripts/setup-linux.sh --yes` again. If the new version applied a migration the old one does not understand, restore the backup taken in step 1 (see [Restore](#restore)).

## Backup

`ah backup [DB_PATH]` writes WAL-consistent copies (`VACUUM INTO`, then gzip) of the state database and, if present, the metering database to `backups/` next to the state database. It keeps the newest 10 of each. It refuses to run when the disk is 90% full.

```sh
sudo -u ah /usr/local/bin/ah backup /var/lib/ah/ah.db
ls -l /var/lib/ah/backups/   # ah-YYYYMMDD-HHMMSS.db.gz, ah-metering-YYYYMMDD-HHMMSS.db.gz
```

Backups are only readable with the same master key. Encrypted env vars, database passwords and API key hashes depend on it. Keep a copy of `/var/lib/ah/master.key` in a secret store, separate from the backups. Copy backups off the host. For continuous off-host replication, see [litestream-replication.md](../ops/litestream-replication.md).

Docker volumes (managed Postgres/Redis data, the registry) are not part of `ah backup`.

## Restore

```sh
systemctl stop ah
cd /var/lib/ah
mkdir -p pre-restore && mv ah.db* ah-metering.db* pre-restore/ 2>/dev/null || true
gunzip -c backups/ah-YYYYMMDD-HHMMSS.db.gz > ah.db
gunzip -c backups/ah-metering-YYYYMMDD-HHMMSS.db.gz > ah-metering.db
chown ah:ah ah.db ah-metering.db && chmod 0600 ah.db ah-metering.db
systemctl start ah
curl -fsS -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
```

The master key on the host must be the one in use when the backup was taken. The reconciler then brings containers back in line with the restored state within about 30 seconds.

## Master key rotation (`ah rotate-key`)

`ah rotate-key` re-encrypts every encrypted field (service and snapshot env vars, database passwords and connection strings, kanban tokens, and the API-key hash key) from the old key to the new one in one transaction. Existing API keys keep working. The server must be stopped.

```sh
# 0. Back up the database and the current key.
sudo -u ah /usr/local/bin/ah backup /var/lib/ah/ah.db

# 1. Generate the new key as the ah user (64 hex chars, mode 0600).
sudo -u ah sh -c 'umask 077; od -An -N32 -tx1 /dev/urandom | tr -d " \n" > /var/lib/ah/master-new.key'

# 2. Stop the server.
systemctl stop ah

# 3. Dry run: reports counts, commits nothing.
sudo -u ah /usr/local/bin/ah rotate-key --old-key-path /var/lib/ah/master.key \
  --new-key-path /var/lib/ah/master-new.key --dry-run

# 4. Rotate.
sudo -u ah /usr/local/bin/ah rotate-key --old-key-path /var/lib/ah/master.key \
  --new-key-path /var/lib/ah/master-new.key

# 5. Swap the files and start.
sudo -u ah mv /var/lib/ah/master.key /var/lib/ah/master-old.key
sudo -u ah mv /var/lib/ah/master-new.key /var/lib/ah/master.key
systemctl start ah
```

`--db-path` defaults to `$AH_DB_PATH`, else `$AH_DATA_DIR/ah.db`, else `/var/lib/ah/ah.db`. Run `ah rotate-key --help` for the full usage. After confirming services start and `ahc service env get <svc> --reveal` decrypts, store the new key in your secret store and destroy `master-old.key`. If step 4 fails, nothing was committed: start `ah` again with the old key.

## Bootstrap and admin token rotation

Both variables accept a comma-separated list. Every token in the list must be at least 32 characters. Rotate without downtime:

1. Edit `/etc/default/ah`: `AH_BOOTSTRAP_TOKEN=<new>,<old>` (and the same pattern for `AH_ADMIN_TOKEN`).
2. `systemctl restart ah`. The log shows `bootstrap token rotation: 2 tokens configured`.
3. Move clients to the new token, then remove the old one and restart again.

Generate a token with `od -An -N32 -tx1 /dev/urandom | tr -d ' \n'`.

## Reading logs

| What | Command |
|---|---|
| API server | `journalctl -u ah -f` (add `--since "10 min ago"`) |
| Traefik → API relay | `journalctl -u ah-api-proxy` |
| Build broker / one build | `journalctl -u ah-build-broker`; `journalctl -u 'ah-build@<build-id>'` |
| Ingress | `docker logs --since 10m paas-traefik` |
| A tenant service | `ahc logs <service> [--follow] [--tail N]`, or `GET /v1/services/{id}/logs` |
| A build | `ahc build logs <service> [build-id]` |
| Host health | `scripts/health-check.sh --dry-run` (see [scripts/README.md](../../scripts/README.md)) |
| Detailed health | `curl -H "Authorization: Bearer $AH_KEY" https://api.<domain>/v1/system/health/detailed` (reports Docker, gVisor, disk, and `container_runtime`) |

`ah` logs to the journal with the syslog identifier `ah`. Messages starting `FATAL:` explain why the server exited. Lines starting `gc:`, `reconciler:` and `warm-pool:` come from the background loops.

## Troubleshooting

| Symptom (log line or behaviour) | Cause and fix |
|---|---|
| `FATAL: gVisor runtime (runsc) not found in Docker daemon` | `/etc/docker/daemon.json` lost its `runtimes.runsc` entry, or `runsc` was removed. Restore `{"runtimes":{"runsc":{"path":"/usr/bin/runsc"}}, …}`, run `systemctl restart docker`, and check `docker info \| grep -i runtimes` |
| `FATAL: AH_CONTAINER_RUNTIME=runc is a dev-only fallback and requires --dev` | Someone set the sandbox runtime on a server. Remove `AH_CONTAINER_RUNTIME` from `/etc/default/ah`. Production must use `runsc` |
| `FATAL: AH_SANDBOX_RUNTIME=… is not registered with Docker` / `… is not gVisor` / `AH_SANDBOX_RUNTIME=runc … requires --dev` | The disposable-sandbox runtime is set but Docker does not have it (or it is not runsc). Run `scripts/setup-linux.sh --sandbox-hardening`, or remove `AH_SANDBOX_RUNTIME` from `/etc/default/ah`. See [sandboxes-hardening.md](sandboxes-hardening.md) |
| `WARNING: sandbox egress is DISABLED: …` | `AH_SANDBOX_EGRESS_NETWORK` is set but the egress bridge or its host firewall rules could not be verified, so `egress: "internet"` keeps returning 403. Fix the reason in the log and restart ah. See [sandboxes-hardening.md](sandboxes-hardening.md) |
| `FATAL: invalid configuration: …` | An `AH_*` value does not parse (for example `AH_SNAPSHOT_MAX_AGE`, `AH_KANBAN_PORT_*`, `AH_WARM_POOL_ENABLED`). Fix the named variable |
| `AH_BOOTSTRAP_TOKEN must be set` / `must be at least 32 characters` | `/etc/default/ah` is missing or has a short token |
| `master key file … contains N characters (expected 64)` | The key file is not 64 hex characters. Restore it from your secret store. Never generate a new one in place: that makes existing data unreadable |
| API healthy locally, public HTTPS fails | DNS not pointing at the host yet, or Traefik down: `docker ps --filter name=paas-traefik`, `docker logs paas-traefik`, `journalctl -u ah-api-proxy`. Do not open port 8081 |
| Service URL returns 404 | No route file: `ls /etc/traefik/dynamic/<service-id>.yml`. Redeploy with `ahc service redeploy <svc>` |
| Service URL returns 502 | The container is not listening on the configured port, or is crashing: `ahc logs <svc>`, then `ahc service get <svc>` |
| `circuit_open: true` | 5 crashes in 10 minutes. Fix the cause, then `ahc service reset <svc>` and `ahc service redeploy <svc>` |
| Deploys fail at about 90% disk | Disk watermark. `docker system df`; remove unused images; `ah` GC runs every 5 minutes |
| Builds never start | `systemctl status ah-build-broker`; `journalctl -u ah-build-broker -n 50`; check that `/usr/local/bin/nixpacks` exists |
| `setup-linux.sh` reports `mode=partial` | An install stopped halfway. Follow "Partial failure and recovery" in [fresh-install.md](../ops/fresh-install.md); do not delete state |

For tenant-side recovery (lost API keys, stuck deploys, database timeouts), see [AGENT_RUNBOOK.md](../../AGENT_RUNBOOK.md).
