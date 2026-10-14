# Docs alignment (2026-10-14 follow-up)

Operator-facing files were rewritten to match `internal/` after the docs site shipped.

## What changed

- README, `website/index.html`, Claude skill + `/ah-*` commands, `website/install.sh`, `scripts/deploy.sh`
- Docs-site pages no longer warn that the skill is stale
- Audit table: [`docs-site/CORRECTIONS.md`](../../docs-site/CORRECTIONS.md)

## Learnings (wrong assumptions)

See [`docs/learnings.md`](../learnings.md). Two extra ones found this pass:

1. **Bootstrap validate is a JSON body, not a header.** `handleBootstrapTokenValidate` decodes `{"token"}` and always returns 200 with `valid`. Register / recover / reactivate still use `X-Bootstrap-Token`.
2. **`image` is required on `POST /v1/services`.** Omitting it is 400. The git path used by `ahc` sends `"pending"` as a Docker Hub-shaped placeholder; create still starts an async Deploy that will fail the pull until the build replaces the image.
