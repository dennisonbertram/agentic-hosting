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
lacks "$tmp/out" 'running the installer preflight'

# With every prerequisite met (stubbed Linux, root, tools), the fake-root run
# must still skip the real bootstrap preflight so the test stays hermetic.
lstub=$tmp/linux-stub
mkdir -p "$lstub"
printf '#!/bin/sh\necho Linux\n' > "$lstub/uname"
printf '#!/bin/sh\necho 0\n' > "$lstub/id"
for tool in python3 curl git; do printf '#!/bin/sh\nexit 0\n' > "$lstub/$tool"; done
chmod +x "$lstub"/*
PATH="$lstub:/usr/bin:/bin" AH_SETUP_ROOT=$fake "$linux" --dry-run --domain example.com --acme-email ops@example.com > "$tmp/out" 2>&1
has "$tmp/out" 'skipping the installer preflight (AH_SETUP_ROOT test hook)'
lacks "$tmp/out" 'running the installer preflight'
has "$tmp/out" 'dry-run: no changes made'

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
has "$tmp/out" 'add AH_ADMIN_TOKEN to /etc/default/ah if it is missing (bootstrap token untouched'
has "$tmp/out" 'restart ah only if something changed'
has "$tmp/out" 'ah-build-broker if the ah binary or its unit changed'
has "$tmp/out" 'bootstrap tokens no longer authorize recover, reactivate or quota'
has "$tmp/out" 'dry-run'
lacks "$tmp/out" 'sandbox hardening'
AH_SETUP_ROOT=$fake "$linux" --dry-run --sandbox-hardening > "$tmp/out" 2>&1
has "$tmp/out" '8. sandbox hardening: add runtime runsc-sbx (runsc --overlay2=root:memory)'
has "$tmp/out" 'AH_SANDBOX_RUNTIME=runsc-sbx and AH_SANDBOX_WORKSPACE_TMPFS=true'
has "$tmp/out" 'Sandbox egress stays off'
has "$tmp/out" 'python3 (daemon.json merge for --sandbox-hardening)'
pass 'setup-linux.sh --dry-run (fresh, partial, existing, non-Ubuntu, --sandbox-hardening)'

# --- setup-linux.sh daemon.json merge (the embedded python) ----------------
if command -v python3 >/dev/null 2>&1; then
  awk "/<<'PY'/{f=1;next} /^PY\$/{f=0} f" "$linux" > "$tmp/merge.py"
  merge() { python3 -I "$tmp/merge.py" "$1" "$2" runsc-sbx '["--overlay2=root:memory"]'; }
  # runsc's own flags are kept; its --overlay2 is replaced; other keys stay.
  printf '%s\n' '{"log-level":"info","runtimes":{"runsc":{"path":"/usr/bin/runsc","runtimeArgs":["--platform=kvm","--overlay2=root:self"]}}}' > "$tmp/d1.json"
  merge "$tmp/d1.json" "$tmp/o1.json" 2> "$tmp/err"
  python3 -I -c 'import json,sys; c=json.load(open(sys.argv[1])); assert c["runtimes"]["runsc-sbx"]=={"path":"/usr/bin/runsc","runtimeArgs":["--platform=kvm","--overlay2=root:memory"]}, c; assert c["log-level"]=="info"; assert c["runtimes"]["runsc"]["runtimeArgs"]==["--platform=kvm","--overlay2=root:self"]' "$tmp/o1.json" ||
    fail 'daemon.json merge dropped runsc runtimeArgs or other keys'
  lacks "$tmp/err" 'WARNING'
  # Re-running on the merged file changes nothing and does not warn.
  merge "$tmp/o1.json" "$tmp/o1b.json" 2> "$tmp/err"
  cmp -s <(python3 -I -m json.tool --sort-keys "$tmp/o1.json") <(python3 -I -m json.tool --sort-keys "$tmp/o1b.json") || fail 'daemon.json merge is not idempotent'
  lacks "$tmp/err" 'WARNING'
  # A customised runsc-sbx entry is left alone, with a warning.
  printf '%s\n' '{"runtimes":{"runsc-sbx":{"path":"/opt/runsc","runtimeArgs":["--debug"]}}}' > "$tmp/d2.json"
  merge "$tmp/d2.json" "$tmp/o2.json" 2> "$tmp/err"
  python3 -I -c 'import json,sys; c=json.load(open(sys.argv[1])); assert c["runtimes"]["runsc-sbx"]=={"path":"/opt/runsc","runtimeArgs":["--debug"]}, c' "$tmp/o2.json" ||
    fail 'daemon.json merge overwrote a customised runsc-sbx entry'
  has "$tmp/err" 'WARNING: daemon.json already has a runsc-sbx runtime'
  pass 'setup-linux.sh daemon.json merge'
fi

# --- docs/ops/sandbox-egress-firewall.sh --dry-run -------------------------
fw=$repo_root/docs/ops/sandbox-egress-firewall.sh
"$fw" install --dry-run --host-ip 203.0.113.7 > "$tmp/out" 2>&1
has "$tmp/out" 'would run: iptables -w -A AH-SBX-EGRESS -d 169.254.0.0/16 -j DROP'
has "$tmp/out" 'would run: iptables -w -A AH-SBX-EGRESS -d 203.0.113.7/32 -j DROP'
has "$tmp/out" 'would run: iptables -w -I DOCKER-USER 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS'
has "$tmp/out" 'would run: iptables -w -I INPUT 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j DROP'
has "$tmp/out" 'dry-run: no changes made'
"$fw" remove --dry-run --bridge sbx0 > "$tmp/out" 2>&1
has "$tmp/out" 'would run: iptables -w -D INPUT -i sbx0'
if "$fw" install --dry-run --bridge 'bad name' > "$tmp/out" 2>&1; then fail 'firewall script accepted an invalid bridge'; fi
if "$fw" status --dry-run > "$tmp/out" 2>&1; then fail 'firewall script accepted status --dry-run'; fi
pass 'sandbox-egress-firewall.sh --dry-run'

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
# Hide the real toolchain so go/jq/python3 look missing; keep coreutils.
# A shadow of /usr/bin and /bin is used instead of those directories
# because CI runners (and some Macs) ship jq and python3 in /usr/bin.
shadow=$tmp/shadow
mkdir -p "$shadow"
for f in /bin/* /usr/bin/*; do
  name=${f##*/}
  case $name in go|jq|python3|python3.*) continue ;; esac
  [[ -e $shadow/$name ]] || ln -s "$f" "$shadow/$name"
done
PATH="$stub:$shadow" "$macos" --dry-run --engine colima > "$tmp/out" 2>&1
has "$tmp/out" 'would run: brew install'
has "$tmp/out" 'would run: colima start'
has "$tmp/out" 'would run: make -C'
has "$tmp/out" 'NOT gVisor'
if grep -Ev '^docker info' "$tmp/calls" | grep -q .; then cat "$tmp/calls" >&2; fail 'setup-macos.sh --dry-run invoked an installer or engine'; fi
rm -f "$stub/docker" "$stub/colima"
PATH="$stub:$shadow" "$macos" --dry-run --engine colima --skip-build > "$tmp/out" 2>&1
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

# --- sandbox.sh status (regression: exit 0 while running without a demo) --
sb=$tmp/sbs
mkdir -p "$sb/bin" "$tmp/curlstub"
printf '#!/bin/sh\nsleep 30\n' > "$sb/bin/ah"
printf '#!/bin/sh\nexit 0\n' > "$tmp/curlstub/curl"
chmod +x "$sb/bin/ah" "$tmp/curlstub/curl"
"$sb/bin/ah" & fake_ah=$!
printf '%s' "$fake_ah" > "$sb/ah.pid"
status_rc=0
AH_SANDBOX_DIR=$sb PATH="$tmp/curlstub:$PATH" "$sandbox" status > "$tmp/out" 2>&1 || status_rc=$?
kill "$fake_ah" 2>/dev/null || :
wait "$fake_ah" 2>/dev/null || :
has "$tmp/out" 'running: http://127.0.0.1:18480'
((status_rc == 0)) || { cat "$tmp/out" >&2; fail "sandbox.sh status exited $status_rc while running (no demo service)"; }
pass 'sandbox.sh status without a demo service'

printf 'test-setup-scripts: all checks passed\n'
