#!/usr/bin/env bash
# scripts/tests/smoke.sh — run Magento smoke tests inside the php-fpm container
#
# Called by the Go runner after install.sh succeeds:
#   docker compose exec php-fpm bash /scripts/tests/smoke.sh
#
# Tests performed:
#   1. setup:upgrade          — ensures DB schema is up to date
#   2. setup:di:compile       — dependency injection compilation
#
# Environment:
#   RUN_DI_COMPILE  — set to 0 to skip setup:di:compile (default: 1)
#
# Exits non-zero on the first failure.

set -euo pipefail

: "${MAGENTO_DIR:=/var/www/html}"

cd "${MAGENTO_DIR}"

# ─── Step runner ──────────────────────────────────────────────────────────────
# Output is streamed rather than buffered so large commands (setup:di:compile)
# do not exhaust container memory.
run_step() {
  local step="${1}"
  shift
  local start
  start=$(date +%s)

  echo ""
  echo "=== ${step} ==="

  if "$@" 2>&1; then
    local duration
    duration=$(( $(date +%s) - start ))
    echo "[OK] ${step} passed in ${duration}s"
    return 0
  else
    local exit_code=$?
    local duration
    duration=$(( $(date +%s) - start ))
    echo "[ERROR] ${step} FAILED after ${duration}s (exit ${exit_code})" >&2
    return "${exit_code}"
  fi
}

# ─── Smoke tests ──────────────────────────────────────────────────────────────

echo ""
echo "=== Smoke Tests ==="

run_step "setup:upgrade" \
  bin/magento setup:upgrade --no-interaction --keep-generated

if [[ "${RUN_DI_COMPILE:-1}" == "1" ]]; then
  run_step "setup:di:compile" \
    bin/magento setup:di:compile --no-interaction
fi

# ─── Fix permissions ──────────────────────────────────────────────────────────
echo ""
echo "=== Fixing file permissions ==="
chown -R www-data:www-data \
  "${MAGENTO_DIR}/var" \
  "${MAGENTO_DIR}/pub" \
  "${MAGENTO_DIR}/generated" \
  2>/dev/null || \
  chmod -R 777 \
    "${MAGENTO_DIR}/var" \
    "${MAGENTO_DIR}/pub" \
    "${MAGENTO_DIR}/generated" \
    2>/dev/null || true

echo "[OK] All smoke tests passed"
