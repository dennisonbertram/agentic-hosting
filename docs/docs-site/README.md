# Documentation site

Astro static site in `/docs-site`. Visual language is copied from `website/index.html`: stone-50 background, Instrument Serif italics, Inter body, `ah` monogram, ★ Star on GitHub.

## Why a separate tree

The landing page is hand-written HTML + Tailwind CDN. Docs need markdown, a sidebar, sitemap, and a GitHub Pages workflow. Astro generates static HTML that can also be copied into the website nginx image at `/docs/`.

## Canonical source of truth

`internal/api/server.go` and the handler structs. README, homepage, skill, and slash commands were aligned to the same facts. The before/after list is `docs-site/CORRECTIONS.md`. Agent onboarding files: repo-root `llms.txt` / `llms-full.txt` and `skills/agentic-hosting/SKILL.md` (see `docs/docs-site/agent-onboard.md`). The 2026-10-19 fact-check (disk 500 vs 503, reset→redeploy, `circuit_open` boolean, idempotency POST/PUT only, quota defaults, nginx-unprivileged, master-key secrets) is in `docs/docs-site/factcheck-2026-10-19.md`.

## Commands

```bash
cd docs-site
npm ci
ASTRO_BASE=/ ASTRO_SITE=http://localhost:4321 npm run dev
npm run build          # base /docs/ for https://agentic.hosting/docs/
```

CI: `.github/workflows/docs.yml` builds docs with `ASTRO_BASE=/docs/` and assembles `website/` at the Pages root. Pages source must be GitHub Actions. Domain split: [`docs/website/github-pages.md`](../website/github-pages.md).

Website image (repo root context; `www` 301s to GitHub):

```bash
docker build -f website/Dockerfile -t website .
```

## Screenshots

Key pages captured after a local `npm run build` + static preview:

- `docs/docs-site/screenshots/docs-overview.png`
- `docs/docs-site/screenshots/docs-architecture.png`
- `docs/docs-site/screenshots/docs-api.png`
- `docs/docs-site/screenshots/docs-install.png`
- `docs/docs-site/screenshots/docs-security.png`
- `docs/docs-site/screenshots/docs-auth.png`
- `docs/docs-site/screenshots/docs-overview-mobile.png`

## SEO

Per-page title/description, canonical, Open Graph, Twitter card, JSON-LD `TechArticle`, `sitemap-index.xml`, `robots.txt`, `favicon.svg`, `og.png`.
