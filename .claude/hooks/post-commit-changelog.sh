#!/usr/bin/env bash
# PostToolUse hook: after a `git commit` via the Bash tool,
# tell Claude to update CHANGELOG.md, website changelog, and skill docs.
#
# Loop prevention: exits silently when the commit message starts with
# docs(changelog) or docs(skill), or when the commit is --amend / merge.

set -euo pipefail

# ── Read hook payload from stdin ──────────────────────────────────────
INPUT=$(cat)

TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty')
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
EXIT_CODE=$(echo "$INPUT" | jq -r '.tool_response.exitCode // empty')

# ── Fast exits ────────────────────────────────────────────────────────

# Only act on Bash tool
[ -z "$TOOL_NAME" ] && exit 0
[ "$TOOL_NAME" != "Bash" ] && exit 0

# Only act on git commit commands
echo "$COMMAND" | grep -q 'git commit' || exit 0

# Skip if the commit failed
[ "$EXIT_CODE" != "0" ] && exit 0

# Skip --amend commits
echo "$COMMAND" | grep -q '\-\-amend' && exit 0

# ── Get last commit info ─────────────────────────────────────────────
COMMIT_HASH=$(git rev-parse --short HEAD 2>/dev/null) || exit 0
COMMIT_MSG=$(git log -1 --pretty=format:'%s' 2>/dev/null) || exit 0

# Skip docs(changelog) and docs(skill) commits (loop prevention)
echo "$COMMIT_MSG" | grep -qE '^docs\((changelog|skill)\)' && exit 0

# Skip merge commits
PARENT_COUNT=$(git cat-file -p HEAD 2>/dev/null | grep -c '^parent' || true)
[ "$PARENT_COUNT" -gt 1 ] && exit 0

# ── Gather commit details ────────────────────────────────────────────
FILES_CHANGED=$(git diff-tree --no-commit-id --name-only -r HEAD 2>/dev/null | head -30)
DIFF_STAT=$(git diff-tree --no-commit-id --stat -r HEAD 2>/dev/null | tail -1)

# ── Map conventional commit type to Keep a Changelog section ─────────
CHANGELOG_SECTION="Changed"
if echo "$COMMIT_MSG" | grep -qE '^feat(\(|:)'; then
    CHANGELOG_SECTION="Added"
elif echo "$COMMIT_MSG" | grep -qE '^fix(\(|:)'; then
    CHANGELOG_SECTION="Fixed"
elif echo "$COMMIT_MSG" | grep -qE '^refactor(\(|:)'; then
    CHANGELOG_SECTION="Changed"
elif echo "$COMMIT_MSG" | grep -qE '^(docs|chore|test|ci)(\(|:)'; then
    CHANGELOG_SECTION="Changed"
fi

# ── Check if API/feature files were touched ──────────────────────────
SKILL_UPDATE_NEEDED="false"
if echo "$FILES_CHANGED" | grep -qE '(internal/api/|cmd/ah/|internal/reconciler/)'; then
    SKILL_UPDATE_NEEDED="true"
fi

# ── Build additionalContext ──────────────────────────────────────────
SKILL_INSTRUCTION=""
if [ "$SKILL_UPDATE_NEEDED" = "true" ]; then
    SKILL_INSTRUCTION="3. The commit touches API/feature code. Review .claude/skills/agentic-hosting/SKILL.md and .claude/skills/agentic-hosting/references/api-reference.md — update them if new endpoints, features, or behaviors were added or changed.
4. "
else
    SKILL_INSTRUCTION="3. "
fi

jq -n --arg hash "$COMMIT_HASH" \
      --arg msg "$COMMIT_MSG" \
      --arg files "$FILES_CHANGED" \
      --arg stat "$DIFF_STAT" \
      --arg section "$CHANGELOG_SECTION" \
      --arg skill_instr "$SKILL_INSTRUCTION" \
      --arg skill_needed "$SKILL_UPDATE_NEEDED" \
'{
  "additionalContext": (
    "A commit was just made. Please update the changelog and docs:\n\n" +
    "Commit: " + $hash + " — " + $msg + "\n" +
    "Files changed:\n" + $files + "\n" +
    "Stats: " + $stat + "\n\n" +
    "Instructions:\n" +
    "1. Add a user-facing entry to CHANGELOG.md under [Unreleased] → " + $section + ". Write it from the users perspective — what changed for them, not implementation details.\n" +
    "2. Add a matching HTML entry to website/changelog/index.html under the Unreleased section (if one exists) or the latest version section.\n" +
    $skill_instr +
    "Commit all changelog/skill updates together with message: docs(changelog): update for " + $hash + "\n\n" +
    "IMPORTANT: Use the exact docs(changelog) or docs(skill) prefix so this hook does not trigger again."
  )
}'
