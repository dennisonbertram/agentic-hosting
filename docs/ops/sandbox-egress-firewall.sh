#!/usr/bin/env bash
# Host firewall rules for the sandbox egress bridge (AH_SANDBOX_EGRESS_NETWORK).
#
# Sandboxes created with egress "internet" attach only to one shared Docker
# bridge whose interface has the same name as the network (default
# ah-sbx-egress). Docker lets that bridge reach anything the host can route
# to. These rules limit it to the public internet:
#
#   DOCKER-USER  -i BRIDGE -j AH-SBX-EGRESS        (position 1)
#   AH-SBX-EGRESS:
#     ESTABLISHED,RELATED            -> RETURN (replies keep flowing)
#     10/8 172.16/12 192.168/16      -> DROP   (RFC 1918: LAN, other Docker networks)
#     100.64/10                      -> DROP   (carrier-grade NAT)
#     169.254/16                     -> DROP   (link-local, cloud metadata 169.254.169.254)
#     127/8, 0/8                     -> DROP   (loopback, "this network")
#     224/4, 240/4                   -> DROP   (multicast, reserved and broadcast)
#     each host public IPv4 /32      -> DROP
#     anything else                  -> RETURN (Docker's own rules allow it out)
#   INPUT        -i BRIDGE -j DROP                 (position 1: the ah API, sshd,
#                                                   anything bound on the host)
#   ip6tables FORWARD/INPUT -i BRIDGE -j DROP      (IPv6 is disabled on the bridge;
#                                                   these are defence in depth)
#
# Every rule carries the comment "ah-sbx-egress" so remove/status find them.
# install is idempotent: it rebuilds the chain and moves the two jump/drop
# rules back to position 1. While it does, temporary "ah-sbx-egress-guard"
# drops at position 1 cut the bridge off entirely, so running sandboxes never
# see a partial ruleset; a failed install leaves them in place (fail closed).
# Run it again after anything that reorders INPUT or DOCKER-USER
# (ufw/firewalld reloads, fail2ban restarts).
#
# The rules only work while Docker manages iptables with its iptables
# firewall backend (FORWARD -j DOCKER-USER); ah checks that too.
#
# ah verifies these rules at startup (from `snapshot` output, see
# docs/runbooks/sandboxes-hardening.md) and keeps egress disabled if they
# are missing. Kept bash 3.2 compatible so --dry-run runs on macOS.
set -Eeuo pipefail

chain=AH-SBX-EGRESS
comment=ah-sbx-egress
guard_comment=ah-sbx-egress-guard
bridge=${AH_SANDBOX_EGRESS_NETWORK:-ah-sbx-egress}
dry_run=0 persist=1 detect_host_ips=1
host_ips=()
cmd=''
snapshot_file=''

drop_cidrs='10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 127.0.0.0/8 0.0.0.0/8 224.0.0.0/4 240.0.0.0/4'

usage() {
  cat <<'EOF'
Usage: sudo docs/ops/sandbox-egress-firewall.sh install  [--bridge NAME] [--host-ip IP]... [--no-persist] [--dry-run]
       sudo docs/ops/sandbox-egress-firewall.sh remove   [--bridge NAME] [--no-persist] [--dry-run]
       sudo docs/ops/sandbox-egress-firewall.sh status   [--bridge NAME]
       sudo docs/ops/sandbox-egress-firewall.sh snapshot FILE [--bridge NAME]

Installs (or removes) the iptables rules that keep sandboxes on the egress
bridge off private networks, cloud metadata and this host.

Commands:
  install          Create/refresh chain AH-SBX-EGRESS, put the DOCKER-USER jump
                   and the INPUT drop at position 1, add ip6tables drops, and
                   save with netfilter-persistent when it is installed
  remove           Delete every rule tagged ah-sbx-egress and the chain
  status           Print the rules; exit 0 if complete, 1 if anything is missing
  snapshot FILE    Write the iptables -S output ah checks at startup to FILE
                   (atomically, mode 0644). Run as root before ah starts.

Options:
  --bridge NAME    Bridge interface (= AH_SANDBOX_EGRESS_NETWORK; default
                   ah-sbx-egress, or $AH_SANDBOX_EGRESS_NETWORK)
  --host-ip IP     Also drop traffic to this host IPv4 address (repeatable).
                   Without it, install drops every global-scope IPv4 address
                   from `ip -4 addr` (the INPUT drop covers them as well)
  --no-persist     Do not run netfilter-persistent save
  --dry-run        Print the commands only; needs no root and changes nothing
  -h, --help       Show this help
EOF
}

log() { printf 'sandbox-egress-firewall: %s\n' "$*"; }
die() { printf 'sandbox-egress-firewall: %s\n' "$*" >&2; exit 1; }

# run CMD...: execute, or only print it under --dry-run.
run() {
  if ((dry_run)); then printf 'would run: %s\n' "$*"; return 0; fi
  "$@"
}

while (($#)); do
  case "$1" in
    install|remove|status) [[ -z $cmd ]] || die "only one command (got $cmd and $1)"; cmd=$1; shift ;;
    snapshot)
      [[ -z $cmd ]] || die "only one command (got $cmd and $1)"
      (($# >= 2)) || die 'snapshot requires a FILE'
      cmd=snapshot snapshot_file=$2; shift 2 ;;
    --bridge) (($# >= 2)) || die '--bridge requires a value'; bridge=$2; shift 2 ;;
    --host-ip) (($# >= 2)) || die '--host-ip requires a value'; host_ips+=("$2"); detect_host_ips=0; shift 2 ;;
    --no-persist) persist=0; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done
[[ -n $cmd ]] || { usage >&2; exit 2; }

# The bridge name is an interface name: at most 15 characters, no spaces.
[[ $bridge =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$ ]] || die "invalid bridge name: $bridge (1-15 letters, digits, _ . -)"
if ((${#host_ips[@]})); then
  for ip in "${host_ips[@]}"; do
    [[ $ip =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "invalid --host-ip: $ip (IPv4 address)"
  done
fi
if [[ $cmd == status || $cmd == snapshot ]] && ((dry_run)); then die "--dry-run applies to install and remove only"; fi

if ((dry_run == 0)); then
  [[ $(id -u) == 0 ]] || die 'must run as root (or use --dry-run)'
  command -v iptables >/dev/null 2>&1 || die 'iptables not found'
fi
have_ip6=0
if ((dry_run)) || command -v ip6tables >/dev/null 2>&1; then have_ip6=1; fi

ipt() { run iptables -w "$@"; }
ip6t() { run ip6tables -w "$@"; }

# delete_tagged BIN CHAIN RULE...: remove every copy of RULE from CHAIN.
delete_tagged() {
  local bin=$1 ch=$2; shift 2
  if ((dry_run)); then printf 'would run: %s -w -D %s %s   (repeated while present)\n' "$bin" "$ch" "$*"; return 0; fi
  while "$bin" -w -C "$ch" "$@" >/dev/null 2>&1; do "$bin" -w -D "$ch" "$@"; done
}

detect_ips() {
  if ((detect_host_ips == 0)); then return 0; fi
  if ! command -v ip >/dev/null 2>&1; then
    log 'note: the ip command was not found; host IPs not detected (the INPUT drop still blocks traffic to this host)'
    return 0
  fi
  local addr
  for addr in $(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1); do
    host_ips+=("$addr")
  done
}

persist_rules() {
  ((persist)) || return 0
  if ((dry_run)); then
    printf 'would run: netfilter-persistent save   (only if netfilter-persistent is installed)\n'
    return 0
  fi
  if command -v netfilter-persistent >/dev/null 2>&1; then
    netfilter-persistent save >/dev/null
    log 'saved with netfilter-persistent'
  else
    log 'note: netfilter-persistent not installed; rules are lost on reboot unless install runs at boot (see docs/runbooks/sandboxes-hardening.md)'
  fi
}

# ensure_chain NAME: create iptables chain NAME if it does not exist.
ensure_chain() {
  if ((dry_run)); then
    printf 'would run: iptables -w -N %s   (if missing)\n' "$1"
  elif ! iptables -w -S "$1" >/dev/null 2>&1; then
    iptables -w -N "$1"
  fi
}

install_rules() {
  detect_ips
  # DOCKER-USER exists once Docker has started; create it so install can run
  # first (ah still checks that FORWARD jumps to it).
  ensure_chain DOCKER-USER
  ensure_chain "$chain"

  # Fail closed while rebuilding: running egress sandboxes must never see a
  # flushed or half-built chain, or a moment with no INPUT drop. Guard drops
  # go to position 1 first and are removed only after everything else is in
  # place. If a step fails (set -e), the guards stay and the bridge stays cut
  # off (no egress at all) until install succeeds.
  trap 'log "install FAILED: the bridge stays blocked by the $guard_comment rules until install succeeds"' ERR
  ipt -I DOCKER-USER 1 -i "$bridge" -m comment --comment "$guard_comment" -j DROP
  ipt -I INPUT 1 -i "$bridge" -m comment --comment "$guard_comment" -j DROP
  local ch
  if ((have_ip6)); then
    for ch in FORWARD INPUT; do ip6t -I "$ch" 1 -i "$bridge" -m comment --comment "$guard_comment" -j DROP; done
  fi

  # Chain: rebuild from scratch (behind the guard).
  ipt -F "$chain"
  ipt -A "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
  local cidr
  for cidr in $drop_cidrs; do ipt -A "$chain" -d "$cidr" -j DROP; done
  if ((${#host_ips[@]})); then
    for ip in "${host_ips[@]}"; do ipt -A "$chain" -d "$ip/32" -j DROP; done
  fi
  ipt -A "$chain" -j RETURN

  delete_tagged iptables DOCKER-USER -i "$bridge" -m comment --comment "$comment" -j "$chain"
  ipt -I DOCKER-USER 1 -i "$bridge" -m comment --comment "$comment" -j "$chain"

  delete_tagged iptables INPUT -i "$bridge" -m comment --comment "$comment" -j DROP
  ipt -I INPUT 1 -i "$bridge" -m comment --comment "$comment" -j DROP

  if ((have_ip6)); then
    for ch in FORWARD INPUT; do
      delete_tagged ip6tables "$ch" -i "$bridge" -m comment --comment "$comment" -j DROP
      ip6t -I "$ch" 1 -i "$bridge" -m comment --comment "$comment" -j DROP
    done
  else
    log 'note: ip6tables not found; skipping IPv6 drops (IPv6 is disabled on the egress bridge)'
  fi

  # Everything is in place: lift the guards.
  delete_guards
  trap - ERR
  persist_rules
  if ((dry_run)); then log "dry-run: no changes made (bridge $bridge)"; else log "installed for bridge $bridge"; fi
}

# delete_guards: remove install's temporary guard drops (also leftovers of
# a failed install).
delete_guards() {
  delete_tagged iptables DOCKER-USER -i "$bridge" -m comment --comment "$guard_comment" -j DROP
  delete_tagged iptables INPUT -i "$bridge" -m comment --comment "$guard_comment" -j DROP
  if ((have_ip6)); then
    local ch
    for ch in FORWARD INPUT; do
      delete_tagged ip6tables "$ch" -i "$bridge" -m comment --comment "$guard_comment" -j DROP
    done
  fi
}

remove_rules() {
  delete_guards
  delete_tagged iptables DOCKER-USER -i "$bridge" -m comment --comment "$comment" -j "$chain"
  delete_tagged iptables INPUT -i "$bridge" -m comment --comment "$comment" -j DROP
  if ((have_ip6)); then
    delete_tagged ip6tables FORWARD -i "$bridge" -m comment --comment "$comment" -j DROP
    delete_tagged ip6tables INPUT -i "$bridge" -m comment --comment "$comment" -j DROP
  fi
  if ((dry_run)); then
    printf 'would run: iptables -w -F %s; iptables -w -X %s   (if present)\n' "$chain" "$chain"
  elif iptables -w -S "$chain" >/dev/null 2>&1; then
    iptables -w -F "$chain"
    iptables -w -X "$chain"
  fi
  persist_rules
  if ((dry_run)); then log "dry-run: no changes made (bridge $bridge)"; else log "removed rules for bridge $bridge"; fi
}

# first_rule BIN CHAIN: the first "-A CHAIN" line of BIN -S CHAIN, or "".
first_rule() { "$1" -w -S "$2" 2>/dev/null | grep -m1 -- "^-A $2 " || :; }

status_rules() {
  local ok=1 rules cidr first
  first=$(first_rule iptables FORWARD)
  if [[ $first == "-A FORWARD -j DOCKER-USER" ]]; then log "ok: FORWARD jumps to DOCKER-USER first"
  else log "MISSING: FORWARD rule 1 is not '-j DOCKER-USER' (got: ${first:-none}); Docker must manage iptables (iptables firewall backend)"; ok=0; fi
  first=$(first_rule iptables DOCKER-USER)
  if [[ $first == *"-i $bridge "*"-j $chain"* ]]; then log "ok: DOCKER-USER jumps to $chain first"
  else log "MISSING: DOCKER-USER rule 1 is not '-i $bridge -j $chain' (got: ${first:-none})"; ok=0; fi
  if rules=$(iptables -w -S "$chain" 2>/dev/null); then
    for cidr in $drop_cidrs; do
      if grep -qx -- "-A $chain -d $cidr -j DROP" <<<"$rules"; then :; else log "MISSING: $chain drop for $cidr"; ok=0; fi
    done
    ((ok)) && log "ok: $chain drops private, link-local, loopback and multicast ranges"
  else
    log "MISSING: chain $chain"; ok=0
  fi
  first=$(first_rule iptables INPUT)
  if [[ $first == *"-i $bridge "*"-j DROP"* ]]; then log "ok: INPUT drops $bridge first"
  else log "MISSING: INPUT rule 1 is not '-i $bridge -j DROP' (got: ${first:-none})"; ok=0; fi
  if command -v ip6tables >/dev/null 2>&1; then
    local ch
    for ch in FORWARD INPUT; do
      first=$(first_rule ip6tables "$ch")
      if [[ $first == *"-i $bridge "*"-j DROP"* ]]; then log "ok: ip6tables $ch drops $bridge first"
      else log "warning: ip6tables $ch rule 1 is not '-i $bridge -j DROP' (IPv6 is off on the bridge)"; fi
    done
  fi
  if ((ok)); then log "complete for bridge $bridge"; return 0; fi
  log "incomplete: run: sudo $0 install --bridge $bridge"
  return 1
}

# snapshot: the iptables -S sections ah parses (see cmd/ah/sandbox_hardening.go).
snapshot_rules() {
  local dir tmp
  dir=$(dirname "$snapshot_file")
  install -d -m 0755 "$dir"
  tmp=$(mktemp "$dir/.snapshot.XXXXXX")
  {
    printf '# ah sandbox egress firewall snapshot v1 bridge=%s\n' "$bridge"
    local spec bin ch
    for spec in "iptables FORWARD" "iptables DOCKER-USER" "iptables $chain" "iptables INPUT" "ip6tables FORWARD" "ip6tables INPUT"; do
      bin=${spec%% *} ch=${spec#* }
      printf '### %s %s\n' "$bin" "$ch"
      if command -v "$bin" >/dev/null 2>&1 && "$bin" -w -S "$ch" 2>/dev/null; then :; else printf '#!unreadable\n'; fi
    done
  } > "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$snapshot_file"
  log "wrote $snapshot_file"
}

case "$cmd" in
  install) install_rules ;;
  remove) remove_rules ;;
  status) status_rules ;;
  snapshot) snapshot_rules ;;
esac
