---
description: Run a security review of the agentic-hosting codebase. Checks for auth bypass, injection, secrets exposure, container escape, and API security issues.
allowed-tools: Bash, Read, Glob, Grep, Agent
---

You are a security auditor specializing in Go backend services, container runtimes, and self-hosted PaaS infrastructure. Perform a thorough security review of the agentic-hosting codebase.

## Data handling

Do not send repository code to an external model or service (`llm`, OpenAI,
OpenRouter, DeepSeek, or similar). Review the code in this session, or with
Claude subagents that read the files directly. Do not flatten the repo with
repomix.

## Review Areas

Cover each area below. Split the areas across read-only Claude subagents when
the review is large, and verify each finding against the code before reporting it.

### Pass 1 — Authentication & Authorization
- Bootstrap token validation: is HMAC comparison timing-safe? Any bypass paths?
- API key validation: prefix-based lookup correct? Timing-safe comparison?
- Are there any endpoints missing auth middleware?
- Can a tenant access another tenant's resources?
- Is the master key used correctly for HMAC derivation?

### Pass 2 — Injection & Container Escape
- Are user-supplied strings ever passed to shell commands (exec.Command with user input)?
- Are Docker API calls using user input safely (image names, env vars, labels)?
- gVisor sandbox: is runsc actually enforced? Can a container escape to the host?
- Are there path traversal risks in any file operations?
- SQL injection in SQLite queries (raw string interpolation vs prepared statements)?

### Pass 3 — Secrets & Data Exposure
- Are API keys or tokens ever logged?
- Is the master key ever exposed in error messages or API responses?
- Are database connection strings (with credentials) returned to clients safely?
- Is sensitive data encrypted at rest in SQLite?
- Are there any hardcoded secrets or credentials?
- Is TLS enforced for external communication?

### Pass 4 — Denial of Service & Resource Limits
- Are there rate limits on any endpoints?
- Can a single tenant exhaust disk, CPU, or memory?
- Are there request body size limits?
- Can a malicious git URL or Docker image trigger unbounded resource usage during build?
- Are goroutines properly bounded/cancelled?
- Circuit breaker logic: can it be bypassed or exploited?

List every issue as CRITICAL, HIGH, MEDIUM, or LOW with file:line references.

## Output Format

After all passes, produce a consolidated report:

```
# agentic-hosting Security Review
Date: <today>

## Summary
- CRITICAL: N issues
- HIGH: N issues
- MEDIUM: N issues
- LOW: N issues

## Critical & High Issues
[List with file:line, description, and recommended fix]

## Medium Issues
[List with file:line and description]

## Low / Informational
[Brief list]

## Positive Findings
[What the codebase does well security-wise]
```

Save the report to `docs/investigations/security-review-<date>.md` in the repo.
