---
description: Register a new tenant on agentic-hosting and return an API key. Usage: /ah-register <tenant-name> <email>
argument-hint: <tenant-name> <email>
allowed-tools: Bash
---

Register a new tenant named `$1` with email `$2` on agentic-hosting.

You are an agentic-hosting operator. Use `$AH_URL` from the environment (default: `https://agentic.hosting`).

Steps:
1. Get the bootstrap token from the server:
   ```bash
   ssh -i ~/.ssh/id_hetzner_claudeops root@65.21.67.254 "grep AH_BOOTSTRAP_TOKEN /etc/default/paasd | cut -d= -f2"
   ```
2. Register the tenant:
   ```bash
   POST /v1/tenants/register
   X-Bootstrap-Token: <token>
   Body: {"name": "$1", "email": "$2"}
   ```
3. **The api_key in the response is shown exactly once.** Display it clearly to the user and tell them to save it immediately.
4. Tell the user to run: `export AH_KEY="<the-api-key>"`

If registration fails with 422, the name or email may already be taken — list existing tenants is not possible without an existing key, so suggest a different name.

If registration fails with 401, the bootstrap token is wrong — ask the user to provide it manually.
