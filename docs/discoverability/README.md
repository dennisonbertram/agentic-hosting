# Discoverability

Work to make [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting) easier to find and star.

## What shipped

1. **MIT license** at the repository root (`LICENSE`), copyright 2026 Dennison Bertram, plus a README badge and License section. The marketing site already said MIT; the file was missing.
2. **GitHub About metadata** (topics, homepage). See [topics.md](topics.md) for what we could and could not set from this environment.
3. **README demo GIF** at `docs/assets/demo.gif`: the real local loop against `ah --dev` — register a tenant, deploy from git, tail logs, see the service running.

## Demo GIF provenance

The GIF is a recording of real commands, not a mocked transcript:

- Local `ah --dev` on `127.0.0.1:8080` with gVisor (`runsc`) verified.
- `ahc register` against that server with a bootstrap token.
- `ahc deploy https://github.com/hashicorp/http-echo` (Nixpacks git build).
- `ahc logs` and `ahc status` after the service reached `running`.

How it was captured: `asciinema rec` of `/tmp/ah-demo-record.sh`, then `agg` to GIF. A local Unix-socket build broker stood in for systemd (`ah-build@`) because this capture host is PID 1 `tini`, not systemd. That broker still ran the real `ah build-worker` / Nixpacks / Docker path.

## Layout

| Path | Role |
|------|------|
| `/LICENSE` | MIT license text |
| `docs/assets/demo.gif` | README hero recording |
| `docs/discoverability/topics.md` | GitHub topics / homepage attempt |
| `docs/sessions/2026-10-15.md` | Session log |
