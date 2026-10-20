#!/usr/bin/env bash
# One-command setup for a macOS developer machine.
#
# gVisor cannot run on macOS, so this prepares a LOCAL DEV SANDBOX: Homebrew
# prerequisites, a Docker engine (Docker Desktop, OrbStack or Colima), and the
# ah/ahc binaries. Workloads in the sandbox use the runc runtime, which is a
# dev-only fallback with no syscall sandbox. For a gVisor-isolated install on
# a Mac, use the Lima VM path in docs/ops/macmini.md instead.
#
# See docs/runbooks/setup-macos.md. Written for bash 3.2 (/bin/bash on macOS).
set -Eeuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
dry_run=0 assume_yes=0 skip_build=0 engine=auto

usage() {
  cat <<'EOF'
Usage: scripts/setup-macos.sh [--dry-run] [--yes] [--engine auto|docker-desktop|orbstack|colima] [--skip-build]

Installs with Homebrew (only what is missing): go, jq, the docker CLI, and
Colima when no Docker engine is present. Then checks that a Docker engine is
running (starting Colima if that is the engine), and builds bin/ah and bin/ahc.
Next step: make sandbox.

Options:
  --dry-run       Print what would be installed or started; change nothing
  --yes, -y       Do not prompt before installing
  --engine NAME   Docker engine to use (default auto: whichever is running or
                  installed, preferring Docker Desktop, then OrbStack, then Colima)
  --skip-build    Do not build bin/ah and bin/ahc
  -h, --help      Show this help

This is a development setup. Workloads run under runc, NOT gVisor.
EOF
}

log() { printf 'setup-macos: %s\n' "$*"; }
warn() { printf 'setup-macos: WARNING: %s\n' "$*" >&2; }
die() { printf 'setup-macos: %s\n' "$*" >&2; exit 1; }
run() {
  if ((dry_run)); then printf 'setup-macos: [dry-run] would run: %s\n' "$*"; return 0; fi
  "$@"
}

while (($#)); do
  case "$1" in
    --dry-run) dry_run=1; shift ;;
    --yes|-y) assume_yes=1; shift ;;
    --skip-build) skip_build=1; shift ;;
    --engine) (($# >= 2)) || die '--engine requires a value'; engine=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done
case "$engine" in auto|docker-desktop|orbstack|colima) ;; *) die "unknown engine: $engine" ;; esac

if [[ $(uname -s) != Darwin ]]; then
  ((dry_run)) || die 'this script is for macOS; on Linux use scripts/setup-linux.sh'
  warn 'not macOS; continuing because this is a dry run'
fi

confirm() {
  ((assume_yes || dry_run)) && return 0
  [[ -t 0 ]] || die "$1 -- rerun with --yes to proceed without a prompt"
  local reply
  printf 'setup-macos: %s Proceed? [y/N] ' "$1"
  read -r reply
  [[ $reply == y || $reply == Y || $reply == yes ]] || die 'aborted'
}

# ------------------------------------------------------------ prerequisites
if ! xcode-select -p >/dev/null 2>&1; then
  if ((dry_run)); then warn 'Xcode Command Line Tools missing (needed for cgo/SQLite): run xcode-select --install'
  else die 'Xcode Command Line Tools are required for cgo (SQLite). Run: xcode-select --install'; fi
fi
if ! command -v brew >/dev/null 2>&1; then
  if ((dry_run)); then warn 'Homebrew missing: install it from https://brew.sh, then rerun'
  else die 'Homebrew is required. Install it from https://brew.sh (review the script first), then rerun.'; fi
fi

app_installed() { [[ -d /Applications/$1.app || -d $HOME/Applications/$1.app ]]; }
docker_up() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

if [[ $engine == auto ]]; then
  if docker_up; then engine=running
  elif app_installed Docker; then engine=docker-desktop
  elif app_installed OrbStack || command -v orb >/dev/null 2>&1; then engine=orbstack
  else engine=colima
  fi
fi
log "Docker engine: $engine"

missing=''
for tool in go jq python3; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
# Docker Desktop and OrbStack ship their own docker CLI.
if ! command -v docker >/dev/null 2>&1 && [[ $engine == colima ]]; then missing="$missing docker"; fi
if [[ $engine == colima ]] && ! command -v colima >/dev/null 2>&1; then missing="$missing colima"; fi
missing=${missing# }

if [[ -n $missing ]]; then
  log "missing: $missing"
  confirm "Install with Homebrew: $missing."
  # shellcheck disable=SC2086 # word-splitting the formula list is intended
  run brew install $missing
else
  log 'Homebrew prerequisites already present'
fi

# -------------------------------------------------------------- docker engine
case "$engine" in
  running) log 'Docker engine is already running' ;;
  docker-desktop)
    if docker_up; then log 'Docker Desktop is running'
    else
      run open -a Docker
      ((dry_run)) || log 'started Docker Desktop; wait for it to report "running", then run: make sandbox'
    fi
    ;;
  orbstack)
    if docker_up; then log 'OrbStack is running'
    else
      run open -a OrbStack
      ((dry_run)) || log 'started OrbStack; wait for it to finish starting, then run: make sandbox'
    fi
    ;;
  colima)
    if ((dry_run)) || ! docker_up; then
      confirm 'Start Colima (2 CPU, 4 GiB RAM, 30 GiB disk).'
      run colima start --cpu 2 --memory 4 --disk 30
    fi
    ;;
esac

# ------------------------------------------------------------------ build
if ((skip_build == 0)); then
  log 'building bin/ah and bin/ahc'
  run make -C "$repo_root" build build-cli
fi

suffix=''
((dry_run)) && suffix=' (dry run: nothing was changed)'
cat <<EOF

setup-macos: done$suffix.
Next steps:
  make sandbox          start a throwaway local instance and deploy the example
  make sandbox-down     remove it again
Docs: docs/runbooks/setup-macos.md and docs/runbooks/local-sandbox.md

The sandbox runs workloads under runc, NOT gVisor: local development only.
For gVisor isolation on a Mac, run the Lima VM path in docs/ops/macmini.md.
EOF
