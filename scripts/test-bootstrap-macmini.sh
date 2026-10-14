#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wrapper="$repo_root/scripts/bootstrap-macmini.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
touch "$tmp/instances"
: > "$tmp/log"

cat > "$tmp/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  -s) printf 'Darwin\n' ;;
  -m) printf 'arm64\n' ;;
  *) exit 1 ;;
esac
EOF
cat > "$tmp/bin/limactl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$MACMINI_TEST_LOG"
case "$1" in
  validate) [[ -f $2 ]] ;;
  list) cat "$MACMINI_TEST_INSTANCES" ;;
  start|stop) : ;;
  *) printf 'unexpected fake limactl command: %s\n' "$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$tmp/bin/uname" "$tmp/bin/limactl"
export PATH="$tmp/bin:$PATH" MACMINI_TEST_LOG="$tmp/log" MACMINI_TEST_INSTANCES="$tmp/instances"

fail() { printf 'test-bootstrap-macmini: %s\n' "$*" >&2; exit 1; }
contains() { grep -Fqx -- "$2" "$1" || fail "missing expected line: $2"; }
not_contains() { if grep -Fqx -- "$2" "$1"; then fail "unexpected line: $2"; fi; }

"$wrapper" self-test

"$wrapper" create --dry-run > "$tmp/output"
contains "$tmp/log" "validate $repo_root/deploy/lima-macmini.yaml"
contains "$tmp/log" "list --format {{.Name}} {{.Status}}"
not_contains "$tmp/log" "start --mount-none --name=ah-macmini $repo_root/deploy/lima-macmini.yaml"
grep -Fq 'Would create and start Lima VM ah-macmini' "$tmp/output" || fail 'create dry-run did not describe its action'

: > "$tmp/log"
"$wrapper" create
contains "$tmp/log" "start --mount-none --name=ah-macmini $repo_root/deploy/lima-macmini.yaml"

printf 'ah-macmini Running\n' > "$tmp/instances"
: > "$tmp/log"
if "$wrapper" create > "$tmp/output" 2>&1; then fail 'create replaced an existing VM'; fi
not_contains "$tmp/log" "start --mount-none --name=ah-macmini $repo_root/deploy/lima-macmini.yaml"

: > "$tmp/log"
"$wrapper" status > "$tmp/output"
grep -Fqx 'ah-macmini Running' "$tmp/output" || fail 'status did not show the selected VM'

printf 'ah-macmini Stopped\n' > "$tmp/instances"
: > "$tmp/log"
"$wrapper" start --dry-run > "$tmp/output"
grep -Fq 'Would start Lima VM ah-macmini' "$tmp/output" || fail 'start dry-run did not describe its action'
not_contains "$tmp/log" 'start ah-macmini'

: > "$tmp/log"
"$wrapper" stop
contains "$tmp/log" 'stop ah-macmini'

if "$wrapper" bootstrap --domain '*.apps.example.com' --dry-run > "$tmp/output" 2>&1; then fail 'bootstrap accepted wildcard input instead of a parent domain'; fi
printf 'ah-macmini Running\n' > "$tmp/instances"
: > "$tmp/log"
"$wrapper" bootstrap --domain example.com --dry-run > "$tmp/output"
grep -Fq 'Would bootstrap VM ah-macmini for domain example.com' "$tmp/output" || fail 'bootstrap dry-run did not describe its action'
not_contains "$tmp/log" 'shell ah-macmini'

printf 'test-bootstrap-macmini: wrapper command checks passed\n'
