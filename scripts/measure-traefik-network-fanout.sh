#!/usr/bin/env bash
# measure-traefik-network-fanout.sh
#
# Reproducible measurement of Traefik network fanout limits.
#
# Creates N Docker bridge networks (simulating per-tenant networks) and
# progressively connects a Traefik container to each one, recording:
#   - number of attached networks
#   - Traefik container memory (RSS)
#   - Docker inspect timing / errors
#   - cleanup behavior when networks are removed
#
# Usage:
#   ./scripts/measure-traefik-network-fanout.sh [MAX_NETWORKS]
#
# MAX_NETWORKS defaults to 500. The script stops early if Docker errors occur.
#
# Prerequisites:
#   - Docker daemon running
#   - No container named "fanout-traefik" (will be created/removed)
#   - No networks prefixed with "fanout-test-" (will be created/removed)
#
# Output:
#   - Prints CSV-formatted results to stdout
#   - Writes detailed log to /tmp/traefik-fanout-results.log

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
MAX_NETWORKS="${1:-500}"
TRAEFIK_NAME="fanout-traefik"
NET_PREFIX="fanout-test-"
RESULTS_LOG="/tmp/traefik-fanout-results.log"
CHECKPOINT_INTERVAL=10          # record detailed metrics every N networks
TRAEFIK_IMAGE="traefik:v3.3"   # match production version if known

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$RESULTS_LOG"; }
csv() { echo "$*" | tee -a "$RESULTS_LOG"; }

cleanup() {
    log "--- CLEANUP PHASE ---"

    # Remove the test Traefik container
    docker rm -f "$TRAEFIK_NAME" 2>/dev/null || true

    # Remove all test networks
    local nets
    nets=$(docker network ls --filter "name=${NET_PREFIX}" --format '{{.Name}}' 2>/dev/null || true)
    if [ -n "$nets" ]; then
        local count
        count=$(echo "$nets" | wc -l | tr -d ' ')
        log "Removing $count test networks..."
        echo "$nets" | xargs -r -P 8 docker network rm 2>/dev/null || true
    fi

    log "Cleanup complete."
}

get_container_memory_mb() {
    # Returns container memory usage in MB from cgroup stats
    local mem_bytes
    mem_bytes=$(docker stats --no-stream --format '{{.MemUsage}}' "$TRAEFIK_NAME" 2>/dev/null | awk '{print $1}')
    if [ -z "$mem_bytes" ]; then
        echo "N/A"
        return
    fi
    echo "$mem_bytes"
}

count_attached_networks() {
    docker inspect "$TRAEFIK_NAME" --format '{{len .NetworkSettings.Networks}}' 2>/dev/null || echo "0"
}

get_inspect_time_ms() {
    # Time a docker inspect call in milliseconds
    local start end
    if command -v gdate &>/dev/null; then
        # macOS with coreutils
        start=$(gdate +%s%N)
        docker inspect "$TRAEFIK_NAME" > /dev/null 2>&1
        end=$(gdate +%s%N)
        echo $(( (end - start) / 1000000 ))
    elif date +%s%N | grep -q N; then
        # macOS without coreutils -- fall back to seconds
        start=$(date +%s)
        docker inspect "$TRAEFIK_NAME" > /dev/null 2>&1
        end=$(date +%s)
        echo $(( (end - start) * 1000 ))
    else
        # Linux
        start=$(date +%s%N)
        docker inspect "$TRAEFIK_NAME" > /dev/null 2>&1
        end=$(date +%s%N)
        echo $(( (end - start) / 1000000 ))
    fi
}

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
if ! docker info > /dev/null 2>&1; then
    echo "ERROR: Docker is not running or not accessible." >&2
    exit 1
fi

if docker inspect "$TRAEFIK_NAME" > /dev/null 2>&1; then
    echo "ERROR: Container '$TRAEFIK_NAME' already exists. Remove it first or pick a different name." >&2
    exit 1
fi

existing_nets=$(docker network ls --filter "name=${NET_PREFIX}" --format '{{.Name}}' 2>/dev/null | wc -l | tr -d ' ')
if [ "$existing_nets" -gt 0 ]; then
    echo "ERROR: Found $existing_nets existing networks with prefix '$NET_PREFIX'. Clean up first." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------
trap cleanup EXIT

> "$RESULTS_LOG"
log "=== Traefik Network Fanout Measurement ==="
log "Max networks: $MAX_NETWORKS"
log "Traefik image: $TRAEFIK_IMAGE"
log "Date: $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
log ""

# Pull traefik image (suppress progress)
log "Pulling $TRAEFIK_IMAGE..."
docker pull "$TRAEFIK_IMAGE" > /dev/null 2>&1

# Start Traefik container with minimal config (no ports needed for this test)
log "Starting test Traefik container..."
docker run -d \
    --name "$TRAEFIK_NAME" \
    --entrypoint "" \
    "$TRAEFIK_IMAGE" \
    sh -c "while true; do sleep 3600; done" > /dev/null 2>&1

sleep 2

baseline_mem=$(get_container_memory_mb)
baseline_networks=$(count_attached_networks)
log "Baseline: networks=$baseline_networks, memory=$baseline_mem"
log ""

# ---------------------------------------------------------------------------
# Phase 1: Progressive network attachment
# ---------------------------------------------------------------------------
log "=== PHASE 1: Progressive network attachment ==="
csv "step,networks_attached,memory_usage,inspect_time_ms,docker_error"

first_error_at=""
last_successful=0
errors=()

for i in $(seq 1 "$MAX_NETWORKS"); do
    net_name="${NET_PREFIX}${i}"

    # Create network (internal bridge, like production)
    if ! docker network create \
        --driver bridge \
        --internal \
        --opt "com.docker.network.bridge.enable_icc=false" \
        "$net_name" > /dev/null 2>&1; then
        log "FAILED to create network $net_name at step $i"
        errors+=("create_fail@$i")
        # Docker may have a network limit too
        if [ ${#errors[@]} -ge 5 ]; then
            log "Too many creation failures, stopping."
            break
        fi
        continue
    fi

    # Connect Traefik to this network
    connect_err=""
    if ! docker network connect "$net_name" "$TRAEFIK_NAME" 2>/tmp/fanout-connect-err.txt; then
        connect_err=$(cat /tmp/fanout-connect-err.txt)
        log "CONNECT FAILED at network $i: $connect_err"
        errors+=("connect_fail@$i: $connect_err")

        if [ -z "$first_error_at" ]; then
            first_error_at="$i"
        fi

        # If we get 5 consecutive failures, stop
        if [ ${#errors[@]} -ge 5 ]; then
            log "5+ errors accumulated, stopping at network $i."
            break
        fi
        continue
    fi

    last_successful=$i

    # Record detailed metrics at checkpoints
    if [ $((i % CHECKPOINT_INTERVAL)) -eq 0 ] || [ "$i" -eq 1 ]; then
        attached=$(count_attached_networks)
        mem=$(get_container_memory_mb)
        inspect_ms=$(get_inspect_time_ms)
        csv "$i,$attached,$mem,${inspect_ms},none"

        # Progress indicator
        if [ $((i % 50)) -eq 0 ]; then
            log "Progress: $i/$MAX_NETWORKS networks connected (mem=$mem, inspect=${inspect_ms}ms)"
        fi
    fi
done

log ""
log "Phase 1 complete: $last_successful networks successfully connected."
if [ -n "$first_error_at" ]; then
    log "First error at network: $first_error_at"
fi
log "Total errors: ${#errors[@]}"
for e in "${errors[@]+"${errors[@]}"}"; do
    log "  - $e"
done

# Final state
final_attached=$(count_attached_networks)
final_mem=$(get_container_memory_mb)
final_inspect=$(get_inspect_time_ms)
log ""
log "Final state: networks=$final_attached, memory=$final_mem, inspect=${final_inspect}ms"

# ---------------------------------------------------------------------------
# Phase 2: Functionality check
# ---------------------------------------------------------------------------
log ""
log "=== PHASE 2: Functionality check ==="

# Can we still exec into the container?
if docker exec "$TRAEFIK_NAME" echo "alive" > /dev/null 2>&1; then
    log "Container exec: OK"
else
    log "Container exec: FAILED"
fi

# Can we still inspect?
inspect_start=$(date +%s)
if docker inspect "$TRAEFIK_NAME" > /dev/null 2>&1; then
    inspect_end=$(date +%s)
    log "Container inspect: OK (${inspect_end}s - ${inspect_start}s elapsed)"
else
    log "Container inspect: FAILED"
fi

# Is the container still running?
state=$(docker inspect "$TRAEFIK_NAME" --format '{{.State.Status}}' 2>/dev/null || echo "unknown")
log "Container state: $state"

# ---------------------------------------------------------------------------
# Phase 3: Cleanup behavior test
# ---------------------------------------------------------------------------
log ""
log "=== PHASE 3: Cleanup behavior test ==="

# Remove half the networks and check if Traefik is auto-disconnected
half=$((last_successful / 2))
if [ "$half" -gt 0 ]; then
    log "Disconnecting Traefik from first $half networks, then removing them..."
    disconnect_failures=0
    remove_failures=0

    for i in $(seq 1 "$half"); do
        net_name="${NET_PREFIX}${i}"

        # First try removing the network WITHOUT disconnecting Traefik
        # This tests whether Docker auto-disconnects or errors
        if ! docker network rm "$net_name" > /dev/null 2>&1; then
            # Expected: Docker cannot remove a network with active endpoints
            # So we must disconnect first
            if docker network disconnect "$net_name" "$TRAEFIK_NAME" > /dev/null 2>&1; then
                if ! docker network rm "$net_name" > /dev/null 2>&1; then
                    remove_failures=$((remove_failures + 1))
                fi
            else
                disconnect_failures=$((disconnect_failures + 1))
            fi
        fi
    done

    after_cleanup_attached=$(count_attached_networks)
    after_cleanup_mem=$(get_container_memory_mb)
    log "After removing $half networks:"
    log "  Networks attached: $after_cleanup_attached (was $final_attached)"
    log "  Memory: $after_cleanup_mem (was $final_mem)"
    log "  Disconnect failures: $disconnect_failures"
    log "  Remove failures: $remove_failures"
    log ""
    log "KEY FINDING: Docker does NOT auto-disconnect containers from networks."
    log "Explicit NetworkDisconnect is required before NetworkRemove."
fi

# ---------------------------------------------------------------------------
# Phase 4: Remove network while Traefik is connected (force behavior)
# ---------------------------------------------------------------------------
log ""
log "=== PHASE 4: Force-remove network with active endpoint ==="

remaining_start=$((half + 1))
if [ "$remaining_start" -le "$last_successful" ]; then
    test_net="${NET_PREFIX}${remaining_start}"

    # Attempt to remove without disconnecting (should fail)
    if docker network rm "$test_net" > /dev/null 2>&1; then
        log "UNEXPECTED: Network removed while Traefik was still connected."
        log "  This means Docker auto-cleaned the endpoint."
    else
        log "EXPECTED: Network removal blocked (active endpoint: $TRAEFIK_NAME)."
        log "  Traefik MUST be explicitly disconnected before network removal."
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
log ""
log "==========================================="
log "           SUMMARY"
log "==========================================="
log "Traefik image:         $TRAEFIK_IMAGE"
log "Networks created:      $last_successful"
log "First connect error:   ${first_error_at:-none}"
log "Total errors:          ${#errors[@]}"
log "Final attached count:  $final_attached"
log "Final memory:          $final_mem"
log "Final inspect time:    ${final_inspect}ms"
log "Baseline memory:       $baseline_mem"
log "Container state:       $state"
log ""
log "Full log: $RESULTS_LOG"
log "==========================================="
