# GitHub topics and homepage

Requested repository About settings:

| Field | Target |
|-------|--------|
| Homepage | `https://agentic.hosting/` |
| Topics | `self-hosted`, `paas`, `ai-agents`, `gvisor`, `docker`, `claude-code`, `golang`, `heroku-alternative`, `devops`, `llm` |
| Description | Keep the existing one if present |

## What we found

The public description was already set:

> Agentic-first self-hosted PaaS — Docker + gVisor + Traefik + Nixpacks + SQLite, operated by AI agents

Homepage was empty. Topics were empty.

## What we could set

**Not from this agent.** Attempts:

1. `gh repo edit dennisonbertram/agentic-hosting --homepage … --add-topic …` returned **HTTP 403 Resource not accessible by integration**. `gh auth status` is the Cursor GitHub App (`cursor`), not the repo owner.
2. The owner-authenticated GitHub MCP (`github-pat`, login `dennisonbertram`, `admin: true` on this repo) has no tool to patch repository metadata (no `update_repository` / `replace_topics`).
3. `github-official` and `Github` MCP namespaces failed live tool discovery.

Setting topics and homepage needs the GitHub UI (Settings → General → About) or a PAT/app with the Administration permission:

```bash
gh api -X PATCH repos/dennisonbertram/agentic-hosting \
  -f homepage='https://agentic.hosting/' \
  -f description='Agentic-first self-hosted PaaS — Docker + gVisor + Traefik + Nixpacks + SQLite, operated by AI agents'

gh api -X PUT repos/dennisonbertram/agentic-hosting/topics \
  -f names[]=self-hosted -f names[]=paas -f names[]=ai-agents \
  -f names[]=gvisor -f names[]=docker -f names[]=claude-code \
  -f names[]=golang -f names[]=heroku-alternative -f names[]=devops -f names[]=llm
```
