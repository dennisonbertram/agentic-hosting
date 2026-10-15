# GitHub Pages + www redirect

`https://agentic.hosting` is the public homepage and docs host. After this cutover:

| Host | Where | What it serves |
|------|--------|----------------|
| `agentic.hosting` | GitHub Pages | `website/` at `/` (homepage, changelog, dashboard, install scripts) and Astro docs at `/docs/` |
| `www.agentic.hosting` | Hetzner (`178.104.44.25`) | TLS still terminates here; nginx 301s to `https://github.com/dennisonbertram/agentic-hosting` |
| `api.agentic.hosting` | Hetzner | Control-plane `/v1/` |
| `*.apps.agentic.hosting` | Hetzner | Tenant services |

Apex `/v1/` goes away with the Pages cutover. Clients must use `https://api.agentic.hosting/v1/`.

## Artifact

`.github/workflows/docs.yml` builds `docs-site/` with `ASTRO_BASE=/docs/` and `ASTRO_SITE=https://agentic.hosting`, then `scripts/assemble-github-pages.sh` writes:

- `website/` static files at the artifact root (`index.html`, `changelog/`, `dashboard/`, `install.sh`, `install-cli.sh`, `install-skill.sh`, `robots.txt`; **not** `Dockerfile` or `nginx.conf`)
- repo-root `llms.txt` / `llms-full.txt` at `/`
- docs build at `/docs/`
- `CNAME` containing `agentic.hosting`
- `.nojekyll`

Local check:

```bash
cd docs-site && npm ci && ASTRO_BASE=/docs/ ASTRO_SITE=https://agentic.hosting npm run build
../scripts/assemble-github-pages.sh ../pages-dist
python3 -m http.server --directory ../pages-dist 4173
# curl http://127.0.0.1:4173/
# curl http://127.0.0.1:4173/docs/
# curl http://127.0.0.1:4173/changelog/
# curl -I http://127.0.0.1:4173/install.sh
```

## Dashboard API base

`website/dashboard/` is a static client. It does **not** assume same-origin with the API when served from Pages:

1. `?api=https://host` wins
2. On `agentic.hosting`, `www.agentic.hosting`, or `*.github.io` it defaults to `https://api.agentic.hosting`
3. Elsewhere it falls back to `window.location.origin` (self-hosted copies)

The API allowlists those dashboard origins (`AH_CORS_ORIGINS` / `--cors-origins`, defaulting to `https://agentic.hosting`, `https://www.agentic.hosting`, `https://dennisonbertram.github.io`). Preflight `OPTIONS` is answered before auth. `Access-Control-Allow-Origin` echoes the request origin (never `*`); `Authorization`, `Content-Type`, and `Idempotency-Key` are allowed; `Vary: Origin` is always set; credentials are not enabled. Other dashboard hosts must be added to the allowlist.

## Manual cutover

Do these in order. Do not merge this PR until they are ready; the PR only ships the files.

1. **Deploy the nginx change on Hetzner** so `www` starts 301ing while apex still works:
   - Checkout this commit on host `170337926` (`178.104.44.25`).
   - Rebuild and recreate `ah-website` from the repo root: `docker build -f website/Dockerfile -t ah-website:bootstrap .` then recreate the container on `traefik-public` with the same IP.
   - Leave `/etc/traefik/dynamic/10-website.yml` matching both `agentic.hosting` and `www.agentic.hosting` until apex DNS has moved. TLS for `www` stays on the existing `tls.certResolver: letsencrypt` router. HTTP `www` still 301s to HTTPS, then nginx 301s to GitHub.
2. **GitHub Pages**: Settings → Pages → custom domain `agentic.hosting` → Enforce HTTPS. Source remains GitHub Actions. The workflow writes `CNAME`.
3. **Namecheap DNS**
   - Apex `@` A records → GitHub Pages: `185.199.108.153`, `185.199.109.153`, `185.199.110.153`, `185.199.111.153`
   - Leave `www` A at `178.104.44.25`
   - Leave `api` and `*.apps` on the Hetzner address
4. After apex resolves to Pages, replace `/etc/traefik/dynamic/10-website.yml` with `deploy/traefik/10-website-www.yml` (Host `www.agentic.hosting` only).
5. Deploy this `ah` binary on Hetzner (CORS lives in the API process, not the website container). Defaults cover the Pages dashboard; override with `AH_CORS_ORIGINS` in `/etc/default/ah` if needed, then `systemctl restart ah`.
6. Confirm `https://agentic.hosting/` (homepage), `/docs/`, `/changelog/`, `/install.sh`, and `https://www.agentic.hosting/` → 301 GitHub with a valid cert. From a browser on the dashboard, `GET https://api.agentic.hosting/v1/system/health` should include `Access-Control-Allow-Origin: https://agentic.hosting`.
