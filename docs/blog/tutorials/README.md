# agentic-hosting tutorials (blog drafts)

29 hands-on tutorials for publishing as blog posts. Each post has YAML front matter (`title`, `description`, `slug`, `tags`, `cover_diagram`, `sources`) and one diagram in `diagrams/` (hand-written SVG plus a 2400x1350 PNG render; posts reference the PNG).

## How these were checked

- Every command, endpoint, field and flag was checked against the code on `main` (commit 330ff4f). The commands were **not executed** against a live server; each post says so.
- Each post was checked for accuracy by Muse (`meta/muse-spark-1.3` via OpenRouter) against the files in its `sources` list. A Sonnet agent then verified each Muse finding in the code and fixed only the confirmed ones.
- A final pass edited for a plain, first-person voice and removed AI-slop phrasing. Posts contain no invented anecdotes, metrics or transcripts.

## Notes for the publishing agent

- Image paths are relative (`diagrams/NN-slug.png`). Upload the PNG and rewrite the path for the target platform.
- `sources` is for reviewers. Drop it from the published front matter if the platform rejects unknown keys.
- Posts 01-04 on dev.to already used covers `docs-site/public/blog/cover-0[1-4]*.jpg`; these tutorials are new topics.
- The API base used throughout is `https://api.agentic.hosting/v1/`. If the GitHub Pages cutover in `docs/website/github-pages.md` changes hostnames, update the posts before publishing.

## Posts

| # | Title | File |
|---|---|---|
| 01 | Install agentic-hosting on a fresh Ubuntu 24.04 server | [01-install-on-a-fresh-ubuntu-server.md](01-install-on-a-fresh-ubuntu-server.md) |
| 02 | Run agentic-hosting on a Mac with a Lima VM | [02-try-it-on-a-mac-with-lima.md](02-try-it-on-a-mac-with-lima.md) |
| 03 | Register a tenant and get your first API key | [03-register-a-tenant-and-get-your-first-api-key.md](03-register-a-tenant-and-get-your-first-api-key.md) |
| 04 | Deploy a Docker image and open it in your browser | [04-deploy-a-docker-image.md](04-deploy-a-docker-image.md) |
| 05 | Deploy a Go app from GitHub with one command | [05-deploy-a-go-app-from-git.md](05-deploy-a-go-app-from-git.md) |
| 06 | Deploy a Node.js app from git and keep it running | [06-deploy-a-node-app-from-git.md](06-deploy-a-node-app-from-git.md) |
| 07 | Build your own image, push it to the local registry, and add a HEALTHCHECK | [07-deploy-a-custom-image-with-a-healthcheck.md](07-deploy-a-custom-image-with-a-healthcheck.md) |
| 08 | Add a Postgres database to your service | [08-add-postgres-to-a-service.md](08-add-postgres-to-a-service.md) |
| 09 | Add Redis as a cache or job queue | [09-add-redis-to-a-service.md](09-add-redis-to-a-service.md) |
| 10 | Manage environment variables and secrets safely | [10-env-vars-and-secrets.md](10-env-vars-and-secrets.md) |
| 11 | Give your services real URLs with a base domain and wildcard DNS | [11-give-your-app-a-real-domain.md](11-give-your-app-a-real-domain.md) |
| 12 | Find out why a deploy failed: logs, builds and deployment history | [12-read-logs-and-deployment-history.md](12-read-logs-and-deployment-history.md) |
| 13 | Monitor your server from CI and cron with ahc status | [13-monitor-with-ahc-status.md](13-monitor-with-ahc-status.md) |
| 14 | What happens when your app crash-loops, and how to recover | [14-recover-a-crash-looping-service.md](14-recover-a-crash-looping-service.md) |
| 15 | Why docker stop is the wrong way to stop a service: the reconciler, garbage collector and disk guards | [15-reconciler-gc-and-disk-guards.md](15-reconciler-gc-and-disk-guards.md) |
| 16 | Snapshot a working service and fork it into a copy | [16-snapshot-and-fork-a-service.md](16-snapshot-and-fork-a-service.md) |
| 17 | Back up and restore the ah database and master key | [17-back-up-and-restore-ah.md](17-back-up-and-restore-ah.md) |
| 18 | Rate limits, quotas and usage: what an agent will hit and how to react | [18-rate-limits-quotas-and-usage.md](18-rate-limits-quotas-and-usage.md) |
| 19 | Make agent retries safe with idempotency keys | [19-idempotency-keys-for-agent-retries.md](19-idempotency-keys-for-agent-retries.md) |
| 20 | Give every agent its own API key (and revoke it in one call) | [20-one-api-key-per-agent.md](20-one-api-key-per-agent.md) |
| 21 | What gVisor and the container lockdown actually protect | [21-gvisor-and-container-lockdown.md](21-gvisor-and-container-lockdown.md) |
| 22 | How a request reaches your container: Traefik, file-provider routes and tenant networks | [22-how-a-request-reaches-your-container.md](22-how-a-request-reaches-your-container.md) |
| 23 | Deploy from Claude Code with the agentic-hosting skill and /ah-* commands | [23-claude-code-skill-and-slash-commands.md](23-claude-code-skill-and-slash-commands.md) |
| 24 | Give any agent one URL and let it install and operate agentic-hosting | [24-give-an-agent-one-url-and-let-it-deploy.md](24-give-an-agent-one-url-and-let-it-deploy.md) |
| 25 | Write an 80-line deploy loop that an agent can trust | [25-write-an-agent-deploy-loop.md](25-write-an-agent-deploy-loop.md) |
| 26 | A tour of the ahc CLI: configure, deploy, inspect, clean up | [26-tour-of-the-ahc-cli.md](26-tour-of-the-ahc-cli.md) |
| 27 | Watch your agents work in the supervisory dashboard | [27-use-the-dashboard.md](27-use-the-dashboard.md) |
| 28 | Give an agent a disposable sandbox to run code in | [28-disposable-sandboxes-for-agents.md](28-disposable-sandboxes-for-agents.md) |
| 29 | Fix the ten most common agentic-hosting errors | [29-fix-the-ten-most-common-errors.md](29-fix-the-ten-most-common-errors.md) |
