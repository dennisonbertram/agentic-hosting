#!/usr/bin/env bash
# Local dev sandbox: a throwaway ah instance on this machine's Docker.
#
#   scripts/sandbox.sh up      [--dry-run] [--yes]   (make sandbox)
#   scripts/sandbox.sh down    [--dry-run] [--keep-data]  (make sandbox-down)
#   scripts/sandbox.sh status
#
# Works on macOS (Docker Desktop, Colima, OrbStack) and Linux. Written for
# bash 3.2 because that is /bin/bash on macOS.
set -Eeuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sandbox_dir=${AH_SANDBOX_DIR:-$repo_root/.sandbox}
api_port=${AH_SANDBOX_API_PORT:-18480}
http_port=${AH_SANDBOX_HTTP_PORT:-18400}
runtime=${AH_SANDBOX_RUNTIME:-auto}
skip_example=${AH_SANDBOX_SKIP_EXAMPLE:-0}
example_dir=$repo_root/examples/openrouter-agent
example_image=ah-sandbox/openrouter-agent:dev
service_name=demo
tenant_name=sandbox-demo
tenant_email=demo@example.com
traefik_name=paas-traefik # fixed: ah connects this container name to ingress networks

dry_run=0 assume_yes=0 keep_data=0

usage() {
  cat <<'EOF'
Usage: scripts/sandbox.sh <up|down|status> [options]

  up       Start a throwaway local instance: separate data dir, generated master
           key and bootstrap/admin tokens, a local Traefik on 127.0.0.1, a demo
           tenant, and examples/openrouter-agent deployed from a local image.
  down     Delete the demo services, stop ah and the sandbox Traefik, remove
           sandbox-owned Docker networks, and delete the sandbox data dir.
  status   Show whether the sandbox is running and where.

Options:
  --dry-run     Print what would happen; change nothing (no Docker calls).
  --yes         Do not ask before starting on a Docker daemon that has other work.
  --keep-data   (down) Keep the sandbox data dir, keys and tenant credentials.
  -h, --help    Show this help.

Environment:
  AH_SANDBOX_DIR          State dir (default: <repo>/.sandbox)
  AH_SANDBOX_API_PORT     ah API port on 127.0.0.1 (default: 18480)
  AH_SANDBOX_HTTP_PORT    Traefik HTTP port on 127.0.0.1 (default: 18400)
  AH_SANDBOX_RUNTIME      auto | runsc | runc (auto: runsc when Docker has it, else runc)
  AH_SANDBOX_SKIP_EXAMPLE 1 = register the tenant but deploy nothing
  OPENROUTER_API_KEY      Optional; passed to the demo so /api/chat works.
                          Without it the demo serves /health only.

The sandbox runs ah with --dev. When gVisor is unavailable (always on macOS) it
uses the runc runtime, which has NO syscall sandbox: it is for local
development only. Never expose it to a network or run untrusted code in it.
EOF
}

log() { printf 'sandbox: %s\n' "$*"; }
warn() { printf 'sandbox: WARNING: %s\n' "$*" >&2; }
die() { printf 'sandbox: %s\n' "$*" >&2; exit 1; }
run() {
  if ((dry_run)); then printf 'sandbox: [dry-run] would run: %s\n' "$*"; return 0; fi
  "$@"
}

command_name=${1:-}
case "$command_name" in
  -h|--help|help) usage; exit 0 ;;
  up|down|status) shift ;;
  '') usage >&2; exit 2 ;;
  *) usage >&2; die "unknown command: $command_name" ;;
esac
while (($#)); do
  case "$1" in
    --dry-run) dry_run=1 ;;
    --yes|-y) assume_yes=1 ;;
    --keep-data) keep_data=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
  shift
done

valid_port() { [[ $1 =~ ^[0-9]+$ ]] && (($1 >= 1024 && $1 <= 65535)); }
valid_port "$api_port" || die "AH_SANDBOX_API_PORT must be 1024-65535"
valid_port "$http_port" || die "AH_SANDBOX_HTTP_PORT must be 1024-65535"
[[ $api_port != "$http_port" ]] || die 'API and HTTP ports must differ'
case "$runtime" in auto|runsc|runc) ;; *) die 'AH_SANDBOX_RUNTIME must be auto, runsc or runc' ;; esac
case "$sandbox_dir" in /*) ;; *) die 'AH_SANDBOX_DIR must be an absolute path' ;; esac

api_url="http://127.0.0.1:$api_port"
data_dir=$sandbox_dir/data
marker=$sandbox_dir/.ah-sandbox
secrets_file=$sandbox_dir/secrets.env
tenant_file=$sandbox_dir/tenant.env
pid_file=$sandbox_dir/ah.pid
log_file=$sandbox_dir/logs/ah.log
ah_bin=$sandbox_dir/bin/ah

# json_get FILE EXPR: print one value from a JSON file (python3 stdlib, isolated mode).
json_get() {
  python3 -I -c 'import json,sys
d=json.load(open(sys.argv[1]))
for k in sys.argv[2].split("."):
    d=d[int(k)] if isinstance(d,list) else d.get(k,"")
print("" if d is None else d)' "$1" "$2"
}
# service_ids_by_name FILE NAME: ids of services named NAME in a list response.
service_ids_by_name() {
  python3 -I -c 'import json,sys
for s in json.load(open(sys.argv[1])):
    if not sys.argv[2] or s.get("name")==sys.argv[2]: print(s["id"])' "$1" "$2"
}
rand_hex() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }

# ah_running: the PID file names a live process that is this sandbox's ah
# binary. Checking the command line guards against a stale PID file whose PID
# was reused by an unrelated process (down would otherwise signal it).
ah_pid_alive() {
  [[ -f $pid_file ]] || return 1
  local pid; pid=$(cat "$pid_file")
  [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null
}
ah_running() {
  ah_pid_alive || return 1
  local cmd
  cmd=$(ps -p "$(cat "$pid_file")" -o command= 2>/dev/null) || return 1
  [[ $cmd == *"$ah_bin"* ]]
}
api_healthy() { curl -fsS -m 3 "$api_url/v1/system/health" >/dev/null 2>&1; }

# api METHOD PATH OUTFILE [BODYFILE]: authenticated call with the tenant key.
# Credentials go through a header file so they never appear in argv.
api() {
  local method=$1 path=$2 out=$3 body=${4:-} hdr code
  hdr=$sandbox_dir/.auth-header
  ( umask 077; printf 'Authorization: Bearer %s\n' "$AH_SANDBOX_KEY" > "$hdr" )
  if [[ -n $body ]]; then
    code=$(curl -sS -m 30 -o "$out" -w '%{http_code}' -X "$method" -H @"$hdr" -H 'Content-Type: application/json' --data-binary @"$body" "$api_url$path") || code=000
  else
    code=$(curl -sS -m 30 -o "$out" -w '%{http_code}' -X "$method" -H @"$hdr" "$api_url$path") || code=000
  fi
  rm -f "$hdr"
  printf '%s' "$code"
}

docker_arch_suffix() {
  case "$(docker info --format '{{.Architecture}}')" in
    x86_64|amd64) printf 'AMD64' ;;
    aarch64|arm64) printf 'ARM64' ;;
    *) die 'unsupported Docker architecture (need amd64 or arm64)' ;;
  esac
}
# Reuse the reviewed Traefik digest pins from the production installer.
traefik_image() {
  local suffix digest
  suffix=$(docker_arch_suffix)
  digest=$(sed -n "s/^readonly TRAEFIK_DIGEST_${suffix}=//p" "$repo_root/scripts/bootstrap-stack.sh")
  [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "could not read pinned Traefik digest from scripts/bootstrap-stack.sh"
  printf 'traefik@%s' "$digest"
}

resolve_runtime() {
  if [[ $runtime != auto ]]; then printf '%s' "$runtime"; return; fi
  if ((dry_run)); then printf 'auto (runsc if Docker lists it on Linux, else runc)'; return; fi
  if [[ $(uname -s) == Linux ]] && docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q '"runsc"'; then
    printf 'runsc'
  else
    printf 'runc'
  fi
}

confirm() {
  ((assume_yes)) && return 0
  if [[ ! -t 0 ]]; then die "$1 Re-run with --yes to continue."; fi
  local reply
  printf 'sandbox: %s Continue? [y/N] ' "$1"
  read -r reply
  [[ $reply == y || $reply == Y || $reply == yes ]] || die 'aborted'
}

# Refuse to share a Docker daemon with a real ah install: the sandbox's
# reconciler and GC would treat that install's containers as orphans.
safety_checks() {
  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet ah 2>/dev/null; then
    die 'a production ah.service is active on this host; the sandbox must not share its Docker daemon'
  fi
  local label
  if docker container inspect "$traefik_name" >/dev/null 2>&1; then
    label=$(docker container inspect --format '{{index .Config.Labels "ah.sandbox"}}' "$traefik_name")
    [[ $label == true ]] || die "a non-sandbox container named $traefik_name exists (a real ah install?); refusing"
  fi
  if [[ ! -f $marker ]] && [[ -n $(docker ps -aq --filter label=ah.managed=true) ]]; then
    die 'Docker already has ah.managed containers that this sandbox did not create; refusing'
  fi
  if [[ -n $(docker ps -q) ]] && ! docker container inspect "$traefik_name" >/dev/null 2>&1; then
    confirm 'Docker is running other containers. The sandbox only manages ah-labelled objects and disables daemon-wide pruning.'
  fi
}

write_traefik_config() {
  mkdir -p "$sandbox_dir/traefik/dynamic"
  cat > "$sandbox_dir/traefik/traefik.yml" <<'EOF'
# Local sandbox only: HTTP on one entrypoint, file provider, no dashboard.
api:
  dashboard: false
log:
  level: INFO
providers:
  file:
    directory: /etc/traefik/dynamic
    watch: true
entryPoints:
  web:
    address: ":80"
EOF
}

start_traefik() {
  local img
  if docker container inspect "$traefik_name" >/dev/null 2>&1; then
    if [[ $(docker container inspect --format '{{.State.Running}}' "$traefik_name") != true ]]; then
      docker start "$traefik_name" >/dev/null
    fi
    return
  fi
  # Resolve first: a die inside an argument's $(...) would not stop docker run.
  img=$(traefik_image) || exit 1
  docker run -d --name "$traefik_name" --label ah.sandbox=true \
    -p "127.0.0.1:$http_port:80" \
    -v "$sandbox_dir/traefik/traefik.yml:/etc/traefik/traefik.yml:ro" \
    -v "$sandbox_dir/traefik/dynamic:/etc/traefik/dynamic:ro" \
    "$img" --configFile=/etc/traefik/traefik.yml >/dev/null
}

start_ah() {
  local rt=$1 nixpacks
  nixpacks=$(command -v nixpacks || printf '/nonexistent/nixpacks')
  mkdir -p "$sandbox_dir/logs" "$sandbox_dir/builds"
  (
    set -a
    # shellcheck disable=SC1090
    . "$secrets_file"
    set +a
    # This script's AH_SANDBOX_RUNTIME (auto|runsc|runc) picks the local
    # instance's AH_CONTAINER_RUNTIME. ah reads AH_SANDBOX_RUNTIME as the
    # disposable-sandbox runtime, so it must not inherit this value.
    unset AH_SANDBOX_RUNTIME
    export AH_DATA_DIR="$data_dir" AH_BUILD_DIR="$sandbox_dir/builds" \
      AH_TRAEFIK_CONFIG_DIR="$sandbox_dir/traefik/dynamic" AH_DOCKER_DATA_DIR="$data_dir" \
      AH_NIXPACKS_PATH="$nixpacks" AH_CONTAINER_RUNTIME="$rt" \
      AH_WARM_POOL_ENABLED=false AH_GC_PRUNE_DOCKER=false AH_BASE_DOMAIN=
    exec nohup "$ah_bin" serve --dev --port "$api_port" \
      --db-path "$data_dir/ah.db" --master-key-path "$data_dir/master.key"
  ) >>"$log_file" 2>&1 &
  printf '%s\n' "$!" > "$pid_file"
  printf '%s\n' "$rt" > "$sandbox_dir/runtime"
  local i
  for i in $(seq 1 40); do
    api_healthy && return 0
    # Liveness only: until the subshell execs ah, its command line is still
    # this script's, so the command-line check in ah_running would misfire.
    ah_pid_alive || { tail -n 40 "$log_file" >&2; die "ah exited during startup (log: $log_file)"; }
    sleep 0.5
  done
  tail -n 40 "$log_file" >&2
  die "ah did not become healthy on $api_url (log: $log_file)"
}

register_tenant() {
  [[ -f $tenant_file ]] && return 0
  local hdr body out code
  hdr=$sandbox_dir/.bootstrap-header body=$sandbox_dir/.register.json out=$sandbox_dir/.register-out.json
  (
    umask 077
    # shellcheck disable=SC1090
    . "$secrets_file"
    printf 'X-Bootstrap-Token: %s\n' "$AH_BOOTSTRAP_TOKEN" > "$hdr"
  )
  printf '{"name":"%s","email":"%s"}\n' "$tenant_name" "$tenant_email" > "$body"
  code=$(curl -sS -m 30 -o "$out" -w '%{http_code}' -X POST -H @"$hdr" -H 'Content-Type: application/json' --data-binary @"$body" "$api_url/v1/tenants/register") || code=000
  rm -f "$hdr" "$body"
  [[ $code == 201 ]] || { cat "$out" >&2 2>/dev/null || :; rm -f "$out"; die "tenant registration failed (HTTP $code)"; }
  ( umask 077; printf 'AH_SANDBOX_TENANT_ID=%s\nAH_SANDBOX_KEY=%s\n' "$(json_get "$out" tenant_id)" "$(json_get "$out" api_key)" > "$tenant_file" )
  rm -f "$out"
  log "registered demo tenant $tenant_name"
}

load_tenant() {
  [[ -f $tenant_file ]] || return 1
  # shellcheck disable=SC1090
  . "$tenant_file"
  [[ -n ${AH_SANDBOX_KEY:-} ]]
}

deploy_example() {
  local out=$sandbox_dir/.svc.json body=$sandbox_dir/.svc-create.json code id status i
  log "building $example_image from examples/openrouter-agent"
  docker build -q -t "$example_image" "$example_dir" >/dev/null
  code=$(api GET /v1/services "$out")
  [[ $code == 200 ]] || die "listing services failed (HTTP $code)"
  id=$(service_ids_by_name "$out" "$service_name" | sed -n 1p)
  if [[ -z $id ]]; then
    (
      umask 077
      # Secrets go through the environment, never argv (readable by any
      # local user through ps or /proc/<pid>/cmdline).
      OPENROUTER_API_KEY=${OPENROUTER_API_KEY:-sandbox-placeholder-no-provider-calls} \
        DEMO_AUTH_TOKEN=$(rand_hex) \
        python3 -I -c 'import json,os,sys
print(json.dumps({"name":sys.argv[1],"image":sys.argv[2],"port":3000,
  "env":{"OPENROUTER_API_KEY":os.environ["OPENROUTER_API_KEY"],"DEMO_AUTH_TOKEN":os.environ["DEMO_AUTH_TOKEN"]}}))' \
        "$service_name" "$example_image" > "$body"
    )
    code=$(api POST /v1/services "$out" "$body")
    rm -f "$body"
    [[ $code == 201 ]] || { cat "$out" >&2; die "service create failed (HTTP $code)"; }
    id=$(json_get "$out" id)
    log "created service $service_name ($id); waiting for it to run"
  fi
  status=
  for i in $(seq 1 90); do
    code=$(api GET "/v1/services/$id" "$out")
    status=$(json_get "$out" status 2>/dev/null || :)
    case "$status" in running|failed|error|crashed) break ;; esac
    sleep 2
  done
  [[ $status == running ]] || { cat "$out" >&2; die "service did not reach running (status: ${status:-unknown}); see $log_file"; }
  rm -f "$out"
  printf '%s' "$id" > "$sandbox_dir/service.id"
  # Traefik's file watcher may miss events on some macOS bind mounts; restart once if the route is not live.
  for i in $(seq 1 20); do
    curl -fsS -m 3 -H "Host: $id.localhost" "http://127.0.0.1:$http_port/health" >/dev/null 2>&1 && return 0
    if ((i == 10)); then docker restart "$traefik_name" >/dev/null; fi
    sleep 1
  done
  warn "service is running but http://$id.localhost:$http_port/health did not answer yet; check: docker logs $traefik_name"
}

print_summary() {
  local id=''
  [[ -f $sandbox_dir/service.id ]] && id=$(cat "$sandbox_dir/service.id")
  cat <<EOF

  ah sandbox is up (dev mode, runtime: $1)
  API:            $api_url   (log: $log_file)
  Tenant API key: $AH_SANDBOX_KEY
  Use the CLI:    export AH_URL=$api_url AH_KEY=$AH_SANDBOX_KEY
                  go run ./cmd/ahc status
EOF
  if [[ -n $id ]]; then
    cat <<EOF
  Demo service:   http://$id.localhost:$http_port/health
                  (curl -H 'Host: $id.localhost' http://127.0.0.1:$http_port/health)
EOF
  fi
  cat <<EOF
  Tear down:      make sandbox-down

EOF
  if [[ $1 != runsc ]]; then
    warn "workloads run under $1, NOT gVisor. Local development only."
  fi
}

cmd_up() {
  local rt
  rt=$(resolve_runtime)
  if ((dry_run)); then
    log "[dry-run] state dir: $sandbox_dir"
    log "[dry-run] would generate master key + bootstrap/admin tokens (0600) if absent"
    log "[dry-run] would build $ah_bin from ./cmd/ah"
    log "[dry-run] would start Traefik container $traefik_name on 127.0.0.1:$http_port (pinned digest from bootstrap-stack.sh)"
    log "[dry-run] would start: ah serve --dev --port $api_port (AH_CONTAINER_RUNTIME=$rt, AH_WARM_POOL_ENABLED=false, AH_GC_PRUNE_DOCKER=false)"
    log "[dry-run] would register tenant $tenant_name <$tenant_email>"
    if [[ $skip_example == 1 ]]; then
      log '[dry-run] would skip the example deploy (AH_SANDBOX_SKIP_EXAMPLE=1)'
    else
      log "[dry-run] would build $example_image from examples/openrouter-agent and deploy it as service '$service_name'"
    fi
    warn 'when runc is used it is a dev-only fallback without gVisor isolation'
    return 0
  fi
  for c in docker curl python3 go od; do command -v "$c" >/dev/null 2>&1 || die "missing prerequisite: $c (see docs/runbooks/local-sandbox.md)"; done
  docker info >/dev/null 2>&1 || die 'Docker is not reachable; start Docker Desktop, OrbStack or Colima first'
  rt=$(resolve_runtime)

  if ah_running && api_healthy && load_tenant; then
    log 'sandbox already running'
    [[ -f $sandbox_dir/runtime ]] && rt=$(cat "$sandbox_dir/runtime")
    print_summary "$rt"
    return 0
  fi
  safety_checks

  mkdir -p "$data_dir" "$sandbox_dir/bin" "$sandbox_dir/logs"
  chmod 0700 "$sandbox_dir" "$data_dir"
  : > "$marker"
  if [[ ! -f $data_dir/master.key ]]; then
    ( umask 077; rand_hex > "$data_dir/master.key"; printf '\n' >> "$data_dir/master.key" )
    log 'generated sandbox master key'
  fi
  if [[ ! -f $secrets_file ]]; then
    ( umask 077; printf 'AH_BOOTSTRAP_TOKEN=%s\nAH_ADMIN_TOKEN=%s\n' "$(rand_hex)" "$(rand_hex)" > "$secrets_file" )
    log 'generated sandbox bootstrap and admin tokens'
  fi

  log 'building ah'
  (cd "$repo_root" && CGO_ENABLED=1 go build -o "$ah_bin" ./cmd/ah)

  write_traefik_config
  start_traefik
  if ! ah_running; then start_ah "$rt"; fi
  register_tenant
  load_tenant || die "tenant credentials missing in $tenant_file"
  if [[ $skip_example != 1 ]]; then deploy_example; fi
  print_summary "$rt"
}

cmd_down() {
  if ((dry_run)); then
    log "[dry-run] would delete sandbox services via $api_url, stop ah (pid file $pid_file)"
    log "[dry-run] would remove the sandbox-labelled $traefik_name container and the demo tenant's ah-tenant-*/ah-ingress-* networks and ah-env-* workspace volumes"
    if ((keep_data)); then log "[dry-run] would keep $sandbox_dir"; else log "[dry-run] would delete $sandbox_dir"; fi
    return 0
  fi
  local out=$sandbox_dir/.svc.json id tid='' i pid vols v
  if load_tenant; then tid=${AH_SANDBOX_TENANT_ID:-}; fi
  if [[ -n $tid ]] && ah_running && api_healthy; then
    if [[ $(api GET /v1/services "$out") == 200 ]]; then
      for id in $(service_ids_by_name "$out" ''); do
        log "deleting service $id"
        api DELETE "/v1/services/$id" /dev/null >/dev/null || :
      done
    fi
    rm -f "$out"
  fi
  if ah_running; then
    pid=$(cat "$pid_file")
    log "stopping ah (pid $pid)"
    kill "$pid" 2>/dev/null || :
    for i in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
    if ah_running; then kill -9 "$pid" 2>/dev/null || :; fi
  elif ah_pid_alive; then
    warn "pid file $pid_file names a process that is not the sandbox ah; not signalling it"
  fi
  rm -f "$pid_file"
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    # Remove Traefik before the networks: when ah could not clean up through
    # the API, Traefik is still attached to the ah-ingress-* networks and
    # docker network rm would fail with active endpoints.
    if docker container inspect "$traefik_name" >/dev/null 2>&1 &&
      [[ $(docker container inspect --format '{{index .Config.Labels "ah.sandbox"}}' "$traefik_name") == true ]]; then
      log "removing sandbox $traefik_name"
      docker rm -f "$traefik_name" >/dev/null
    fi
    if [[ -n $tid ]]; then
      # Workspace volumes (ah-env-<id>) carry no tenant label, and the state
      # dir that records them is deleted below, so collect them from the
      # tenant's containers before removing those.
      vols=$(docker ps -aq --filter "label=ah.tenant=$tid" |
        xargs -r docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' 2>/dev/null |
        grep -E '^ah-env-' | sort -u || :)
      for id in $(docker ps -aq --filter "label=ah.tenant=$tid"); do docker rm -f "$id" >/dev/null || :; done
      for v in $vols; do
        docker volume rm "$v" >/dev/null 2>&1 || warn "could not remove volume $v"
      done
      for id in $(docker network ls --format '{{.Name}}' | grep -E "^ah-(tenant|ingress)-$tid(-|$)" || :); do
        docker network rm "$id" >/dev/null 2>&1 || warn "could not remove network $id"
      done
    fi
  fi
  if ((keep_data)); then
    log "kept $sandbox_dir"
  elif [[ -f $marker ]]; then
    rm -rf "$sandbox_dir"
    log "deleted $sandbox_dir"
  elif [[ -e $sandbox_dir ]]; then
    warn "$sandbox_dir has no sandbox marker; not deleting it"
  fi
  log "sandbox is down (image $example_image is kept; remove with: docker rmi $example_image)"
}

cmd_status() {
  if ah_running && api_healthy; then
    log "running: $api_url (pid $(cat "$pid_file"), state $sandbox_dir)"
    # An if, not &&: without a demo service the && list would make status
    # (and the script) exit 1, which means "not running".
    if [[ -f $sandbox_dir/service.id ]]; then
      log "demo: http://$(cat "$sandbox_dir/service.id").localhost:$http_port/health"
    fi
  else
    log 'not running'
    return 1
  fi
}

case "$command_name" in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
esac
