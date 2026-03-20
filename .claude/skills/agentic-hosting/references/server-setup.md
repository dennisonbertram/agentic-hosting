# Fresh Server Setup

Complete procedure for installing agentic-hosting on a bare Ubuntu 22.04/24.04 server.

**Requirements**: Root access, ports 80 + 443 open, 2+ GB RAM, 20+ GB disk.

---

## 1. Install Docker

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
docker network create traefik-public
```

## 2. Install gVisor (container sandbox)

```bash
curl -fsSL https://gvisor.dev/archive.key \
  | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] \
  https://storage.googleapis.com/gvisor/releases release main" \
  > /etc/apt/sources.list.d/gvisor.list
apt-get update && apt-get install -y runsc
runsc install          # registers runsc as a Docker runtime
systemctl restart docker
docker info 2>/dev/null | grep runsc   # must show: Runtimes: ... runsc
```

## 3. Install Go (required — CGO_ENABLED=1 for sqlite3)

**Must build on the server** — cross-compiling with CGO_ENABLED=0 does not work.

```bash
# Check latest: https://go.dev/dl/
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
source /etc/profile.d/go.sh
go version   # must show go1.25.x
```

## 4. Install Nixpacks (Git-based builds)

```bash
curl -sSL https://nixpacks.com/install.sh | bash
which nixpacks   # must return a path
```

## 5. Clone and Build

```bash
mkdir -p /agentic-hosting
cd /agentic-hosting
git clone https://github.com/dennisonbertram/agentic-hosting .
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=1 go build -o /usr/local/bin/paasd ./cmd/ah/
paasd --help   # verify binary works
```

## 6. Generate Secrets

```bash
mkdir -p /var/lib/paasd

# Master key — encrypts all database credentials; back this up off-server
head -c 32 /dev/urandom | xxd -p -c 64 > /var/lib/paasd/master.key
chmod 600 /var/lib/paasd/master.key

# Bootstrap token — required to register new tenants; save this securely
BOOTSTRAP_TOKEN=$(openssl rand -hex 32)
echo "AH_BOOTSTRAP_TOKEN=$BOOTSTRAP_TOKEN" > /etc/default/paasd
chmod 600 /etc/default/paasd
echo ""
echo "⚠ Bootstrap token (save this — it cannot be recovered):"
echo "  $BOOTSTRAP_TOKEN"
```

## 7. Create Systemd Service

```bash
cat > /etc/systemd/system/paasd.service << 'EOF'
[Unit]
Description=paasd - Agentic PaaS daemon
After=docker.service
Requires=docker.service

[Service]
Type=simple
EnvironmentFile=/etc/default/paasd
ExecStart=/usr/local/bin/paasd serve \
  --port 8080 \
  --db-path /var/lib/paasd/paasd.db \
  --master-key-path /var/lib/paasd/master.key \
  --dev
  # Optional: enable automatic subdomain routing for all services
  # Requires wildcard DNS record *.apps.example.com → this server's IP
  # --base-domain apps.example.com
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=paasd

UMask=0077
NoNewPrivileges=yes
ProtectHome=yes
PrivateTmp=yes
ProtectSystem=full
ReadWritePaths=/var/lib/paasd /etc/traefik/dynamic

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now paasd
systemctl status paasd   # must show Active: active (running)
```

Verify:
```bash
curl -s http://127.0.0.1:8080/v1/system/health
# → {"status":"ok"}
```

## 8. Set Up Traefik (TLS + Reverse Proxy)

```bash
mkdir -p /etc/traefik/dynamic /etc/traefik/certs
touch /etc/traefik/certs/acme.json
chmod 600 /etc/traefik/certs/acme.json
```

Create `/etc/traefik/traefik.yml` — replace `admin@example.com` with your email:

```yaml
entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
  websecure:
    address: ":443"

certificatesResolvers:
  letsencrypt:
    acme:
      email: admin@example.com
      storage: /certs/acme.json
      httpChallenge:
        entryPoint: web

providers:
  docker:
    endpoint: "unix:///var/run/docker.sock"
    exposedByDefault: false
    network: traefik-public
  file:
    directory: /dynamic
    watch: true

api:
  dashboard: true
  insecure: false
```

Create the Traefik dynamic config for the API (`/etc/traefik/dynamic/paasd.yml`):

```yaml
http:
  routers:
    paasd:
      rule: "Host(`your-domain.com`) && PathPrefix(`/v1`)"
      entryPoints:
        - websecure
      service: paasd-svc
      tls:
        certResolver: letsencrypt

  services:
    paasd-svc:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:8080"
```

Start Traefik:
```bash
docker run -d --name paas-traefik \
  --restart=unless-stopped \
  --network traefik-public \
  --add-host host.docker.internal:host-gateway \
  -p 80:80 -p 443:443 -p 8090:8080 \
  -v /etc/traefik/traefik.yml:/etc/traefik/traefik.yml:ro \
  -v /etc/traefik/dynamic:/dynamic:ro \
  -v /etc/traefik/certs:/certs \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  traefik:latest
```

## 9. Start Local Registry (for Nixpacks builds)

```bash
docker run -d --name paas-registry \
  --restart=unless-stopped \
  -p 127.0.0.1:5000:5000 \
  registry:2
```

## 10. Register First Tenant

```bash
BOOTSTRAP_TOKEN=$(grep AH_BOOTSTRAP_TOKEN /etc/default/paasd | cut -d= -f2)

curl -s -X POST http://127.0.0.1:8080/v1/tenants/register \
  -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"admin","email":"you@example.com"}' | python3 -m json.tool
```

The `api_key` in the response is shown **exactly once**. Save it immediately:

```bash
export AH_KEY="keyid.secret-from-above"
export AH_URL="https://your-domain.com"
```

Verify end-to-end:
```bash
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/system/health/detailed | python3 -m json.tool
# Should show: status ok, docker ok, gvisor ok, disk info
```

---

## Post-Install Checklist

- [ ] `systemctl status paasd` → active (running)
- [ ] `curl http://127.0.0.1:8080/v1/system/health` → `{"status":"ok"}`
- [ ] `docker info | grep runsc` → gVisor runtime present
- [ ] `/var/lib/paasd/master.key` backed up off-server
- [ ] Bootstrap token saved securely (from `/etc/default/paasd`)
- [ ] Traefik is routing your domain to port 8080
- [ ] First tenant registered with working API key
- [ ] (Optional) If using `--base-domain`: wildcard DNS record `*.apps.example.com A <server-ip>` is live and resolving

---

## Key File Locations (this server)

| File | Purpose |
|------|---------|
| `/usr/local/bin/paasd` | The daemon binary |
| `/var/lib/paasd/paasd.db` | SQLite state database |
| `/var/lib/paasd/master.key` | Encryption key for DB credentials |
| `/etc/default/paasd` | `AH_BOOTSTRAP_TOKEN` env var |
| `/etc/systemd/system/paasd.service` | Systemd unit |
| `/etc/traefik/` | Traefik config and TLS certs |
| `/etc/traefik/dynamic/` | Hot-reload routing rules |
| `/var/lib/paasd/backups/` | Auto-backup destination |

---

## Security: Isolating Tenant Containers from Traefik (Production Multi-Tenant)

By default, Docker containers in the same network can reach each other freely. In a multi-tenant deployment, a tenant container should not be able to connect directly to Traefik's port 80/443 — this could allow tenants to bypass access controls or perform SSRF against the proxy.

Add `DOCKER-USER` iptables rules to block tenant-to-Traefik traffic. The `DOCKER-USER` chain is evaluated before Docker's own rules and survives container restarts:

```bash
# Block tenant containers from connecting to Traefik on port 80
iptables -I DOCKER-USER -m conntrack --ctstate NEW -p tcp --dport 80 -j DROP

# Block tenant containers from connecting to Traefik on port 443
iptables -I DOCKER-USER -m conntrack --ctstate NEW -p tcp --dport 443 -j DROP
```

**Explanation:**
- `DOCKER-USER` is a chain Docker creates specifically for operator-managed rules — rules here are not overwritten when Docker restarts.
- `-m conntrack --ctstate NEW` matches only new connection attempts (not established/related traffic), which keeps existing connections intact.
- `-j DROP` silently drops the packet rather than sending RST (use `-j REJECT` if you prefer immediate error feedback to tenants).

**Make persistent** (rules are lost on reboot without this):
```bash
apt-get install -y iptables-persistent
netfilter-persistent save
```

These rules apply to production multi-tenant deployments. In single-tenant or dev environments, this is not required.

---

## Upgrading

Build and redeploy in-place (Go must be on the server — CGO required):

```bash
cd /path/to/agentic-hosting-source
git pull
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=1 go build -o /usr/local/bin/paasd ./cmd/ah/

# Kill any orphaned process on :8080 first
lsof -ti :8080 | xargs kill 2>/dev/null || true

systemctl restart paasd
journalctl -u paasd -n 20   # check for migration logs and startup
```

New migrations apply automatically on startup.
