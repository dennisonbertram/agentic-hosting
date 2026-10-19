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

`npm run build` in `docs-site/` runs that first (`prebuild`). GitHub Pages copies `llms.txt` and `llms-full.txt` to the apex (`https://agentic.hosting/llms.txt`) and the docs build also serves them at `https://agentic.hosting/docs/llms.txt`. The Hetzner website image still copies them for leftover IP/apex traffic before DNS cutover.
