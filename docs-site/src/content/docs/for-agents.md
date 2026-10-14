---
title: Give this to your agent
description: One shareable link so an AI agent can install, register, deploy, and operate agentic-hosting from llms.txt or the standalone skill.
section: Start
order: 2
---

# Give this to your agent

Paste one of these URLs into an agent chat. The agent can fetch the file and operate agentic-hosting without a dashboard.

## Index (`llms.txt`)

[llmstxt.org](https://llmstxt.org/) format: project summary plus links to every docs page.

```
https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/llms.txt
```

On this docs site (after GitHub Pages is enabled):

```
https://dennisonbertram.github.io/agentic-hosting/llms.txt
```

On the production website (baked into the nginx image):

```
https://agentic.hosting/llms.txt
https://agentic.hosting/docs/llms.txt
```

Full docs dump (every page + API reference, one markdown file):

```
https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/llms-full.txt
```

## Standalone skill

Agent Skills / Claude Code format (`name` + `description` frontmatter). Self-contained: install the server, register a tenant, deploy from git, add Postgres/Redis, read logs, and the live `/v1/` map.

```
https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/skills/agentic-hosting/SKILL.md
```

Copy into Claude Code:

```bash
mkdir -p ~/.claude/skills/agentic-hosting
curl -fsSL https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/skills/agentic-hosting/SKILL.md \
  -o ~/.claude/skills/agentic-hosting/SKILL.md
```

The in-repo Claude skill and `/ah-*` slash commands are the same API, split for Claude Code:

- `.claude/skills/agentic-hosting/SKILL.md`
- `.claude/commands/ah-deploy.md` (and `ah-status`, `ah-db`, `ah-logs`, `ah-register`, `ah-snapshot`)
- `curl -fsSL https://agentic.hosting/install-skill.sh | bash`

## What the agent will do

1. Install `ah` on Ubuntu (Docker, gVisor, Nixpacks, Traefik, systemd)
2. Read `AH_BOOTSTRAP_TOKEN` from `/etc/default/ah`
3. `POST /v1/tenants/register` → save `api_key` (shown once)
4. Deploy with `source_type` / `source_url` / `source_ref` (not `git_url` / `branch`)
5. Provision Postgres or Redis and wire `DATABASE_URL` / `REDIS_URL`
6. Stream `GET /v1/services/{id}/logs`

Source of truth: `internal/api/server.go`. Stale vs correct: [CORRECTIONS.md](https://github.com/dennisonbertram/agentic-hosting/blob/main/docs-site/CORRECTIONS.md).
