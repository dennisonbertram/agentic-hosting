---
title: "Back up and restore the ah database and master key"
description: "Run ah backup on a schedule, copy the master key off the box, and prove the restore works on a scratch host before you trust ah with real workloads."
slug: back-up-and-restore-ah
tags: [backups, operations, sqlite, disaster-recovery, litestream]
cover_diagram: diagrams/17-back-up-and-restore-ah.png
sources:
  - cmd/ah/backup.go
  - cmd/ah/main.go
  - internal/db/db.go
  - internal/diskcheck/diskcheck.go
  - internal/reconciler/reconciler.go
  - internal/config/config_test.go
  - internal/api/server.go
  - deploy/ah.service
  - deploy/litestream.yml
  - docs-site/src/content/docs/backups.md
  - docs-site/src/content/docs/reconciliation.md
  - docs/ops/litestream-replication.md
---

Before you put real workloads on ah, decide what you will do when the host dies. I think this is the first thing to settle, because an agent that deploys services for you will keep adding state you cannot easily rebuild by hand. ah keeps its state in SQLite, and the secrets in that state are encrypted with a key that lives in a separate file. Lose either one and you have a problem. Lose the key and the database is full of text you cannot decrypt.

By the end of this post you will have a backup that runs on a schedule, a copy of the master key stored somewhere else, and a restore you have already done once on a scratch host. You will also know which parts ah does not back up.

![Diagram. Left: ah.db and ah-metering.db go through ah backup (VACUUM INTO, gzip, atomic rename, keep 10) into /var/lib/ah/backups. Right: a dashed box lists what to copy separately: master.key, /etc/default/ah, Docker volumes, registry images. Bottom: restored files go to a new host, where the reconciler compares rows to Docker.](diagrams/17-back-up-and-restore-ah.png)

I checked these commands against the source. I did not run them for this post, so treat the output as a rough shape, not captured text.

## Prerequisites

- An ah host installed from `deploy/ah.service`. That unit runs `ah serve` as user `ah` with the database at `/var/lib/ah/ah.db` and the key at `/var/lib/ah/master.key`.
- A second machine for the restore test. A cheap VM with ah installed but not yet in use is fine.
- Somewhere off the box to keep files, such as another server or object storage.

## Step 1: Run a backup and read what it does

```bash
sudo ah backup /var/lib/ah/ah.db
```

With no argument, `ah backup` uses the configured database path. Passing the path makes it explicit, which helps in cron. Here is what the code does, in order:

1. It creates `backups/` next to the database (`/var/lib/ah/backups`, mode 0700).
2. It checks disk usage on that directory. At 80% used it logs a warning. At 90% it stops and the backup fails.
3. It opens the state database read-only and runs `VACUUM INTO` a temporary file. That gives a consistent snapshot even while ah is writing in WAL mode. There is a 5 minute timeout.
4. It gzips the snapshot to `ah-YYYYMMDD-HHMMSS.db.gz`, writing to a `.tmp` file and renaming it so a half-written file never has the final name.
5. If the metering database exists, it does the same and writes `ah-metering-YYYYMMDD-HHMMSS.db.gz`. The metering path is the state path with `.db` replaced by `-metering.db`, unless `AH_METER_DB_PATH` is set. A metering failure is a warning, not a failed run.
6. It keeps the 10 newest state backups and the 10 newest metering backups, and deletes the rest. The two series are pruned separately.

Expect log lines like this:

```text
backed up ...vacuumtmp → ah-20261019-120000.db.gz (… KB)
backup complete: /var/lib/ah/backups
```

Check the result:

```bash
ls -l /var/lib/ah/backups
gunzip -c /var/lib/ah/backups/ah-*.db.gz | head -c 16   # starts with "SQLite format 3"
```

## Step 2: Know what is not covered

The backup holds metadata and encrypted values. It does not hold:

| Item | Why it matters |
|---|---|
| `/var/lib/ah/master.key` | Without it, encrypted env vars and database passwords cannot be decrypted. It is never written into the gzip files. |
| `/etc/default/ah` | Holds your bootstrap token and other environment settings. `ah.service` reads it. |
| Docker volumes | Postgres and Redis data lives in `ah-db-*` volumes, not in SQLite. |
| Registry data | Built images, unless you pushed them somewhere else. |

Treat the first two as small files you copy every time. The last two need their own plan. Volume dumps are outside what `ah backup` does.

## Step 3: Schedule it and copy files off the box

A backup that lives only on the machine it protects does not survive losing the machine. Here is a root cron entry, as a sketch:

```cron
# /etc/cron.d/ah-backup
17 * * * * root /usr/local/bin/ah backup /var/lib/ah/ah.db >> /var/log/ah-backup.log 2>&1
```

Then ship the files. Pick your own destination and tool. This uses `rsync` over SSH:

```bash
rsync -a /var/lib/ah/backups/ backup-host:/srv/ah-backups/
install -m 600 /var/lib/ah/master.key /root/master.key.copy   # then move it to a secrets manager
scp /etc/default/ah backup-host:/srv/ah-backups/default-ah
```

The key does not change between backups, so copy it once and again whenever you rotate it (`ah rotate-key` exists for that). Store it apart from the database backups. Someone holding both can read your secrets.

A systemd timer works as well as cron. The command is the same.

## Step 4: Restore on a scratch host

Test this step before you need it. On the new host, install ah the same way as on the first one, then copy over the latest backup, `master.key` and `/etc/default/ah`.

```bash
sudo systemctl stop ah

# Move any existing database aside, including WAL files.
sudo mkdir -p /var/lib/ah/old
sudo mv /var/lib/ah/ah.db* /var/lib/ah/ah-metering.db* /var/lib/ah/old/ 2>/dev/null || true

# Decompress into place.
gunzip -c ah-20261019-120000.db.gz          | sudo tee /var/lib/ah/ah.db >/dev/null
gunzip -c ah-metering-20261019-120000.db.gz | sudo tee /var/lib/ah/ah-metering.db >/dev/null

# Restore the key and make sure the ah user owns everything.
sudo install -m 600 master.key /var/lib/ah/master.key
sudo chown ah:ah /var/lib/ah/ah.db /var/lib/ah/ah-metering.db /var/lib/ah/master.key
sudo chmod 600 /var/lib/ah/ah.db /var/lib/ah/ah-metering.db

sudo systemctl start ah
curl -s http://127.0.0.1:8080/v1/system/health
```

There is no `ah restore` command. The restore is these file operations. Stop ah first, because it holds the database open. Moving the old `-wal` and `-shm` files out of the way matters: a stale WAL file next to a restored database is a bad combination. The `ah:ah` ownership matches `User=ah` in the unit file. The health route is public, so it needs no API key.

If you skip the metering file, ah creates a fresh metering database and you lose usage history.

## Step 5: Watch the reconciler catch up

The restored rows describe a world that may no longer exist. A scratch host has none of the old containers, so the rows are wrong from the start. The reconciler runs every 30 seconds and compares them to Docker. Watch it:

```bash
sudo journalctl -u ah -f | grep reconciler
```

For every service row marked `running` whose container Docker cannot find, the reconciler records a crash with the reason `container not found by reconciler` and sets the service to `crashed`. A row already `crashed` with a missing container is not counted again, so the service does not run into the circuit breaker just because the host is new. Environment rows marked `running` with a missing container become `stopped`.

One thing to be aware of: the project's backup page says missing containers end up `stopped`. For services, the code in `internal/reconciler/reconciler.go` marks them `crashed` first. Either way, nothing restarts them on its own.

Bring a service back with a redeploy:

```bash
ahc service redeploy my-api
ahc status
```

Then check that a restored service with env vars deploys and runs. That is the practical test that the restored key matches the restored database. I did not trace in the code exactly where a wrong key would first fail, so treat this as a check to run, not a guarantee.

## Optional: Litestream for continuous replication

`ah backup` gives you periodic snapshots, so you can lose up to the interval between runs. If that is too much, the repo includes `deploy/litestream.yml` and a runbook at `docs/ops/litestream-replication.md` for streaming both databases to an S3-compatible bucket with [Litestream](https://litestream.io/).

This is separate from the ah binary. ah does not install, start or monitor Litestream, and `ah backup` does not know it exists. You run Litestream as its own service. It does not replicate `master.key`, so Step 3 still applies. Follow the runbook for the restore commands, and note that the runbook uses `/healthz` in one check where the route in the code is `/v1/system/health`.

## How it works

Look at the diagram again. SQLite in WAL mode keeps recent writes in a side file, so copying `ah.db` with `cp` can give you a database that is missing recent writes or is inconsistent. `VACUUM INTO` reads through SQLite itself and writes a clean standalone file, which is why `ah backup` uses it. Gzip plus an atomic rename means the newest `.db.gz` is always complete.

The dashed box on the right is the part people forget. The database is only half of the state: the key decrypts it, and the volumes hold your data. The bottom row shows why restore is safe to repeat. ah does not trust the restored rows, and the reconciler checks them against Docker.

## What to try next

- Repeat the restore drill on a schedule, not just once. A backup you have never restored is a guess.
- Read [Backups](https://agentic.hosting/docs/backups/) and [Reconciliation](https://agentic.hosting/docs/reconciliation/) for the full reference.
- Read [Troubleshooting](https://agentic.hosting/docs/troubleshooting/) if a restored service will not come back.
- Star or file issues at [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).
