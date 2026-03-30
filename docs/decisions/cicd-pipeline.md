# Decision: CI/CD Pipeline for agentic-hosting

## Status
Proposed — 2026-03-30

## Context
agentic-hosting deploys as a single Go binary (`ah`) to a Hetzner VPS (65.21.67.254). The binary requires CGO (go-sqlite3) and cannot be cross-compiled from macOS. Current deploy process is manual: rsync source → SSH → build on server → copy binary → restart systemd service. This works but is error-prone (orphan processes, pkill race conditions) and has no automated testing gate.

## Options Evaluated

### Option 1: GitHub Actions with SSH Deploy
GitHub-hosted runners execute `go test` (with CGO via apt packages), then SSH into the server to rsync, build, and restart.

- **Setup complexity**: Low — single workflow file, 3-4 GitHub secrets
- **Build time**: ~3-5 min (test on GitHub runner, build on server via SSH)
- **Secret management**: GitHub encrypted secrets (SSH key, host key, bootstrap token)
- **Rollback story**: Keep `ah.bak` on server; workflow can restore on failure
- **Cost**: Free (GitHub Actions free tier covers this)
- **CGO fit**: Tests run with CGO on Ubuntu runner; production build happens on server natively

### Option 2: GitHub Actions with Self-Hosted Runner
Install GitHub Actions runner directly on the Hetzner server.

- **Setup complexity**: Medium — runner installation, auto-update config, security hardening
- **Build time**: ~1-2 min (everything local, no network transfer)
- **Secret management**: Secrets stored in GitHub but runner has direct server access (security concern)
- **Rollback story**: Same as Option 1
- **Cost**: Free (self-hosted runners are free)
- **CGO fit**: Perfect — builds natively on the production server

### Option 3: Woodpecker CI (Self-Hosted)
Docker-native CI system, self-hosted on the server.

- **Setup complexity**: High — install Woodpecker server + agent, configure Docker-in-Docker or host Docker access, manage Woodpecker itself
- **Build time**: ~2-3 min (local builds, Docker overhead)
- **Secret management**: Woodpecker's own secret store
- **Rollback story**: Custom scripting required
- **Cost**: Free (open source) but operational overhead of running another service
- **CGO fit**: Requires CGO-capable Docker image or host mount

## Comparison Matrix

| Dimension | GitHub Actions + SSH | Self-Hosted Runner | Woodpecker CI |
|-----------|---------------------|-------------------|---------------|
| Setup effort | Low | Medium | High |
| Build speed | 3-5 min | 1-2 min | 2-3 min |
| Maintenance | None (GitHub manages) | Runner updates | Full service ops |
| Security | SSH key in GitHub | Runner has server access | Docker access concerns |
| Cost | Free | Free | Free + ops time |
| CGO support | Via apt on runner + native on server | Native | Needs config |
| Complexity | Simple | Medium | High |

## Recommendation

**Option 1: GitHub Actions with SSH Deploy.**

Rationale: This is a single-binary, single-server project. The simplest solution wins. GitHub Actions + SSH requires one workflow file and a few secrets. It separates the test environment (GitHub runner) from the deploy target (production server), which is good security practice. The self-hosted runner (Option 2) is faster but puts a GitHub-managed process on the production server. Woodpecker (Option 3) adds operational complexity that isn't justified for a single-server deploy.

## Implementation Sketch

1. Generate a deploy-only SSH key pair
2. Add the public key to server's `~/.ssh/authorized_keys`
3. Store in GitHub secrets: `DEPLOY_SSH_KEY`, `DEPLOY_HOST_KEY`, `AH_BOOTSTRAP_TOKEN`
4. Create `.github/workflows/deploy.yml`:
   - Trigger: push to `main`
   - Job 1: `test` — checkout, setup Go 1.25, `go test ./...`
   - Job 2: `deploy` (needs: test) — rsync via SSH, build on server, stop/copy/start
5. Use `systemctl stop ah.service && cp && systemctl start ah.service` (not pkill + restart)
6. Add post-deploy health check: `curl http://localhost:8080/v1/system/health`
