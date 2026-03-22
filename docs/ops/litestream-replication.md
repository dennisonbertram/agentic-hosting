# Litestream Replication & Disaster Recovery Runbook

This document covers continuous SQLite replication with [Litestream](https://litestream.io/)
for the two databases used by `ah`:

| Database | Default path | Contents |
|---|---|---|
| State DB | `/var/lib/ah/ah.db` | Tenants, services, deployments, secrets, kanbans |
| Metering DB | `/var/lib/ah/ah-metering.db` | Usage counters and billing events |

Both databases run in WAL mode. Litestream streams WAL pages to an
S3-compatible object store so you can restore to any point in time.

---

## 1. Prerequisites

### Install Litestream

```bash
# Debian / Ubuntu
wget -qO- https://github.com/benbjohnson/litestream/releases/latest/download/litestream-linux-amd64.deb \
  | dpkg -i -

# Verify
litestream version
```

### Create an S3 bucket

Any S3-compatible provider works (AWS S3, Backblaze B2, MinIO, Hetzner Object Storage, etc.).

```bash
aws s3 mb s3://ah-backups --region us-east-1
```

### Create IAM credentials

The IAM user (or service-account equivalent) needs these permissions on the bucket:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::ah-backups",
        "arn:aws:s3:::ah-backups/*"
      ]
    }
  ]
}
```

### Configure credentials on the server

Copy the example env file and fill in your credentials:

```bash
cp deploy/litestream.env.example /etc/default/litestream
chmod 600 /etc/default/litestream
# Edit /etc/default/litestream and set LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY
```

---

## 2. Enable replication

### Copy the config file

```bash
cp deploy/litestream.yml /etc/litestream.yml
```

### Start Litestream

Litestream wraps the replicate process. The simplest approach is to run it
as a systemd service alongside `ah.service`.

```bash
# Load credentials into the environment and start replication
export $(grep -v '^#' /etc/default/litestream | xargs)
litestream replicate -config /etc/litestream.yml
```

For a persistent setup, create a systemd unit:

```ini
# /etc/systemd/system/litestream.service
[Unit]
Description=Litestream SQLite Replication
After=network.target ah.service

[Service]
Type=simple
EnvironmentFile=/etc/default/litestream
ExecStart=/usr/bin/litestream replicate -config /etc/litestream.yml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now litestream.service
```

---

## 3. Verify replication is working

### List snapshots

```bash
export $(grep -v '^#' /etc/default/litestream | xargs)

# State DB snapshots
litestream snapshots -config /etc/litestream.yml /var/lib/ah/ah.db

# Metering DB snapshots
litestream snapshots -config /etc/litestream.yml /var/lib/ah/ah-metering.db
```

You should see at least one snapshot with a recent timestamp.

### List WAL segments

```bash
# State DB WAL segments
litestream wal -config /etc/litestream.yml /var/lib/ah/ah.db

# Metering DB WAL segments
litestream wal -config /etc/litestream.yml /var/lib/ah/ah-metering.db
```

WAL segments should be continuously appearing as writes occur.

### Check Litestream logs

```bash
journalctl -u litestream.service -f
```

Look for periodic "replicated" messages. Any S3 errors will appear here.

---

## 4. Disaster recovery — full restore

> **CRITICAL**: You must stop `ah.service` before restoring. Running the
> application against a database that is being overwritten will corrupt data.

### Step-by-step restore

```bash
# 1. Stop ah and Litestream
systemctl stop ah.service
systemctl stop litestream.service

# 2. Back up the current (possibly corrupt) databases
mv /var/lib/ah/ah.db /var/lib/ah/ah.db.broken
mv /var/lib/ah/ah-metering.db /var/lib/ah/ah-metering.db.broken
# Also move WAL/SHM files if they exist
mv /var/lib/ah/ah.db-wal /var/lib/ah/ah.db-wal.broken 2>/dev/null || true
mv /var/lib/ah/ah.db-shm /var/lib/ah/ah.db-shm.broken 2>/dev/null || true
mv /var/lib/ah/ah-metering.db-wal /var/lib/ah/ah-metering.db-wal.broken 2>/dev/null || true
mv /var/lib/ah/ah-metering.db-shm /var/lib/ah/ah-metering.db-shm.broken 2>/dev/null || true

# 3. Load credentials
export $(grep -v '^#' /etc/default/litestream | xargs)

# 4. Restore state DB
litestream restore -config /etc/litestream.yml -o /var/lib/ah/ah.db /var/lib/ah/ah.db

# 5. Restore metering DB
litestream restore -config /etc/litestream.yml -o /var/lib/ah/ah-metering.db /var/lib/ah/ah-metering.db

# 6. Verify file ownership and permissions
chown root:root /var/lib/ah/ah.db /var/lib/ah/ah-metering.db
chmod 600 /var/lib/ah/ah.db /var/lib/ah/ah-metering.db

# 7. Quick integrity check
sqlite3 /var/lib/ah/ah.db "PRAGMA integrity_check;"
sqlite3 /var/lib/ah/ah-metering.db "PRAGMA integrity_check;"

# 8. Restart services
systemctl start ah.service
systemctl start litestream.service

# 9. Verify ah is healthy
journalctl -u ah.service --no-pager -n 20
curl -s http://127.0.0.1:8080/healthz
```

### Point-in-time restore

Litestream supports restoring to a specific point in time using the
`-timestamp` flag:

```bash
litestream restore -config /etc/litestream.yml \
  -o /var/lib/ah/ah.db \
  -timestamp "2026-03-22T10:00:00Z" \
  /var/lib/ah/ah.db
```

Use ISO 8601 format. The restore will replay WAL segments up to (but not
past) the specified timestamp.

---

## 5. Monitor replication lag

Litestream does not expose a built-in metrics endpoint. Use these approaches:

### Manual check

Compare the latest WAL segment timestamp with the current time:

```bash
litestream wal -config /etc/litestream.yml /var/lib/ah/ah.db | tail -1
```

The rightmost timestamp column shows when the segment was uploaded. If it is
more than a few minutes behind wall-clock time under normal write load,
replication may be stalled.

### Scripted health check

```bash
#!/usr/bin/env bash
# litestream-lag-check.sh — exit 1 if replication appears stalled
set -euo pipefail

export $(grep -v '^#' /etc/default/litestream | xargs)

for db in /var/lib/ah/ah.db /var/lib/ah/ah-metering.db; do
  snapshot_count=$(litestream snapshots -config /etc/litestream.yml "$db" 2>/dev/null | wc -l)
  if [ "$snapshot_count" -lt 1 ]; then
    echo "ALERT: no snapshots found for $db"
    exit 1
  fi
done

echo "OK: snapshots exist for both databases"
```

Add this to cron or your monitoring system.

---

## 6. Integration with existing WAL backup strategy

`ah` already ships a built-in `ah backup` subcommand that creates gzipped
copies of both databases (using SQLite's backup API). Litestream complements
this by providing continuous, near-realtime replication rather than
periodic snapshots.

**Recommended strategy:**

| Layer | Tool | RPO | Purpose |
|---|---|---|---|
| Continuous | Litestream | Seconds | S3 streaming replication |
| Periodic | `ah backup` | Hours | Local gzipped snapshots for quick rollback |

Both can run simultaneously. `ah backup` uses SQLite's `VACUUM INTO` /
backup API which does not interfere with WAL-mode replication.

---

## 7. Important notes

- **Stop `ah.service` before restoring.** The application holds WAL locks
  that will conflict with the restore process. Restoring while `ah` is
  running will corrupt the database.
- **Litestream requires WAL mode.** Both `ah` databases already use WAL mode
  (`PRAGMA journal_mode=WAL` is set at open time). Do not switch to DELETE
  or TRUNCATE journal mode.
- **Encryption at rest** is the responsibility of the S3 provider. Enable
  server-side encryption (SSE-S3 or SSE-KMS) on the bucket.
- **Bucket versioning** is recommended but not required. It provides an
  additional safety net if Litestream objects are accidentally deleted.
- **The master key file (`/var/lib/ah/master.key`) is NOT a database.**
  Litestream does not replicate it. Back up the master key separately and
  store it in a secure location (e.g., a secrets manager). Without the
  master key, encrypted fields in the state database cannot be decrypted.
