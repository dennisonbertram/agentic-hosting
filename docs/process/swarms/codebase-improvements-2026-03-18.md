# Swarm: Codebase Improvements

**Started**: 2026-03-18
**Scope**: Full codebase — 16 improvement prompts
**Spec**: docs/implementation/improvement-prompts.md

## Wave Plan

### Wave 1 — Independent Foundations (4 agents, zero file overlap)
- **Agent A** (diskcheck-cleanup): Prompt 13 (dead code) — `internal/diskcheck/`
- **Agent B** (cache-refactor): Prompt 2 (heap cache) — `internal/cache/` (new), `middleware/auth.go`, `middleware/ratelimit.go`, `api/tenants.go`
- **Agent C** (config-extract): Prompt 9 (config) — `internal/config/` (new), `cmd/ah/main.go`, `gc/gc.go`, `builder/builder.go`
- **Agent D** (docs-versioning): Prompt 16 (API versioning docs) — `CLAUDE.md`

### Wave 2 — Error Handling Core (3 agents)
- **Agent A** (typed-errors): Prompts 1+14+12 (typed errors + wrapping + format) — `internal/apierr/` (new), `api/services.go`, `builds.go`, `databases.go`, managers
- **Agent B** (n1-queries): Prompt 6 (N+1 queries) — `api/tenants.go` (handleTenantUsage)
- **Agent C** (ratelimit-headers): Prompt 11 (rate limit headers) — `middleware/ratelimit.go`

### Wave 3 — Service Layer (1 agent, all touch same files)
- **Agent A** (service-improvements): Prompts 3+4+7+8+15 — deploy persistence, port validation, pagination, cleanup logging, env var validation

### Wave 4 — Test Coverage (1 agent)
- **Agent A** (test-coverage): Prompt 5 — tests for gc, diskcheck, builder, docker, services

### Wave 5 — Observability (1 agent)
- **Agent A** (structured-logging): Prompt 10 — slog migration across all files

---

## Execution Log

(entries appended after each wave)
