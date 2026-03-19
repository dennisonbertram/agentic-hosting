#!/usr/bin/env bash
set -euo pipefail

REPO="https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main"
SKILL_DIR="$HOME/.claude/skills/agentic-hosting"
REF_DIR="$SKILL_DIR/references"
CMD_DIR="$HOME/.claude/commands"

echo "Installing agentic-hosting Claude Code skill..."

mkdir -p "$SKILL_DIR" "$REF_DIR" "$CMD_DIR"

# Skill
curl -fsSL "$REPO/.claude/skills/agentic-hosting/SKILL.md" -o "$SKILL_DIR/SKILL.md"

# Reference files (loaded on demand — full API docs, operations guide, custom domains)
curl -fsSL "$REPO/.claude/skills/agentic-hosting/references/api-reference.md"   -o "$REF_DIR/api-reference.md"
curl -fsSL "$REPO/.claude/skills/agentic-hosting/references/operations.md"      -o "$REF_DIR/operations.md"
curl -fsSL "$REPO/.claude/skills/agentic-hosting/references/custom-domains.md"  -o "$REF_DIR/custom-domains.md"

# Slash commands
for cmd in ah-deploy ah-status ah-db ah-logs ah-register ah-snapshot; do
  curl -fsSL "$REPO/.claude/commands/$cmd.md" -o "$CMD_DIR/$cmd.md"
done

echo ""
echo "✓ Skill installed:    $SKILL_DIR"
echo "✓ References:         $REF_DIR (3 files)"
echo "✓ Commands installed: /ah-deploy /ah-status /ah-db /ah-logs /ah-register /ah-snapshot"
echo ""
echo "Set your server credentials in your shell profile:"
echo ""
echo "  export AH_URL=https://agentic.hosting"
echo "  export AH_KEY=your-keyid.secret"
echo ""
echo "Then open Claude Code and run:"
echo "  /ah-status          → check platform health"
echo "  /ah-deploy          → deploy a service"
echo "  /ah-register        → create a new tenant"
