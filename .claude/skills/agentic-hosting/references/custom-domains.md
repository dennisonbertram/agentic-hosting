# Custom Domain Routing

## Current State

Services deployed via the API receive an auto-generated URL in the form `http://<uuid>.localhost`. This is **not publicly routable** — it only resolves inside the server's Docker network.

Custom domain assignment via the API is not yet implemented (GitHub issue #14). Use the Traefik dynamic config workaround below until the API supports it.

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

## When the API Supports Custom Domains (future)

Once issue #14 ships, the workflow will be:

```bash
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/domains \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"domain":"myapp.example.com"}'
```

Until then, use the SSH + Traefik YAML approach above.
