#!/usr/bin/env bash
# Exercises setup-linux.sh, setup-macos.sh and sandbox.sh through --help and
# --dry-run only, with stubbed host commands. Never installs, never needs sudo,
# never talks to Docker. Run: make test-scripts
set -Eeuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { printf 'test-setup-scripts: FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'test-setup-scripts: ok: %s\n' "$*"; }
has() { grep -Fq -- "$2" "$1" || { cat "$1" >&2; fail "expected output to contain: $2"; }; }
lacks() { if grep -Fq -- "$2" "$1"; then cat "$1" >&2; fail "unexpected output: $2"; fi; }

linux=$repo_root/scripts/setup-linux.sh
macos=$repo_root/scripts/setup-macos.sh
sandbox=$repo_root/scripts/sandbox.sh

# --- --help and argument errors -------------------------------------------
for s in "$linux" "$macos"; do
  "$s" --help > "$tmp/out"
  has "$tmp/out" 'Usage:'
  if "$s" --bogus > "$tmp/out" 2>&1; then fail "$(basename "$s") accepted an unknown option"; fi
done
"$sandbox" --help > "$tmp/out"; has "$tmp/out" 'Usage: scripts/sandbox.sh'
if "$sandbox" > "$tmp/out" 2>&1; then fail 'sandbox.sh without a command succeeded'; fi
if "$sandbox" launch > "$tmp/out" 2>&1; then fail 'sandbox.sh accepted an unknown command'; fi
if AH_SANDBOX_API_PORT=80 "$sandbox" up --dry-run > "$tmp/out" 2>&1; then fail 'sandbox.sh accepted a privileged port'; fi
if AH_SANDBOX_RUNTIME=kata "$sandbox" up --dry-run > "$tmp/out" 2>&1; then fail 'sandbox.sh accepted an unknown runtime'; fi
pass '--help and argument validation'

# --- setup-linux.sh --dry-run against a fake host root ---------------------
fake=$tmp/root
mkdir -p "$fake/etc"
printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$fake/etc/os-release"

if AH_SETUP_ROOT=$fake "$linux" --domain example.com --acme-email ops@example.com > "$tmp/out" 2>&1; then
  fail 'AH_SETUP_ROOT was honoured without --dry-run'
fi
has "$tmp/out" 'requires --dry-run'

AH_SETUP_ROOT=$fake "$linux" --dry-run --domain example.com --acme-email ops@example.com > "$tmp/out" 2>&1
has "$tmp/out" 'mode=fresh'
has "$tmp/out" '[ok]   Ubuntu 24.04'
has "$tmp/out" 'scripts/bootstrap-stack.sh --domain example.com --acme-email ops@example.com'
has "$tmp/out" 'install /usr/local/bin/ahc'

AH_SETUP_ROOT=$fake "$linux" --dry-run --domain example.com --external-tls-terminated --skip-website > "$tmp/out" 2>&1
has "$tmp/out" '--external-tls-terminated --skip-website'
lacks "$tmp/out" '--acme-email given'

AH_SETUP_ROOT=$fake "$linux" --dry-run > "$tmp/out" 2>&1
has "$tmp/out" '[miss] --domain given (none)'

printf 'ID=debian\nVERSION_ID="12"\n' > "$fake/etc/os-release"
AH_SETUP_ROOT=$fake "$linux" --dry-run --domain example.com --acme-email ops@example.com > "$tmp/out" 2>&1
has "$tmp/out" '[miss] Ubuntu 24.04'
printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$fake/etc/os-release"

mkdir -p "$fake/var/lib/ah"
AH_SETUP_ROOT=$fake "$linux" --dry-run > "$tmp/out" 2>&1
has "$tmp/out" 'mode=partial'
has "$tmp/out" 'refusing to touch a partially installed host'

mkdir -p "$fake/etc/default" "$fake/etc/systemd/system"
: > "$fake/var/lib/ah/master.key"; : > "$fake/etc/default/ah"; : > "$fake/etc/systemd/system/ah.service"
AH_SETUP_ROOT=$fake "$linux" --dry-run > "$tmp/out" 2>&1
has "$tmp/out" 'mode=existing'
has "$tmp/out" 'add AH_ADMIN_TOKEN to /etc/default/ah if it is missing (bootstrap token untouched)'
has "$tmp/out" 'restart ah only if something changed'
has "$tmp/out" 'dry-run'
pass 'setup-linux.sh --dry-run (fresh, partial, existing, non-Ubuntu)'

# --- setup-macos.sh --dry-run with stubbed host tools ----------------------
stub=$tmp/stub
mkdir -p "$stub"
: > "$tmp/calls"
cat > "$stub/uname" <<'EOF'
#!/bin/sh
echo Darwin
EOF
cat > "$stub/xcode-select" <<'EOF'
#!/bin/sh
echo /Library/Developer/CommandLineTools
EOF
for cmd in brew docker colima make open; do
  cat > "$stub/$cmd" <<EOF
#!/bin/sh
echo "$cmd \$*" >> "$tmp/calls"
[ "$cmd" = docker ] && exit 1
exit 0
EOF
done
chmod +x "$stub"/*
# Hide the real toolchain so go/jq look missing; keep coreutils.
PATH="$stub:/usr/bin:/bin" "$macos" --dry-run --engine colima > "$tmp/out" 2>&1
has "$tmp/out" 'would run: brew install'
has "$tmp/out" 'would run: colima start'
has "$tmp/out" 'would run: make -C'
has "$tmp/out" 'NOT gVisor'
if grep -Ev '^docker info' "$tmp/calls" | grep -q .; then cat "$tmp/calls" >&2; fail 'setup-macos.sh --dry-run invoked an installer or engine'; fi
rm -f "$stub/docker" "$stub/colima"
PATH="$stub:/usr/bin:/bin" "$macos" --dry-run --engine colima --skip-build > "$tmp/out" 2>&1
has "$tmp/out" 'colima'
lacks "$tmp/out" 'would run: make'
pass 'setup-macos.sh --dry-run (stubbed brew/docker/colima)'

# --- sandbox.sh --dry-run -------------------------------------------------
AH_SANDBOX_DIR=$tmp/sb "$sandbox" up --dry-run > "$tmp/out" 2>&1
has "$tmp/out" 'AH_WARM_POOL_ENABLED=false, AH_GC_PRUNE_DOCKER=false'
has "$tmp/out" 'ah serve --dev --port 18480'
has "$tmp/out" 'deploy it as service'
AH_SANDBOX_DIR=$tmp/sb AH_SANDBOX_RUNTIME=runc AH_SANDBOX_SKIP_EXAMPLE=1 "$sandbox" up --dry-run > "$tmp/out" 2>&1
has "$tmp/out" 'AH_CONTAINER_RUNTIME=runc'
has "$tmp/out" 'skip the example deploy'
AH_SANDBOX_DIR=$tmp/sb "$sandbox" down --dry-run > "$tmp/out" 2>&1
has "$tmp/out" "would delete $tmp/sb"
[[ ! -e $tmp/sb ]] || fail 'sandbox.sh --dry-run created its state dir'
if AH_SANDBOX_DIR=relative "$sandbox" up --dry-run > "$tmp/out" 2>&1; then fail 'sandbox.sh accepted a relative state dir'; fi
pass 'sandbox.sh --dry-run'

printf 'test-setup-scripts: all checks passed\n'
