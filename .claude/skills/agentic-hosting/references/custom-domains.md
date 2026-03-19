# Custom Domain Routing

## Agent Quick Reference

### 1 — Check if base-domain is configured

```bash
curl -s -H "Authorization: Bearer $AH_KEY" \
  $AH_URL/v1/system/health/detailed | python3 -m json.tool
```

Look for `"baseDomain"` in the response:
- `"baseDomain": "apps.example.com"` — subdomain mode active, services get `https://{label}.apps.example.com`
- `"baseDomain": ""` or key absent — localhost mode, services get `http://{service-id}.localhost` (not publicly routable)

### 2 — Predict the URL before creating a service

Given service name `My Blog App` and baseDomain `apps.example.com`:
1. Lowercase: `my blog app`
2. Replace non-alphanumeric with hyphens: `my-blog-app`
3. Trim leading/trailing hyphens (none here)
4. Result: `https://my-blog-app.apps.example.com`

The `url` field in the `POST /v1/services` response confirms the actual assigned URL.

### 3 — What to do if service creation returns 422

A `422` on service creation when base-domain is active means one of:

| Reason | What to do |
|--------|------------|
| Reserved name (`api`, `admin`, `dashboard`, `traefik`, `www`, `auth`, `login`, `registry`) | Rename the service — append a suffix, e.g. `myapp-api` instead of `api` |
| DNS label exceeds 63 characters | Shorten the service name |
| DNS label already claimed by another tenant | Choose a different name — first-come-first-served is global across all tenants |
| Name/email already exists (non-domain cause) | Standard duplicate check — use a unique name |

```bash
# Check what label your name would produce:
echo "my-service-name" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/-/g' | sed 's/^-//;s/-$//'
```

### 4 — URL stability

The URL assigned at creation time is permanent and stored in the database. It will NOT change if:
- The daemon restarts
- `--base-domain` is changed or removed
- The service is stopped and restarted

The DNS label is locked to the service for its lifetime. Deleting the service releases the label.

---

## Current State

There are two URL modes depending on how the daemon was started:

**Localhost mode (default, no `--base-domain`):**
Services receive `http://<uuid>.localhost` — not publicly routable, only accessible inside the server's Docker network. Manual Traefik config is required to expose these publicly (see workaround below).

**Subdomain mode (`--base-domain apps.example.com`):**
Services automatically receive `https://{dns-label}.apps.example.com` via Traefik file provider. No manual Traefik config needed — routing is created automatically when the service is deployed. Requires a wildcard DNS record `*.apps.example.com → server IP` and a wildcard TLS certificate or wildcard ACME challenge.

---

## Workaround: Traefik Dynamic Config

Traefik watches `/etc/traefik/dynamic/` on the server for `.yml` files. Drop a file there and Traefik picks it up within seconds — no restart needed.

### Step 1 — Find the container name

```bash
ssh root@<server> "docker ps --format '{{.Names}}' | grep <service-id-prefix>"
```

Container names follow the pattern: `paasd-<tenant-id>-<service-id>`

### Step 2 — Write the dynamic config

SSH into the server and create `/etc/traefik/dynamic/<yourapp>.yml`:

```yaml
http:
  routers:
    myapp:
      rule: "Host(`myapp.example.com`)"
      entryPoints:
        - websecure
      service: myapp-svc
      tls:
        certResolver: letsencrypt

  services:
    myapp-svc:
      loadBalancer:
        servers:
          - url: "http://<container-name>:80"
```

Replace:
- `myapp.example.com` → your actual domain (DNS must already point to the server)
- `<container-name>` → the full Docker container name from Step 1
- `80` → the port your service listens on

### Step 3 — Verify

```bash
# Watch Traefik pick it up (should be near-instant)
curl -s https://myapp.example.com/
```

### DNS

Point your domain's A record to the server IP before creating the config. Traefik requests a Let's Encrypt cert on first request — the first hit may take 5-10 seconds.

### Caveat: container restarts

Container names are stable across restarts (ah uses deterministic naming), so the config file survives service restarts without changes.

---

## Example: Expose the agentic.hosting website

This is how the main website container is configured:

```yaml
http:
  routers:
    website:
      rule: "Host(`agentic.hosting`)"
      entryPoints:
        - websecure
      service: website-svc
      tls:
        certResolver: letsencrypt

  services:
    website-svc:
      loadBalancer:
        servers:
          - url: "http://paas-website:80"
```

---

## DNS Setup for Subdomain Mode (Server Operators)

To enable `--base-domain apps.example.com`:

1. **Wildcard DNS record**: Add `*.apps.example.com A <server-ip>` at your DNS provider. This routes all subdomains to the server.

2. **Start paasd with the flag**:
   ```bash
   paasd serve --base-domain apps.example.com ...
   ```
   The daemon writes Traefik dynamic config files under `/etc/traefik/dynamic/` automatically when services are deployed.

3. **TLS**: Traefik requests Let's Encrypt certificates per-subdomain on first access. For high-volume deployments consider a wildcard cert to avoid rate limits. Point Traefik's ACME storage at a persistent path.

4. **Verify**: After deploying a test service, the subdomain should respond within 30 seconds (Traefik file provider hot-reloads, Let's Encrypt HTTPS challenge completes).
