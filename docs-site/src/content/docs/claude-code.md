---
title: Claude Code skill
description: Install the agentic-hosting Claude Code skill and the /ah-* slash commands shipped in this repository.
section: Reference
order: 14
---

# Claude Code skill and slash commands

**Share this instead of a runbook:** the standalone skill at [`skills/agentic-hosting/SKILL.md`](https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/skills/agentic-hosting/SKILL.md) plus [`llms.txt`](https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main/llms.txt). See [Give this to your agent](./for-agents/).

The repo also ships a Claude Code skill at `.claude/skills/agentic-hosting/SKILL.md` and commands under `.claude/commands/`. The homepage, `install.sh`, and `install-skill.sh` all install the six `/ah-*` commands.

## Install

```bash
curl -fsSL https://agentic.hosting/install-skill.sh | bash
```

That script copies:

- `~/.claude/skills/agentic-hosting/SKILL.md`
- references: `api-reference.md`, `operations.md`, `custom-domains.md`, `server-setup.md`
- Commands: `ah-deploy`, `ah-status`, `ah-db`, `ah-logs`, `ah-register`, `ah-snapshot`

Manual copy:

```bash
mkdir -p ~/.claude/skills/agentic-hosting
cp .claude/skills/agentic-hosting/SKILL.md ~/.claude/skills/agentic-hosting/
cp -r .claude/commands/*.md /your/project/.claude/commands/
```

Then set:

```bash
export AH_URL=https://your-host
export AH_KEY=keyid.secret
```

## Commands that exist in git

| Command | File | What it tells the agent to do |
|---------|------|--------------------------------|
| `/ah-deploy <git-or-image> <name> [port]` | `ah-deploy.md` | Health + disk check, create service or build, poll 10 minutes |
| `/ah-status` | `ah-status.md` | Detailed health, list services and databases, surface circuits |
| `/ah-db <service> <postgres\|redis> [name]` | `ah-db.md` | Create DB, fetch connection string, set env, restart |
| `/ah-logs <service> [build-id]` | `ah-logs.md` | Runtime logs, or build logs when a build-id is given |
| `/ah-register <name> <email>` | `ah-register.md` | `POST /v1/tenants/register` |
| `/ah-snapshot <service> [name]` | `ah-snapshot.md` | `POST /v1/services/{id}/snapshots` |

There is also `.claude/commands/security-review.md` — a four-pass code audit command, **not** an `ah` operator command.

## Scripts (no Claude required)

`scripts/` — `register.sh`, `deploy.sh`, `status.sh`, `logs.sh`, `db-provision.sh`. Same env vars (`AH_URL`, `AH_KEY`). See `scripts/README.md`.

## Skill vs this site

The skill, slash commands, homepage, and this site are aligned with `internal/api/server.go`. The audit of older wording is in [`docs-site/CORRECTIONS.md`](https://github.com/dennisonbertram/agentic-hosting/blob/main/docs-site/CORRECTIONS.md). Prefer the [REST API](./api/) page if anything still disagrees.
