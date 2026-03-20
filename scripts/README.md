# agentic-hosting Scripts

Bash automation scripts for common agentic-hosting operations. All scripts read configuration from environment variables.

## Setup

```bash
export AH_URL="https://<your-domain>"   # ah API base URL
export AH_KEY="keyid.secret"             # Your API key
```

For local dev:
```bash
export AH_URL="http://localhost:8080"
```

## Scripts

| Script | Description |
|--------|-------------|
| `register.sh` | Register a new tenant and save credentials |
| `deploy.sh` | Deploy a service from git URL or Docker image |
| `status.sh` | Show status of all services and databases |
| `logs.sh` | Stream build logs for a service |
| `db-provision.sh` | Provision a database and wire it to a service |
| `health-check.sh` | Cron-friendly health check with webhook alerting |

## Usage

```bash
chmod +x scripts/*.sh

# Register (one-time, needs bootstrap token)
AH_BOOTSTRAP_TOKEN=<token> ./scripts/register.sh my-tenant me@example.com

# Deploy from git
./scripts/deploy.sh https://github.com/org/repo my-app 3000

# Deploy from Docker image
./scripts/deploy.sh nginx:alpine my-site 80

# Check status
./scripts/status.sh

# Stream build logs
./scripts/logs.sh my-app

# Provision Postgres and wire to service
./scripts/db-provision.sh my-app postgres

# Provision Redis and wire to service
./scripts/db-provision.sh my-app redis
```

## Health Check Script

`health-check.sh` performs five checks and exits 0 on success or 1 on failure. On failure it POSTs a machine-readable JSON payload to the configured webhook URL.

### Checks performed

1. **Health API** -- `GET /v1/system/health` returns `{"status":"ok"}`
2. **systemd service** -- `systemctl is-active ah` shows `active`
3. **Infrastructure containers** -- `docker ps` contains `paas-traefik` and `paas-registry`
4. **Disk usage** -- warns at 80%, fails at 90% (configurable)
5. **Public HTTPS** -- base domain returns 2xx/3xx (if `AH_BASE_DOMAIN` is set)

### Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ALERT_WEBHOOK_URL` | Yes (unless `--dry-run`) | -- | POST endpoint for failure alerts |
| `AH_API_PORT` | No | `9090` | Local health API port |
| `AH_SERVICE_NAME` | No | `ah` | systemd service name |
| `AH_BASE_DOMAIN` | No | -- | Public domain to probe via HTTPS |
| `AH_DISK_WARN_PCT` | No | `80` | Disk usage warning threshold (%) |
| `AH_DISK_FAIL_PCT` | No | `90` | Disk usage failure threshold (%) |

### Cron setup

Run every 5 minutes (adjust to taste):

```cron
*/5 * * * * ALERT_WEBHOOK_URL="https://hooks.example.com/alert" AH_BASE_DOMAIN="agentic.hosting" /opt/agentic-hosting/scripts/health-check.sh >> /var/log/ah-health.log 2>&1
```

Or with all options:

```cron
*/5 * * * * ALERT_WEBHOOK_URL="https://hooks.example.com/alert" AH_API_PORT=9090 AH_SERVICE_NAME=ah AH_BASE_DOMAIN="agentic.hosting" AH_DISK_WARN_PCT=80 AH_DISK_FAIL_PCT=90 /opt/agentic-hosting/scripts/health-check.sh >> /var/log/ah-health.log 2>&1
```

### Dry-run mode

Test locally without firing the webhook:

```bash
./scripts/health-check.sh --dry-run
```

This runs all checks and prints what would be sent to the webhook, but does not actually POST.

### Webhook payload

On failure the script sends a JSON POST body:

```json
{
  "event": "health_check_failed",
  "hostname": "prod-01",
  "timestamp": "2026-03-20T12:00:00Z",
  "failure_count": 1,
  "warning_count": 0,
  "failures": ["Health API returned HTTP 000 (expected 200)"],
  "warnings": []
}
```
