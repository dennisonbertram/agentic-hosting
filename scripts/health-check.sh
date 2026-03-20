#!/usr/bin/env bash
# health-check.sh — cron-friendly health check for agentic-hosting
#
# Checks:
#   1. GET /v1/system/health returns {"status":"ok"}
#   2. systemctl is-active ah shows active
#   3. docker ps contains expected infra containers (paas-traefik, paas-registry)
#   4. Disk usage thresholds (warn 80%, fail 90%)
#   5. Public HTTPS endpoint responds (if AH_BASE_DOMAIN is set)
#
# Environment variables:
#   ALERT_WEBHOOK_URL  — POST webhook URL for failure alerts (required unless --dry-run)
#   AH_API_PORT        — health API port (default: 8080)
#   AH_SERVICE_NAME    — systemd service name (default: ah)
#   AH_BASE_DOMAIN     — public domain to check via HTTPS (optional)
#   AH_DATA_DIR        — state data dir for disk checks (default: /var/lib/ah)
#   AH_DOCKER_DATA_DIR — Docker data root for disk checks (default: /var/lib/docker)
#   AH_DISK_WARN_PCT   — disk warning threshold (default: 80)
#   AH_DISK_FAIL_PCT   — disk failure threshold (default: 90)
#
# Usage:
#   ./scripts/health-check.sh              # run checks, alert on failure
#   ./scripts/health-check.sh --dry-run    # run checks, print results, skip webhook
#
# Exit codes:
#   0 — all checks passed
#   1 — one or more checks failed

set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
API_PORT="${AH_API_PORT:-8080}"
SERVICE_NAME="${AH_SERVICE_NAME:-ah}"
BASE_DOMAIN="${AH_BASE_DOMAIN:-}"
DATA_DIR="${AH_DATA_DIR:-/var/lib/ah}"
DOCKER_DATA_DIR="${AH_DOCKER_DATA_DIR:-/var/lib/docker}"
DISK_WARN_PCT="${AH_DISK_WARN_PCT:-80}"
DISK_FAIL_PCT="${AH_DISK_FAIL_PCT:-90}"
WEBHOOK_URL="${ALERT_WEBHOOK_URL:-}"

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
fi

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------
FAILURES=()    # array of failure messages
WARNINGS=()    # array of warning messages
HOSTNAME_STR=$(hostname 2>/dev/null || echo "unknown")
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() {
  echo "[health-check] $*"
}

fail() {
  FAILURES+=("$1")
  log "FAIL: $1"
}

warn() {
  WARNINGS+=("$1")
  log "WARN: $1"
}

pass() {
  log "OK:   $1"
}

run_with_timeout() {
  local seconds="$1"
  shift
  if command -v timeout &>/dev/null; then
    timeout "$seconds" "$@"
    return
  fi
  if command -v gtimeout &>/dev/null; then
    gtimeout "$seconds" "$@"
    return
  fi
  "$@"
}

# ---------------------------------------------------------------------------
# Check 1: Health API
# ---------------------------------------------------------------------------
check_health_api() {
  local url="http://127.0.0.1:${API_PORT}/v1/system/health"
  local response
  local http_code

  http_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$url" 2>/dev/null) || true

  if [[ "$http_code" != "200" ]]; then
    fail "Health API returned HTTP ${http_code:-timeout} (expected 200)"
    return
  fi

  response=$(curl -sf --max-time 5 "$url" 2>/dev/null) || true
  local status
  status=$(echo "$response" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null) || true

  if [[ "$status" != "ok" ]]; then
    fail "Health API status is '${status}' (expected 'ok')"
    return
  fi

  pass "Health API /v1/system/health returns ok"
}

# ---------------------------------------------------------------------------
# Check 2: systemd service
# ---------------------------------------------------------------------------
check_systemd() {
  if ! command -v systemctl &>/dev/null; then
    warn "systemctl not found — skipping service check"
    return
  fi

  local state
  state=$(systemctl is-active "$SERVICE_NAME" 2>/dev/null) || true

  if [[ "$state" != "active" ]]; then
    fail "systemd service '${SERVICE_NAME}' is '${state}' (expected 'active')"
    return
  fi

  pass "systemd service '${SERVICE_NAME}' is active"
}

# ---------------------------------------------------------------------------
# Check 3: Infrastructure containers
# ---------------------------------------------------------------------------
check_containers() {
  if ! command -v docker &>/dev/null; then
    fail "docker command not found"
    return
  fi

  local running
  running=$(run_with_timeout 10 docker ps --format '{{.Names}}' 2>/dev/null) || true

  if [[ -z "$running" ]]; then
    fail "docker ps returned no running containers (or Docker is unreachable)"
    return
  fi

  local missing=()
  for container in paas-traefik paas-registry; do
    if ! echo "$running" | grep -qx "$container"; then
      missing+=("$container")
    fi
  done

  if [[ ${#missing[@]} -gt 0 ]]; then
    fail "Missing infrastructure containers: ${missing[*]}"
    return
  fi

  pass "Infrastructure containers running: paas-traefik, paas-registry"
}

# ---------------------------------------------------------------------------
# Check 4: Disk usage
# ---------------------------------------------------------------------------
check_disk_path() {
  local label="$1"
  local path="$2"

  if [[ ! -e "$path" ]]; then
    warn "${label} path ${path} does not exist — skipping disk check"
    return
  fi

  local disk_line
  disk_line=$(df -P "$path" 2>/dev/null | tail -1) || true

  if [[ -z "$disk_line" ]]; then
    fail "Could not read disk usage via df for ${path}"
    return
  fi

  # df -P output: Filesystem 1024-blocks Used Available Capacity Mounted-on
  local used_pct
  used_pct=$(echo "$disk_line" | awk '{print $5}' | tr -d '%')

  if [[ -z "$used_pct" ]] || ! [[ "$used_pct" =~ ^[0-9]+$ ]]; then
    fail "Could not parse disk usage percentage for ${path}"
    return
  fi

  if [[ "$used_pct" -ge "$DISK_FAIL_PCT" ]]; then
    fail "${label} disk usage at ${used_pct}% for ${path} (threshold: ${DISK_FAIL_PCT}%)"
    return
  fi

  if [[ "$used_pct" -ge "$DISK_WARN_PCT" ]]; then
    warn "${label} disk usage at ${used_pct}% for ${path} (warning threshold: ${DISK_WARN_PCT}%)"
    # Still pass — warnings don't cause non-zero exit
    pass "${label} disk usage at ${used_pct}% for ${path} (warning)"
    return
  fi

  pass "${label} disk usage at ${used_pct}% for ${path}"
}

check_disk() {
  check_disk_path "State data" "$DATA_DIR"
  check_disk_path "Docker data" "$DOCKER_DATA_DIR"
}

# ---------------------------------------------------------------------------
# Check 5: Public HTTPS endpoint
# ---------------------------------------------------------------------------
check_public_https() {
  if [[ -z "$BASE_DOMAIN" ]]; then
    log "SKIP: AH_BASE_DOMAIN not set — skipping public HTTPS check"
    return
  fi

  local url="https://${BASE_DOMAIN}/"
  local http_code
  http_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 "$url" 2>/dev/null) || true

  if [[ -z "$http_code" ]] || [[ "$http_code" == "000" ]]; then
    fail "Public HTTPS endpoint ${url} is unreachable (connection failed or timed out)"
    return
  fi

  # Accept any 2xx or 3xx as healthy
  if [[ "$http_code" =~ ^[23] ]]; then
    pass "Public HTTPS endpoint ${url} returned HTTP ${http_code}"
    return
  fi

  fail "Public HTTPS endpoint ${url} returned HTTP ${http_code}"
}

# ---------------------------------------------------------------------------
# Build webhook payload
# ---------------------------------------------------------------------------
build_payload() {
  local failures_json="["
  local first=true
  for f in "${FAILURES[@]}"; do
    if $first; then first=false; else failures_json+=","; fi
    # Escape double quotes and backslashes in the message
    local escaped
    escaped=$(echo "$f" | sed 's/\\/\\\\/g; s/"/\\"/g')
    failures_json+="\"${escaped}\""
  done
  failures_json+="]"

  local warnings_json="["
  first=true
  for w in "${WARNINGS[@]}"; do
    if $first; then first=false; else warnings_json+=","; fi
    local escaped
    escaped=$(echo "$w" | sed 's/\\/\\\\/g; s/"/\\"/g')
    warnings_json+="\"${escaped}\""
  done
  warnings_json+="]"

  cat <<ENDJSON
{
  "event": "health_check_failed",
  "hostname": "${HOSTNAME_STR}",
  "timestamp": "${TIMESTAMP}",
  "failure_count": ${#FAILURES[@]},
  "warning_count": ${#WARNINGS[@]},
  "failures": ${failures_json},
  "warnings": ${warnings_json}
}
ENDJSON
}

# ---------------------------------------------------------------------------
# Send webhook alert
# ---------------------------------------------------------------------------
send_alert() {
  local payload
  payload=$(build_payload)

  if $DRY_RUN; then
    log "DRY-RUN: Would send webhook to ${WEBHOOK_URL:-<not set>}"
    log "DRY-RUN: Payload:"
    echo "$payload"
    return
  fi

  if [[ -z "$WEBHOOK_URL" ]]; then
    log "ERROR: ALERT_WEBHOOK_URL not set — cannot send alert"
    echo "$payload" >&2
    return
  fi

  local http_code
  http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    --max-time 10 \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$payload" \
    "$WEBHOOK_URL" 2>/dev/null) || true

  if [[ "$http_code" =~ ^2 ]]; then
    log "Alert sent to webhook (HTTP ${http_code})"
  else
    log "ERROR: Webhook returned HTTP ${http_code:-timeout}"
    echo "$payload" >&2
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  log "Starting health checks at ${TIMESTAMP}"
  log "Host: ${HOSTNAME_STR}, API port: ${API_PORT}, Service: ${SERVICE_NAME}"
  if $DRY_RUN; then
    log "Mode: --dry-run (webhook will not fire)"
  fi
  echo ""

  check_health_api
  check_systemd
  check_containers
  check_disk
  check_public_https

  echo ""
  log "Results: ${#FAILURES[@]} failure(s), ${#WARNINGS[@]} warning(s)"

  if [[ ${#FAILURES[@]} -gt 0 ]]; then
    echo ""
    send_alert
    exit 1
  fi

  exit 0
}

main
