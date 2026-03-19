# Swarm: Custom Domain Support (Issue #14)

**Started**: 2026-03-19
**Team**: config-agent, migration-agent, services-agent, docker-agent, api-agent, test-agent, security-fix-agent
**Scope**: `internal/services/**,internal/docker/**,internal/api/services.go,internal/config/**,cmd/ah/main.go,internal/db/migrations/state_011*`
**Spec**: docs/implementation/custom-domains-spec-2026-03-19.md

---

## Wave 1 — Foundation: config + migration
**Teammates**: config-agent, migration-agent (parallel)
**Files changed**:
- `internal/config/config.go` — BaseDomain field + AH_BASE_DOMAIN env var
- `cmd/ah/main.go` — --base-domain CLI flag
- `internal/api/server.go` — BaseDomain in ServerConfig + Server struct
- `internal/db/migrations/state_011_service_dns_label.sql` — dns_label column + unique index
**Codex tasks**: none (small enough for Claude teammates)
**Review**: N/A (plumbing only)
**Commit**: f64f8dd

## Wave 2 — Core routing layer
**Teammates**: services-agent, docker-agent (parallel, no file conflicts)
**Files changed**:
- `internal/services/services.go` — baseDomain on Manager, isDNSLabelSafe, toDNSLabel, publicURL, traefikLabels, dns_label on Create/Get/List
- `internal/services/deploy_image.go` — pass traefikLabels() as extraLabels
- `internal/docker/client.go` — remove hardcoded Traefik labels, add traefik-public connect
- `internal/docker/client_test.go` — updated label assertions
**Codex tasks**: none
**Review**: N/A (see Ralph Loop)
**Commit**: d9a53af

## Wave 3 — API enforcement + tests
**Teammates**: api-agent, test-agent (parallel, no file conflicts)
**Files changed**:
- `internal/api/services.go` — ValidateDNSName call on service create
- `internal/services/services.go` — exported ValidateDNSName function
- `internal/services/services_test.go` — 5 new test functions (isDNSLabelSafe, toDNSLabel, publicURL, traefikLabels, CreateSetsDNSLabel)
**Commit**: ac5940e

---

## Ralph Loop

### Pass 1 (Adversarial — security researcher)
**Result**: NOT APPROVED
**CRITICAL: 3, HIGH: 2, MEDIUM: 3**

Critical findings:
1. Cross-tenant isolation breach: connecting tenant containers to shared traefik-public network
2. Spoofable Traefik identification via image name substring match
3. tenantID not validated before embedding in Traefik Host() rule

High findings:
4. Local registry cross-tenant image theft (pre-existing)
5. Tenant ID case normalization collision risk (theoretical — IDs are already hex)

### Security fixes (between Pass 1 and Pass 2)
**Commit**: 0f55d70
- CRITICAL 1: Removed traefik-public container-side connection from RunContainer()
- CRITICAL 2: findTraefikContainer() now matches only exact name "paas-traefik"
- CRITICAL 3: Added tenantIDRe + baseDomainRe validation guards in traefikLabels()
- All packages: go test ./... PASS

### Pass 2 (Skeptical user)
*In progress...*

### Pass 3 (Correctness auditor)
*Pending...*

## Final Status
- [ ] 3 consecutive clean passes
- [x] All tests passing
- [x] Build clean
- [ ] Committed and pushed
