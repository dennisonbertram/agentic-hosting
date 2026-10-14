#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly DEFAULT_VM_NAME=ah-macmini
readonly PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/lima-macmini.yaml"
readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vm_name=$DEFAULT_VM_NAME

usage() {
  cat <<'EOF'
Usage:
  scripts/bootstrap-macmini.sh create [--name VM]
  scripts/bootstrap-macmini.sh start [--name VM]
  scripts/bootstrap-macmini.sh stop [--name VM]
  scripts/bootstrap-macmini.sh status [--name VM]
  scripts/bootstrap-macmini.sh bootstrap --domain DOMAIN [--name VM] [--dry-run]

Creates an isolated native ARM64 Ubuntu VM with Lima on Apple Silicon macOS.
The only forwarded service is guest TCP/80 on host loopback port 18080.
EOF
}
die() { printf 'macmini: %s\n' "$*" >&2; exit 1; }
require_lima() { command -v limactl >/dev/null 2>&1 || die 'install Lima using its official instructions, then rerun this command'; }
check_host() {
  [[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || die 'requires an Apple Silicon Mac running macOS'
}
exists() {
  lima_instance_status "$vm_name" >/dev/null
}
instance_status_from_list() {
  awk -v name="$1" '$1 == name { print $2; found=1; exit } END { if (!found) exit 1 }'
}
lima_instance_status() {
  limactl list --format '{{.Name}} {{.Status}}' | instance_status_from_list "$1"
}
is_valid_domain() {
  local domain=$1 label
  local -a labels
  [[ ${#domain} -le 253 && $domain =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] || return 1
  IFS=. read -r -a labels <<< "$domain"
  for label in "${labels[@]}"; do ((${#label} <= 63)) || return 1; done
}
self_test() {
  is_valid_domain example.com || die 'valid-domain check failed'
  is_valid_domain 'bad domain' && die 'accepted an invalid domain'
  is_valid_domain '*.apps.example.com' && die 'accepted wildcard input where parent domain is required'
  is_valid_domain "$(printf 'a%.0s' {1..64}).example" && die 'accepted a domain label longer than 63 characters'
  [[ ah-macmini =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,39}$ ]] || die 'default VM-name check failed'
  [[ ../bad =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,39}$ ]] && die 'accepted an invalid VM name'
  [[ $(printf 'ah-other Running\nah-macmini Stopped\n' | instance_status_from_list ah-macmini) == Stopped ]] || die 'Lima instance-row parsing failed'
  if printf 'ah-other Running\n' | instance_status_from_list ah-macmini >/dev/null 2>&1; then die 'found a VM that is absent'; fi
  printf 'macmini: input and Lima instance-list self-test passed\n'
}
while (($#)); do
  case "$1" in
    --name) (($# >= 2)) || die '--name requires a value'; vm_name=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) break ;;
  esac
done
command_name=${1:-}
[[ -n $command_name ]] || { usage >&2; exit 2; }
shift
domain= dry_run=0
while (($#)); do
  case "$1" in
    --name) (($# >= 2)) || die '--name requires a value'; vm_name=$2; shift 2 ;;
    --domain) (($# >= 2)) || die '--domain requires a value'; domain=$2; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done
[[ $vm_name =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,39}$ ]] || die 'VM name must use 1–40 ASCII letters, digits, underscores, or hyphens'
if [[ $command_name == self-test ]]; then self_test; exit 0; fi
check_host
require_lima

case "$command_name" in
  create)
    [[ -f $PROFILE ]] || die "Lima profile is missing: $PROFILE"
    limactl validate "$PROFILE"
    if exists; then die "Lima VM '$vm_name' already exists; use start, or remove it yourself after reviewing its stored state"; fi
    ((dry_run)) && { printf 'Would create and start Lima VM %s from %s\n' "$vm_name" "$PROFILE"; exit 0; }
    limactl start --mount-none --name="$vm_name" "$PROFILE"
    ;;
  start)
    exists || die "Lima VM '$vm_name' does not exist; run create first"
    ((dry_run)) && { printf 'Would start Lima VM %s\n' "$vm_name"; exit 0; }
    limactl start "$vm_name"
    ;;
  stop)
    exists || die "Lima VM '$vm_name' does not exist"
    ((dry_run)) && { printf 'Would stop Lima VM %s\n' "$vm_name"; exit 0; }
    limactl stop "$vm_name"
    ;;
  status)
    exists || die "Lima VM '$vm_name' does not exist"
    printf '%s %s\n' "$vm_name" "$(lima_instance_status "$vm_name")"
    ;;
  bootstrap)
    is_valid_domain "$domain" || die 'provide the lowercase parent domain reserved with ngrok, without a wildcard prefix'
    exists || die "Lima VM '$vm_name' does not exist; run create first"
    [[ $(lima_instance_status "$vm_name") == Running ]] || die "Lima VM '$vm_name' is stopped; run start first"
    if ((dry_run)); then
      printf 'Would bootstrap VM %s for domain %s with external TLS termination and website container disabled\n' "$vm_name" "$domain"
      printf 'No VM command, network operation, or credential access was performed.\n'
      exit 0
    fi
    # Stage each immutable revision separately; never overwrite guest files before fresh-host guards run.
    source_commit=$(git -C "$REPO_ROOT" rev-parse --verify HEAD)
    [[ $source_commit =~ ^[0-9a-f]{40}$ ]] || die 'could not resolve a committed source revision'
    remote_source="/opt/agentic-hosting-$source_commit"
    if limactl shell "$vm_name" sudo test -e "$remote_source"; then
      die "source staging path $remote_source already exists; inspect it before retrying"
    fi
    limactl shell "$vm_name" sudo mkdir -p "$remote_source"
    git -C "$REPO_ROOT" archive --format=tar "$source_commit" | limactl shell "$vm_name" sudo tar -xf - -C "$remote_source"
    limactl shell "$vm_name" sudo "$remote_source/scripts/bootstrap-stack.sh" --domain "$domain" --external-tls-terminated --skip-website
    ;;
  *) usage >&2; die "unknown command: $command_name" ;;
esac
