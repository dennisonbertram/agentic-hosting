# Skill: recover uses the admin token

## What changed

`skills/agentic-hosting/SKILL.md` told agents to recover lost keys with the bootstrap header. That 403s on any install that set `AH_ADMIN_TOKEN` (`requireAdmin` in `internal/api/tenants.go`).

The credentials block now exports `AH_ADMIN_TOKEN`. Recover and reactivate are documented as:

- `POST /v1/auth/recover` with `X-Admin-Token` + `{"email"}`, or `ahc recover --admin-token` / `$AH_ADMIN_TOKEN`
- bootstrap token accepted only when `AH_ADMIN_TOKEN` is unset (deprecated)
- `POST /v1/tenants/{tenantID}/reactivate` with the same admin token

The route table already listed `X-Admin-Token`; the register-section prose did not.

## Why

Agents that follow the standalone skill after a stock `setup-linux.sh` install hit 403 and have no next step. The README and `.claude` skill already used the admin token.

## Not changed

- `llms-full.txt` is a generated dump of the docs site; it already matches `auth.md`.
- `.claude/skills/agentic-hosting/SKILL.md` already told operators to use `X-Admin-Token`.
- Recovery code is unchanged.
