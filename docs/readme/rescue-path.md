# README rescue path — field check

The "Your PaaS is down? Deploy here in 5 minutes." section in the root README is a copy-paste path for someone who needs a live URL on their own box. Every command and field in that section was checked against the code on this branch. Nothing in that section names another hosting vendor.

## What the section claims, and where it is true

| Claim | Code |
|---|---|
| Fresh install is `sudo scripts/setup-linux.sh --domain … --acme-email …` (`-y` skips the prompt) | `scripts/setup-linux.sh` usage; delegates to `bootstrap-stack.sh` |
| Installer also installs `ahc` | `scripts/setup-linux.sh` header and end of fresh path |
| API is `https://api.$domain`; services are `*.apps.$domain` | setup-linux help: `AH_BASE_DOMAIN=apps.DOMAIN`; `publicURL` → `https://{dnsLabel}.{baseDomain}` |
| Tokens live in `/etc/default/ah` | `bootstrap-stack.sh` writes `AH_BOOTSTRAP_TOKEN` and `AH_ADMIN_TOKEN` there (root, 0600) |
| Register body is `name` + `email`; response is `tenant_id` + `api_key` | `RegisterRequest` / `RegisterResponse` in `internal/api/tenants.go` and `internal/ahclient/types.go`; `ahc register <name> <email> --save` |
| Header is `X-Bootstrap-Token` | `handleTenantRegister` |
| Git create uses `image: "pending"` | `runDeployGit` in `cmd/ahc/deploy.go`; `ValidateImage` accepts `pending` as a Docker Hub name |
| Omitted port is **8000** | `Create` in `internal/services/services.go`; `resolvePort`; `ahc deploy --port` defaults to `0` so Create fills 8000 |
| Build body is `source_type` / `source_url` / `source_ref` (default `main`) | `StartBuild` in `internal/builds/builds.go` |
| Build create returns **`build_id`**, not `id` | `handleBuildCreate` → `{"build_id","status","image"}` |
| `ahc deploy` prints `URL: …` when status is `running` | `pollUntilRunning` |
| `ahc db provision <service> postgres` sets `DATABASE_URL`, fetches the connection string, restarts | `cmd/ahc/db.go` (`REDIS_URL` for redis) |
| Connection string host is `ah-db-<databaseID>` on `5432` / `6379` | `buildConnectionString` + `DatabaseAlias` + `EnginePort` (issue #164) |
| Restart is required so the service joins `ah-dbnet-*` | CHANGELOG #164; skill and databases docs |

## Out of scope for the short section

- Manual `curl` (the `ahc` wrappers above call the same routes).
- Image deploys, snapshots, sandboxes, custom domains beyond `*.apps`.
- Reading the bootstrap token into the shell without putting it in chat or tickets.
