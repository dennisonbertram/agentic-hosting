# paasd Scripts

Bash automation scripts for common paasd operations. All scripts read configuration from environment variables.

## Setup

```bash
export PAASD_URL="https://<your-domain>"   # paasd API base URL
export PAASD_KEY="keyid.secret"             # Your API key
```

For local dev:
```bash
export PAASD_URL="http://localhost:8080"
```

## Scripts

| Script | Description |
|--------|-------------|
| `register.sh` | Register a new tenant and save credentials |
| `deploy.sh` | Deploy a service from git URL or Docker image |
| `status.sh` | Show status of all services and databases |
| `logs.sh` | Stream build logs for a service |
| `db-provision.sh` | Provision a database and wire it to a service |

## Usage

```bash
chmod +x scripts/*.sh

# Register (one-time, needs bootstrap token)
PAASD_BOOTSTRAP_TOKEN=<token> ./scripts/register.sh my-tenant me@example.com

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
