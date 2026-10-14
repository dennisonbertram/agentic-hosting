#!/usr/bin/env node
/**
 * Copies repo-root llms.txt into docs-site/public/ and builds llms-full.txt
 * from the docs-site markdown pages (plus the skill API reference when present).
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const docsSite = path.resolve(here, '..');
const repoRoot = path.resolve(docsSite, '..');
const hasRepo = fs.existsSync(path.join(repoRoot, 'go.mod'));
const publicDir = path.join(docsSite, 'public');
const docsDir = path.join(docsSite, 'src/content/docs');

const pages = [
  ['index.md', 'Overview'],
  ['for-agents.md', 'Give this to your agent'],
  ['why.md', 'Why it exists'],
  ['architecture.md', 'Architecture'],
  ['install.md', 'Install & quickstart'],
  ['auth.md', 'Registration & API keys'],
  ['deploy.md', 'Deploy from git'],
  ['databases.md', 'Postgres & Redis'],
  ['env.md', 'Env vars & secrets'],
  ['reconciliation.md', 'Reconciliation'],
  ['guards.md', 'Crash-loop & disk guards'],
  ['rate-limits.md', 'Rate limits'],
  ['backups.md', 'Backups'],
  ['api.md', 'REST API'],
  ['claude-code.md', 'Claude Code skill'],
  ['security.md', 'Security model'],
  ['troubleshooting.md', 'Troubleshooting'],
  ['roadmap.md', 'Roadmap'],
];

const requiredPhrases = [
  '# agentic-hosting',
  'source_type',
  'source_url',
  'source_ref',
  '/v1/system/bootstrap-token/validate',
  '/v1/tenants/{tenantID}/reactivate',
  '/v1/services',
  '/v1/databases',
  '/v1/services/{id}/logs',
  'AH_BOOTSTRAP_TOKEN',
  'postgres',
];

function stripFrontmatter(src) {
  if (!src.startsWith('---')) return src.trimStart();
  const end = src.indexOf('\n---', 3);
  if (end === -1) return src.trimStart();
  return src.slice(end + 4).replace(/^\s+/, '');
}

function assertContains(label, text, phrases) {
  const missing = phrases.filter((p) => !text.includes(p));
  if (missing.length) {
    throw new Error(`${label} missing required phrases: ${missing.join(', ')}`);
  }
}

function buildFull() {
  const parts = [
    '# agentic-hosting — full documentation\n',
    '> Generated from `docs-site/src/content/docs` for LLM context. Canonical routes live in `internal/api/server.go`.\n',
    'This file is the complete docs dump. Prefer `llms.txt` for navigation and `skills/agentic-hosting/SKILL.md` for an end-to-end operator runbook.\n',
  ];

  for (const [file, title] of pages) {
    const fp = path.join(docsDir, file);
    if (!fs.existsSync(fp)) {
      if (file === 'for-agents.md') continue;
      throw new Error(`missing docs page: ${file}`);
    }
    const body = stripFrontmatter(fs.readFileSync(fp, 'utf8'));
    parts.push(`\n\n# ${title}\n\n${body.trim()}\n`);
  }

  const apiRef = path.join(
    repoRoot,
    '.claude/skills/agentic-hosting/references/api-reference.md',
  );
  if (fs.existsSync(apiRef)) {
    parts.push(
      '\n\n# Skill API reference\n\n' +
        fs.readFileSync(apiRef, 'utf8').trim() +
        '\n',
    );
  }

  return parts.join('');
}

function writeIfChanged(dest, content) {
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  const prev = fs.existsSync(dest) ? fs.readFileSync(dest, 'utf8') : null;
  if (prev === content) return false;
  fs.writeFileSync(dest, content);
  return true;
}

const llmsSrc = hasRepo
  ? path.join(repoRoot, 'llms.txt')
  : path.join(publicDir, 'llms.txt');

if (!fs.existsSync(llmsSrc)) {
  throw new Error(`llms.txt not found at ${llmsSrc}`);
}

const llmsTxt = fs.readFileSync(llmsSrc, 'utf8');
assertContains('llms.txt', llmsTxt, requiredPhrases);

const full = buildFull();
assertContains('llms-full.txt', full, requiredPhrases);

writeIfChanged(path.join(publicDir, 'llms.txt'), llmsTxt);
writeIfChanged(path.join(publicDir, 'llms-full.txt'), full);
if (hasRepo) {
  writeIfChanged(path.join(repoRoot, 'llms-full.txt'), full);
}

if (process.argv.includes('--check')) {
  assertContains('public/llms.txt', fs.readFileSync(path.join(publicDir, 'llms.txt'), 'utf8'), requiredPhrases);
  assertContains(
    'public/llms-full.txt',
    fs.readFileSync(path.join(publicDir, 'llms-full.txt'), 'utf8'),
    requiredPhrases,
  );
}

console.log('llms.txt and llms-full.txt are up to date');
