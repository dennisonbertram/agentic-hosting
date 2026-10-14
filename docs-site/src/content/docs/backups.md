---
title: Backups
description: ah backup uses SQLite VACUUM INTO, gzip, atomic rename, and keeps the last 10 snapshots.
section: Operate
order: 12
---

# Backups

```bash
ah backup
# optional path:
ah backup /var/lib/ah/ah.db
```

Implementation: `cmd/ah/backup.go`.

## Procedure

1. Destination dir: `{dirname(db)}/backups` (mode 0700)
2. Disk check on that dir at 80 / 90
3. Open the state DB read-only, WAL
4. `VACUUM INTO` a temp file (5 minute timeout) — consistent snapshot under WAL
5. Gzip to `ah-YYYYMMDD-HHMMSS.db.gz` via temp + `rename` (mode 0600)
6. If `{db-stem}-metering.db` exists (for `ah.db` that is `ah-metering.db`), backup it as `ah-metering-YYYYMMDD-HHMMSS.db.gz` (warnings only on failure)
7. Keep the **10 newest** `*.db.gz` files; delete older ones (lexicographic timestamp sort)

This is metadata + encrypted blobs. It is **not** a Docker volume dump. Postgres/Redis data lives in `ah-db-*` volumes and is not inside the SQLite file.

## What you must copy besides SQLite

| Artifact | Why |
|----------|-----|
| `master.key` | Without it, env vars and DB passwords cannot be decrypted |
| `/etc/default/ah` | Bootstrap token(s) |
| Docker volumes | Database contents, environment workspaces |
| Registry data | Built images if you have not pushed elsewhere |

`master.key` is not written into the gzip backup.

## Restore (operational, not a subcommand)

There is no `ah restore`. Stop `ah`, replace `ah.db` with a decompressed VACUUM copy, restore `master.key`, start `ah`. The reconciler will then compare rows to Docker (containers may be gone — expect `stopped` / crash handling).

## Litestream

`docs/ops/litestream-replication.md` exists in the repo as an ops note. The binary itself only implements the `VACUUM INTO` path above.

## Snapshot retention (services, not SQLite)

Service snapshots (image + env forks) are pruned by GC using `AH_SNAPSHOT_MAX_PER_SERVICE` (default 10) and `AH_SNAPSHOT_MAX_AGE` (default 30d). That is unrelated to `ah backup`.
