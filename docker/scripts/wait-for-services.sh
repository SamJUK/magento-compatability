#!/usr/bin/env bash
# scripts/wait-for-services.sh — wait for all required services to be reachable
# before proceeding with Magento installation.
#
# Usage (inside php-fpm container, called by install.sh):
#   DB_HOST=db DB_PORT=3306 SEARCH_HOST=search SEARCH_PORT=9200  \
#   CACHE_HOST=cache CACHE_PORT=6379 QUEUE_HOST=queue QUEUE_PORT=5672 \
#   bash /scripts/wait-for-services.sh
#
# Environment:
#   WAIT_TIMEOUT    — maximum seconds to wait per service (default: 180)
#   WAIT_INTERVAL   — seconds between retries (default: 3)

set -euo pipefail

: "${DB_HOST:=db}"
: "${DB_PORT:=3306}"
: "${SEARCH_HOST:=search}"
: "${SEARCH_PORT:=9200}"
: "${CACHE_HOST:=cache}"
: "${CACHE_PORT:=6379}"
: "${QUEUE_HOST:=queue}"
: "${QUEUE_PORT:=5672}"
: "${WAIT_TIMEOUT:=180}"
: "${WAIT_INTERVAL:=3}"

# ─── TCP reachability check ───────────────────────────────────────────────────
# Uses bash /dev/tcp rather than nc — available in all bash environments
# without requiring the netcat package.
wait_for_tcp() {
  local label="${1}"
  local host="${2}"
  local port="${3}"
  local elapsed=0

  echo "[INFO] Waiting for ${label} at ${host}:${port} (timeout: ${WAIT_TIMEOUT}s)..."

  until (: </dev/tcp/"${host}"/"${port}") 2>/dev/null; do
    if (( elapsed >= WAIT_TIMEOUT )); then
      echo "[ERROR] ${label} not reachable at ${host}:${port} after ${WAIT_TIMEOUT}s" >&2
      return 1
    fi
    sleep "${WAIT_INTERVAL}"
    elapsed=$(( elapsed + WAIT_INTERVAL ))
  done

  echo "[OK] ${label} is up at ${host}:${port} (waited ${elapsed}s)"
}

# ─── Search engine cluster health check ──────────────────────────────────────
# Verifies the cluster status is green or yellow (not red) before proceeding.
# A red cluster accepts connections but cannot index documents.
wait_for_search() {
  local label="${1}"
  local url="${2}/_cluster/health"
  local elapsed=0

  echo "[INFO] Waiting for ${label} at ${url} (timeout: ${WAIT_TIMEOUT}s)..."

  until curl -sf "${url}" 2>/dev/null | grep -q '"status":"green"\|"status":"yellow"'; do
    if (( elapsed >= WAIT_TIMEOUT )); then
      echo "[ERROR] ${label} cluster not healthy at ${url} after ${WAIT_TIMEOUT}s" >&2
      return 1
    fi
    sleep "${WAIT_INTERVAL}"
    elapsed=$(( elapsed + WAIT_INTERVAL ))
  done

  echo "[OK] ${label} cluster healthy at ${url} (waited ${elapsed}s)"
}

# ─── Main ─────────────────────────────────────────────────────────────────────

echo ""
echo "=== Waiting for all services ==="

wait_for_tcp    "Database"      "${DB_HOST}"    "${DB_PORT}"
wait_for_tcp    "Cache"         "${CACHE_HOST}" "${CACHE_PORT}"
wait_for_tcp    "Queue"         "${QUEUE_HOST}" "${QUEUE_PORT}"
wait_for_search "Search engine" "http://${SEARCH_HOST}:${SEARCH_PORT}"

echo "[OK] All services are reachable"
