#!/usr/bin/env bash
# One-command setup for a Linux host (Ubuntu 24.04 primary).
#
# Fresh host:     delegates to scripts/bootstrap-stack.sh (Docker, gVisor,
#                 Traefik, registry, Nixpacks, Go, ah, keys, systemd units,
#                 health check), then installs the ahc CLI.
# Existing host:  idempotent upgrade/repair: rebuilds and reinstalls ah + ahc,
#                 refreshes changed systemd units, fixes key/config
#                 permissions, adds a missing AH_ADMIN_TOKEN, restarts only
#                 what changed (ah, ah-build-broker, ah-api-proxy), and runs
#                 the health check. Never regenerates
#                 the master key or bootstrap token.
#
# See docs/runbooks/setup-linux.md. Kept bash 3.2 compatible so --dry-run and
# --help can be exercised from a macOS checkout.
set -Eeuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
bootstrap=$repo_root/scripts/bootstrap-stack.sh
root=${AH_SETUP_ROOT:-}          # test hook: prefix for host paths; only honoured with --dry-run
os_release=${AH_SETUP_OS_RELEASE:-$root/etc/os-release}
api_port=8080

domain='' email='' external_tls=0 skip_website=0 dry_run=0 assume_yes=0 sandbox_hardening=0

usage() {
  cat <<'EOF'
Usage: sudo scripts/setup-linux.sh --domain DOMAIN [--acme-email EMAIL] [options]
       sudo scripts/setup-linux.sh [--yes]          (upgrade/repair an existing install)
       scripts/setup-linux.sh --dry-run [...]       (no root needed, changes nothing)

Fresh host (Ubuntu 24.04 amd64/arm64, no /var/lib/ah): runs
scripts/bootstrap-stack.sh, which installs Docker + gVisor (runsc), Traefik,
a loopback registry, Go and Nixpacks, builds and installs ah, generates the
master key, bootstrap token and admin token with 0600 permissions, installs
the systemd units and checks API health. This script then installs ahc.

Existing install (state in /var/lib/ah and /etc/default/ah): rebuilds and
reinstalls ah and ahc, refreshes changed systemd units, repairs key and
config permissions, adds AH_ADMIN_TOKEN if missing, restarts ah,
ah-build-broker and ah-api-proxy when their binary or unit changed, and
checks health. Keys and tokens are never replaced. Once AH_ADMIN_TOKEN is
set, the bootstrap token no longer authorizes recover, reactivate or quota.

Options:
  --domain DOMAIN             Parent domain (fresh installs; API at api.DOMAIN,
                              services at *.apps.DOMAIN)
  --acme-email EMAIL          Let's Encrypt contact (fresh installs, required
                              unless --external-tls-terminated)
  --external-tls-terminated   Port 80 is reachable only through a trusted TLS
                              tunnel (see bootstrap-stack.sh --help)
  --skip-website              Do not install the website container
  --sandbox-hardening         Opt in to hard sandbox disk limits: register the
                              Docker runtime runsc-sbx (runsc --overlay2=root:memory)
                              in /etc/docker/daemon.json, reload Docker, and set
                              AH_SANDBOX_RUNTIME=runsc-sbx and
                              AH_SANDBOX_WORKSPACE_TMPFS=true in /etc/default/ah.
                              Does not enable sandbox egress (see
                              docs/runbooks/sandboxes-hardening.md)
  --dry-run                   Check prerequisites and print the plan only
  --yes, -y                   Do not prompt for confirmation
  -h, --help                  Show this help
EOF
}

log() { printf 'setup-linux: %s\n' "$*"; }
warn() { printf 'setup-linux: WARNING: %s\n' "$*" >&2; }
die() { printf 'setup-linux: %s\n' "$*" >&2; exit 1; }
# run CMD...: execute, or only describe it under --dry-run.
run() {
  if ((dry_run)); then printf 'setup-linux: [dry-run] would run: %s\n' "$*"; return 0; fi
  "$@"
}

while (($#)); do
  case "$1" in
    --domain) (($# >= 2)) || die '--domain requires a value'; domain=$2; shift 2 ;;
    --acme-email) (($# >= 2)) || die '--acme-email requires a value'; email=$2; shift 2 ;;
    --external-tls-terminated) external_tls=1; shift ;;
    --skip-website) skip_website=1; shift ;;
    --sandbox-hardening) sandbox_hardening=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    --yes|-y) assume_yes=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done
if [[ -n $root ]] && ((dry_run == 0)); then die 'AH_SETUP_ROOT is a test hook and requires --dry-run'; fi
[[ -f $repo_root/go.mod && -f $repo_root/cmd/ah/main.go && -f $bootstrap ]] || die 'run from an agentic-hosting source checkout'

# ---------------------------------------------------------------- detection
os_id='' os_version=''
if [[ -r $os_release ]]; then
  os_id=$(sed -n 's/^ID=//p' "$os_release" | tr -d '"')
  os_version=$(sed -n 's/^VERSION_ID=//p' "$os_release" | tr -d '"')
fi
kernel=$(uname -s)

state_paths="/var/lib/ah /etc/default/ah /etc/systemd/system/ah.service"
present=0 total=0
for p in $state_paths; do
  total=$((total + 1))
  [[ -e $root$p ]] && present=$((present + 1))
done
if ((present == 0)); then mode=fresh
elif ((present == total)) && [[ -e $root/var/lib/ah/master.key ]]; then mode=existing
else mode=partial
fi

# Sandbox hardening (--sandbox-hardening): a second gVisor runtime whose
# root filesystem overlay lives in sandbox memory, so rootfs writes count
# against the sandbox's memory limit, plus a tmpfs /workspace of disk_mb.
sbx_runtime=runsc-sbx
sbx_runtime_args='["--overlay2=root:memory"]'
print_sandbox_hardening_plan() {
  echo "  $1. sandbox hardening: add runtime $sbx_runtime (runsc --overlay2=root:memory) to"
  echo '     /etc/docker/daemon.json (merge; backup kept; other keys untouched), reload Docker'
  echo '     (SIGHUP: running containers keep running), check docker info lists it, set'
  echo "     AH_SANDBOX_RUNTIME=$sbx_runtime and AH_SANDBOX_WORKSPACE_TMPFS=true in /etc/default/ah,"
  echo '     restart ah. Sandbox egress stays off (docs/runbooks/sandboxes-hardening.md)'
}

problems=0
check() { # check LABEL OK(0/1) [HINT]
  if [[ $2 == 1 ]]; then printf '  [ok]   %s\n' "$1"
  else printf '  [miss] %s%s\n' "$1" "${3:+ -- $3}"; problems=$((problems + 1)); fi
}
have() { command -v "$1" >/dev/null 2>&1 && echo 1 || echo 0; }
is_root() { [[ $(id -u) == 0 ]] && echo 1 || echo 0; }

log "detected: kernel=$kernel os=${os_id:-unknown} ${os_version:-?} mode=$mode"
echo 'Prerequisites:'
check 'Linux kernel' "$([[ $kernel == Linux ]] && echo 1 || echo 0)" 'gVisor needs Linux; on macOS use scripts/setup-macos.sh'
case "$mode" in
  fresh)
    check 'Ubuntu 24.04 (automated install target)' "$([[ $os_id == ubuntu && $os_version == 24.04 ]] && echo 1 || echo 0)" \
      'other Debian/Ubuntu releases: follow the manual steps in docs/runbooks/setup-linux.md'
    check 'running as root' "$(is_root)" 'use sudo'
    check 'python3 (installer subnet checks)' "$(have python3)" 'apt-get install python3'
    check 'curl' "$(have curl)" 'apt-get install curl'
    check 'git' "$(have git)" 'apt-get install git'
    valid_domain=0
    [[ $domain =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] && valid_domain=1
    check "--domain given (${domain:-none})" "$valid_domain" 'pass --domain example.com'
    if ((external_tls == 0)); then
      check "--acme-email given (${email:-none})" "$([[ $email == *@*.* ]] && echo 1 || echo 0)" 'pass --acme-email ops@example.com'
    fi
    ;;
  existing)
    check 'running as root' "$(is_root)" 'use sudo'
    check 'docker' "$(have docker)" 'Docker was expected from the original install'
    check 'systemctl' "$(have systemctl)"
    go_ok=0; { [[ -x $root/usr/local/go/bin/go ]] || command -v go >/dev/null 2>&1; } && go_ok=1
    check 'Go toolchain (/usr/local/go/bin/go or PATH)' "$go_ok"
    check 'curl' "$(have curl)"
    ((sandbox_hardening)) && check 'python3 (daemon.json merge for --sandbox-hardening)' "$(have python3)" 'apt-get install python3'
    [[ -z $domain ]] || warn '--domain is ignored for an existing install (edit /etc/default/ah and Traefik config instead)'
    ;;
  partial)
    echo '  [miss] consistent install state -- some of /var/lib/ah, /etc/default/ah,'
    echo '         /etc/systemd/system/ah.service, /var/lib/ah/master.key exist but not all.'
    problems=$((problems + 1))
    ;;
esac

# -------------------------------------------------------------------- plan
echo 'Plan:'
case "$mode" in
  fresh)
    flags="--domain ${domain:-DOMAIN}"
    if ((external_tls)); then flags="$flags --external-tls-terminated"; else flags="$flags --acme-email ${email:-EMAIL}"; fi
    ((skip_website)) && flags="$flags --skip-website"
    echo "  1. scripts/bootstrap-stack.sh $flags"
    echo '     (Docker 29 + runsc from signed apt repos, pinned Traefik/registry digests,'
    echo '      checksummed Go and Nixpacks, ah build/install, 0600 keys, systemd units, health check)'
    echo '  2. build cmd/ahc with /usr/local/go/bin/go and install /usr/local/bin/ahc'
    echo '  3. health check: GET http://127.0.0.1:8080/v1/system/health'
    ((sandbox_hardening)) && print_sandbox_hardening_plan 4
    ;;
  existing)
    echo '  1. verify Docker is active and has the runsc runtime; start stopped paas-traefik'
    echo '  2. repair permissions: /var/lib/ah 0750 ah:ah, master.key 0600 ah:ah, /etc/default/ah 0600 root'
    echo '  3. add AH_ADMIN_TOKEN to /etc/default/ah if it is missing (bootstrap token untouched;'
    echo '     once it is set, bootstrap tokens no longer authorize recover, reactivate or quota)'
    echo '  4. rebuild ah and ahc; install to /usr/local/bin only if they changed'
    echo '  5. reinstall changed systemd units from deploy/; daemon-reload'
    echo '  6. enable ah-api-proxy, ah-build-broker, ah; restart ah only if something changed,'
    echo '     ah-build-broker if the ah binary or its unit changed, ah-api-proxy if its unit changed'
    echo '  7. health check: GET http://127.0.0.1:8080/v1/system/health'
    ((sandbox_hardening)) && print_sandbox_hardening_plan 8
    ;;
  partial)
    echo '  none: refusing to touch a partially installed host.'
    echo '  See "Partial failure and recovery" in docs/ops/fresh-install.md.'
    ;;
esac

if ((dry_run)); then
  if [[ $mode == fresh && $problems == 0 && -n $root ]]; then
    # Test hook: a fake host root must not run the real installer preflight
    # against this machine (it checks Docker, daemon.json and ports).
    log 'skipping the installer preflight (AH_SETUP_ROOT test hook)'
  elif [[ $mode == fresh && $problems == 0 ]]; then
    log 'running the installer preflight (bootstrap-stack.sh --dry-run)'
    args=(--domain "$domain" --dry-run)
    if ((external_tls)); then args+=(--external-tls-terminated); else args+=(--acme-email "$email"); fi
    ((skip_website)) && args+=(--skip-website)
    "$bootstrap" "${args[@]}"
  fi
  if ((problems)); then log "dry-run: $problems prerequisite(s) missing; a real run would stop here"
  else log 'dry-run: no changes made'; fi
  exit 0
fi

((problems == 0)) || die "$problems prerequisite(s) missing; fix them and rerun (see docs/runbooks/setup-linux.md)"
if ((assume_yes == 0)); then
  [[ -t 0 ]] || die 'not a terminal; rerun with --yes to proceed without a prompt'
  printf 'Proceed? [y/N] '
  read -r reply
  [[ $reply == y || $reply == Y || $reply == yes ]] || die 'aborted'
fi

# ----------------------------------------------------------------- helpers
go_bin() { if [[ -x /usr/local/go/bin/go ]]; then echo /usr/local/go/bin/go; else command -v go; fi; }
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

build_cli() {
  log 'building ahc'
  (cd "$repo_root" && "$(go_bin)" build -trimpath -ldflags='-s -w' -o "$tmpdir/ahc" ./cmd/ahc)
}
# install_if_changed SRC DEST MODE: returns 0 when DEST was replaced and 1
# when it was already identical. Exits the script if install fails. Call it
# directly (e.g. in an if), never inside $(...): errexit does not apply in a
# command substitution, so a failed install could be mistaken for success.
install_if_changed() {
  if [[ -f $2 ]] && cmp -s "$1" "$2"; then return 1; fi
  install -o root -g root -m "$3" "$1" "$2" || die "failed to install $2"
  return 0
}
health_check() {
  local _
  for _ in $(seq 1 40); do
    if curl -fsS -m 3 -H 'X-Forwarded-Proto: https' "http://127.0.0.1:$api_port/v1/system/health" >/dev/null 2>&1; then
      log 'API health check passed'
      return 0
    fi
    sleep 1
  done
  journalctl -u ah.service -n 60 --no-pager >&2 || :
  die 'API health check failed (see journalctl -u ah)'
}

# set_default_env KEY VALUE: set KEY=VALUE in /etc/default/ah; returns 0 when
# the file changed.
set_default_env() {
  if grep -qx "$1=$2" /etc/default/ah; then return 1; fi
  sed -i "/^$1=/d" /etc/default/ah || die 'could not edit /etc/default/ah'
  printf '%s=%s\n' "$1" "$2" >> /etc/default/ah || die 'could not edit /etc/default/ah'
  log "set $1=$2 in /etc/default/ah"
  return 0
}

# apply_sandbox_hardening: register runsc-sbx with Docker and point ah at
# it. Returns 0 when /etc/default/ah changed (ah needs a restart). It runs as
# an if condition, where errexit is off, so each step that can fail ends in
# || die.
apply_sandbox_hardening() {
  local daemon=/etc/docker/daemon.json merged env_changed=1
  merged=$tmpdir/daemon.json
  python3 - "$daemon" "$merged" "$sbx_runtime" "$sbx_runtime_args" <<'PY' || die "could not merge $daemon"
import json, os, sys
src, dst, name, args = sys.argv[1], sys.argv[2], sys.argv[3], json.loads(sys.argv[4])
cfg = {}
if os.path.exists(src):
    with open(src) as f:
        cfg = json.load(f)
runtimes = cfg.setdefault("runtimes", {})
path = runtimes.get("runsc", {}).get("path", "/usr/bin/runsc")
runtimes[name] = {"path": path, "runtimeArgs": args}
with open(dst, "w") as f:
    json.dump(cfg, f, indent=2, sort_keys=True)
    f.write("\n")
PY
  if [[ -f $daemon ]] && python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])) == json.load(open(sys.argv[2])) else 1)' "$daemon" "$merged"; then
    log "daemon.json already registers $sbx_runtime"
  else
    if [[ -f $daemon ]]; then
      cp -p "$daemon" "$daemon.bak-$(date +%Y%m%d%H%M%S)" || die "could not back up $daemon"
      log "backed up $daemon"
    fi
    if command -v dockerd >/dev/null 2>&1; then
      dockerd --validate --config-file "$merged" >/dev/null || die "merged daemon.json failed dockerd --validate; $daemon unchanged"
    fi
    install -o root -g root -m 0644 "$merged" "$daemon" || die "failed to install $daemon"
    log "registered runtime $sbx_runtime in $daemon; reloading Docker (SIGHUP)"
    systemctl reload docker || die 'systemctl reload docker failed (see journalctl -u docker)'
  fi
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    docker info --format '{{json .Runtimes}}' | grep -q "\"$sbx_runtime\"" && break
    [[ $i == 10 ]] && die "Docker does not list runtime $sbx_runtime after reload (check journalctl -u docker)"
    sleep 1
  done
  if set_default_env AH_SANDBOX_RUNTIME "$sbx_runtime"; then env_changed=0; fi
  if set_default_env AH_SANDBOX_WORKSPACE_TMPFS true; then env_changed=0; fi
  return "$env_changed"
}

# ----------------------------------------------------------------- execute
if [[ $mode == fresh ]]; then
  args=(--domain "$domain")
  if ((external_tls)); then args+=(--external-tls-terminated); else args+=(--acme-email "$email"); fi
  ((skip_website)) && args+=(--skip-website)
  "$bootstrap" "${args[@]}"
  build_cli
  install_if_changed "$tmpdir/ahc" /usr/local/bin/ahc 0755 || :
  if ((sandbox_hardening)) && apply_sandbox_hardening; then
    log 'restarting ah (sandbox hardening settings changed)'
    systemctl restart ah.service
  fi
  health_check
else
  systemctl is-active --quiet docker || systemctl start docker
  docker info --format '{{json .Runtimes}}' | grep -q '"runsc"' ||
    die 'Docker has no runsc runtime; restore /etc/docker/daemon.json (see docs/runbooks/operations.md)'
  if docker container inspect paas-traefik >/dev/null 2>&1; then
    [[ $(docker container inspect --format '{{.State.Running}}' paas-traefik) == true ]] || docker start paas-traefik >/dev/null
  else
    warn 'paas-traefik container is missing; services will not be routable (see docs/runbooks/operations.md)'
  fi

  log 'repairing permissions'
  chown ah:ah /var/lib/ah /var/lib/ah/master.key
  chmod 0750 /var/lib/ah
  chmod 0600 /var/lib/ah/master.key
  chown root:root /etc/default/ah
  chmod 0600 /etc/default/ah

  changed=0 admin_token_added=0
  if ((sandbox_hardening)) && apply_sandbox_hardening; then changed=1; fi
  if ! grep -q '^AH_ADMIN_TOKEN=.\{32,\}' /etc/default/ah; then
    log 'adding a generated AH_ADMIN_TOKEN to /etc/default/ah'
    sed -i '/^AH_ADMIN_TOKEN=/d' /etc/default/ah
    printf 'AH_ADMIN_TOKEN=%s\n' "$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" >> /etc/default/ah
    changed=1 admin_token_added=1
    warn 'behaviour change: bootstrap tokens no longer authorize recover, reactivate or quota update'
    warn 'use AH_ADMIN_TOKEN from /etc/default/ah for those operations (ahc recover, reactivation, quota)'
  fi

  log 'building ah'
  (cd "$repo_root" && CGO_ENABLED=1 "$(go_bin)" build -trimpath -ldflags='-s -w' -o "$tmpdir/ah" ./cmd/ah)
  build_cli
  # ah-build-broker runs the same /usr/local/bin/ah binary, so a new binary
  # means restarting it too, or it keeps running the old (deleted) one next to
  # a new ah serve and new per-build ah build-worker processes.
  ah_bin_changed=0
  if install_if_changed "$tmpdir/ah" /usr/local/bin/ah 0755; then ah_bin_changed=1 changed=1; fi
  install_if_changed "$tmpdir/ahc" /usr/local/bin/ahc 0755 || :

  units_changed=0 broker_restart=$ah_bin_changed proxy_restart=0
  for unit in ah.service ah-api-proxy.service ah-build-broker.service ah-build@.service; do
    if install_if_changed "$repo_root/deploy/$unit" "/etc/systemd/system/$unit" 0644; then
      log "updated unit $unit"
      units_changed=1
      case "$unit" in
        ah-build-broker.service) broker_restart=1 ;;
        ah-api-proxy.service) proxy_restart=1 ;;
      esac
    fi
  done
  if ((units_changed)); then systemctl daemon-reload; changed=1; fi
  # enable --now does not restart a unit that is already running, so changed
  # binaries and unit settings (including hardening) need an explicit restart.
  systemctl enable --now ah-api-proxy.service ah-build-broker.service >/dev/null
  systemctl enable ah.service >/dev/null
  if ((proxy_restart)); then
    log 'restarting ah-api-proxy (unit changed)'
    systemctl restart ah-api-proxy.service
  fi
  if ((broker_restart)); then
    log 'restarting ah-build-broker (ah binary or unit changed)'
    systemctl restart ah-build-broker.service
  fi
  if ((changed)) || ! systemctl is-active --quiet ah; then
    log 'restarting ah'
    systemctl restart ah.service
  else
    log 'ah unchanged; not restarting'
  fi
  health_check
  if ((admin_token_added)); then
    warn 'AH_ADMIN_TOKEN was added: the bootstrap token no longer authorizes recover, reactivate or'
    warn 'quota update. Use AH_ADMIN_TOKEN from /etc/default/ah for those operations.'
  fi
fi

cat <<EOF

setup-linux: done.
Next steps:
  - Tokens are in root-only /etc/default/ah (AH_BOOTSTRAP_TOKEN registers tenants,
    AH_ADMIN_TOKEN authorizes recover/reactivate/quota). Read them through an
    approved secure channel; do not paste them into tickets or chat.
  - Point DNS (api, *.apps, www) at this host; see docs/ops/fresh-install.md.
  - Register a tenant:  ahc register <name> <email>   (with AH_URL set)
  - Operations (upgrade, backup/restore, key rotation, logs):
    docs/runbooks/operations.md
EOF
