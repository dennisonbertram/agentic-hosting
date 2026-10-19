---
title: Install & quickstart
description: Install ah on Ubuntu with Docker, gVisor, Nixpacks, and Traefik. Register a tenant and deploy your first service.
section: Start
order: 4
---

# Install & quickstart

These steps match `cmd/ah`, `deploy/ah.service`, and `internal/config`. Paths are `/var/lib/ah` and `/etc/default/ah` (not `paasd`).

## Requirements

- Linux (Ubuntu 22.04+ is what the skill runbook assumes)
- Docker with the `runsc` runtime registered
- Go 1.25+ **with CGO** to build `ah` (the CLI `ahc` does not need CGO)
- Nixpacks on `PATH` (default `/usr/local/bin/nixpacks`)
- Traefik on Docker network `traefik-public`
- Loopback registry on `127.0.0.1:5000` if you use git builds
- `AH_BOOTSTRAP_TOKEN` ≥ 32 characters
- `/var/lib/ah/master.key` — exactly 64 lowercase hex characters (32 bytes)

## 1. Docker + gVisor

```bash
curl -fsSL https://get.docker.com | sh

curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
sudo apt-get update && sudo apt-get install -y runsc

cat > /etc/docker/daemon.json <<'EOF'
{
  "runtimes": {
    "runsc": {
      "path": "/usr/bin/runsc"
    }
  },
  "default-address-pools": [{"base":"172.20.0.0/14","size":24}]
}
EOF
sudo systemctl restart docker
docker info 2>/dev/null | grep runsc
```

## 2. Go and Nixpacks

```bash
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
# new shells:
export PATH=$PATH:/usr/local/go/bin

curl -sSL https://nixpacks.com/install.sh | bash
```

Build **on the server**. `CGO_ENABLED=0` is not supported for `ah`.

## 3. Traefik + registry

```bash
docker network create --driver bridge traefik-public

# Traefik needs a static config and a dynamic dir that ah can write.
# See deploy/traefik/ in the repo for the Let's Encrypt layout.

docker run -d --name paas-registry --restart unless-stopped \
  -p 127.0.0.1:5000:5000 \
  -v /var/lib/ah/registry:/var/lib/registry \
  registry:2
```

## 4. Build and install `ah`

```bash
git clone https://github.com/dennisonbertram/agentic-hosting /agentic-hosting
cd /agentic-hosting
CGO_ENABLED=1 go build -o bin/ah ./cmd/ah
sudo cp bin/ah /usr/local/bin/ah

sudo mkdir -p /var/lib/ah/builds /var/lib/ah/backups
sudo head -c 32 /dev/urandom | xxd -p -c 64 | sudo tee /var/lib/ah/master.key >/dev/null
sudo chmod 600 /var/lib/ah/master.key

# Bootstrap token (save this; you need it to register tenants)
openssl rand -hex 32
```

```bash
sudo tee /etc/default/ah <<EOF
AH_BOOTSTRAP_TOKEN=<paste-64-hex-token>
# optional:
# AH_BASE_DOMAIN=apps.example.com
# AH_BOOTSTRAP_TOKEN=new_token,old_token
EOF
sudo chmod 600 /etc/default/ah

sudo cp deploy/ah.service /etc/systemd/system/ah.service
sudo systemctl daemon-reload
sudo systemctl enable --now ah
```

The unit runs `ah serve --port 8080 --db-path /var/lib/ah/ah.db --master-key-path /var/lib/ah/master.key` as user `ah` with Docker socket access. Create that system user before enabling the unit.

Health check from the host (HTTPS middleware only trusts loopback + `X-Forwarded-Proto`):

```bash
curl -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
# {"status":"ok"}
```

## 5. Dev mode (laptop)

```bash
make build
AH_BOOTSTRAP_TOKEN=dev-token-32-chars-minimum ./bin/ah --dev --port 8080
# or open registration (no bootstrap token):
./bin/ah --dev --open-registration --port 8080
```

`--dev` turns off HTTPS enforcement. gVisor missing is a warning. `--open-registration` without `--dev` is a fatal error.

## 6. Register a tenant and deploy

```bash
export AH_URL=https://your-domain   # or http://127.0.0.1:8080 in --dev
export AH_BOOTSTRAP_TOKEN=...

curl -s -X POST "$AH_URL/v1/tenants/register" \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-agent","email":"agent@example.com"}'
# {"tenant_id":"...","api_key":"keyid.secret"}   ← shown once
```

```bash
export AH_KEY='keyid.secret'

curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"web","image":"nginxinc/nginx-unprivileged","port":8080}'
# Stock nginx:alpine crash-loops under ReadonlyRootfs (no /var/cache/nginx tmpfs).
```

Or with the CLI:

```bash
curl -fsSL https://agentic.hosting/install-cli.sh | bash
ahc configure --url "$AH_URL" --key "$AH_KEY"
ahc deploy nginxinc/nginx-unprivileged web --port 8080
ahc status
```

Git deploys are a create + build. See [Deploy from git](./deploy/).

## File layout

```
/var/lib/ah/
├── ah.db
├── ah-metering.db      # created alongside state
├── master.key          # chmod 600
├── builds/
├── backups/            # ah backup
└── registry/           # if you bind-mount it here
```

## Claude Code skill (optional)

```bash
curl -fsSL https://agentic.hosting/install-skill.sh | bash
# installs SKILL.md + /ah-deploy /ah-status /ah-db /ah-logs /ah-register /ah-snapshot
```

Details: [Claude Code skill](./claude-code/).
