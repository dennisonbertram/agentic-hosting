# Agent onboarding files (2026-10-14)

Shareable entry points so an agent can install and operate ah from one URL.

| File | Role |
|------|------|
| `llms.txt` | llmstxt.org index at repo root; copied to `docs-site/public/llms.txt` |
| `llms-full.txt` | Concatenation of docs-site pages + skill API reference (generated) |
| `skills/agentic-hosting/SKILL.md` | Standalone Agent Skills runbook |
| `.claude/skills/…` + `.claude/commands/ah-*.md` | Claude Code copy of the same API |

Regenerate dumps:

```bash
node docs-site/scripts/generate-llms.mjs --check
```

`npm run build` in `docs-site/` runs that first (`prebuild`). Website image also copies `llms.txt` and `llms-full.txt` to the nginx docroot so `https://agentic.hosting/llms.txt` works.
