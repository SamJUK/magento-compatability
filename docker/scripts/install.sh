#!/usr/bin/env bash
# scripts/install.sh — install Magento / MageOS via Composer and run setup:install
#
# Called inside the php-fpm container by the Go runner:
#   docker compose exec php-fpm env … bash /scripts/install.sh
#
# Required environment variables:
#   PRODUCT_PACKAGE  — e.g. magento/project-community-edition
#   PRODUCT_VERSION  — e.g. 2.4.8
#   MIRROR_URL       — Composer repository URL (no auth required)
#   PHP_VERSION      — e.g. 8.3 (for Composer version selection)
#   DB_HOST / DB_PORT / DB_NAME / DB_USER / DB_PASSWORD
#   SEARCH_TYPE      — opensearch | elasticsearch7
#   SEARCH_HOST / SEARCH_PORT
#   CACHE_HOST / CACHE_PORT
#   QUEUE_HOST / QUEUE_PORT / QUEUE_USER / QUEUE_PASSWORD
#   MAGENTO_BASE_URL — e.g. http://localhost:32768

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

composer --version
php -v

# ─── Defaults ─────────────────────────────────────────────────────────────────
: "${PRODUCT_PACKAGE:=magento/project-community-edition}"
: "${PRODUCT_VERSION:=2.4.8}"
: "${MIRROR_URL:=https://mage-os.hypernode.com/mirror/}"
: "${MAGENTO_DIR:=/var/www/html}"
: "${DB_HOST:=db}"
: "${DB_PORT:=3306}"
: "${DB_NAME:=magento}"
: "${DB_USER:=magento}"
: "${DB_PASSWORD:=magento}"
: "${SEARCH_TYPE:=opensearch}"
: "${SEARCH_HOST:=search}"
: "${SEARCH_PORT:=9200}"
: "${CACHE_HOST:=cache}"
: "${CACHE_PORT:=6379}"
: "${QUEUE_HOST:=queue}"
: "${QUEUE_PORT:=5672}"
: "${QUEUE_USER:=magento}"
: "${QUEUE_PASSWORD:=magento}"
: "${MAGENTO_BASE_URL:=http://localhost}"
: "${MAGENTO_ADMIN_USER:=admin}"
: "${MAGENTO_ADMIN_PASSWORD:=Admin123!}"
: "${MAGENTO_ADMIN_EMAIL:=admin@example.com}"
: "${INSTALL_SAMPLE_DATA:=0}"

# ─── Version-specific Composer constraint fixes ───────────────────────────────
# Some releases ship with broken package version constraints that prevent clean
# installs. These composer require aliases pin the correct versions before
# composer install runs, so the right packages are resolved on the first pass.
apply_version_fixes() {
  case "${PRODUCT_VERSION}" in
    2.4.4)
      echo "[INFO] Applying 2.4.4 version constraint fixes"
      composer require "magento/security-package:1.1.3-p1 as 1.1.3" \
        --no-update --no-interaction 2>&1
      composer require "magento/inventory-metapackage:1.2.4-p1 as 1.2.4" \
        --no-update --no-interaction 2>&1
      ;;
  esac
}

# ─── Patch application ────────────────────────────────────────────────────────
# Applies .patch files from /scripts/patches/ to the Magento installation.
# Uses `patch --dry-run` to test applicability — skips silently if a patch
# does not apply (wrong version or already applied in cached vendor).
apply_patch_files() {
  local patches_dir="/scripts/patches"
  [[ -d "${patches_dir}" ]] || return 0

  local applied=0 skipped=0
  for patch_file in "${patches_dir}"/*.patch; do
    [[ -f "${patch_file}" ]] || continue
    local name
    name="$(basename "${patch_file}")"
    if patch --dry-run -p1 -d "${MAGENTO_DIR}" < "${patch_file}" &>/dev/null; then
      patch -p1 -d "${MAGENTO_DIR}" < "${patch_file}" > /dev/null
      echo "[OK] Applied patch: ${name}"
      (( applied++ )) || true
    else
      echo "[INFO] Skipped patch (not applicable): ${name}"
      (( skipped++ )) || true
    fi
  done
  echo "[INFO] Patching complete: ${applied} applied, ${skipped} skipped"
}

# ─── Wait for services ────────────────────────────────────────────────────────

echo ""
echo "=== Waiting for services ==="
bash "${SCRIPT_DIR}/wait-for-services.sh"

# ─── Vendor cache setup ───────────────────────────────────────────────────────
: "${VENDOR_CACHE_DIR:=/vendor-cache}"
_pkg_slug="${PRODUCT_PACKAGE//\//-}"
_pkg_slug="${_pkg_slug// /-}"
VENDOR_CACHE_KEY="${PHP_VERSION:-8.3}-${_pkg_slug}-${PRODUCT_VERSION}"
VENDOR_CACHE_PATH="${VENDOR_CACHE_DIR}/${VENDOR_CACHE_KEY}"

# ─── Composer global config ───────────────────────────────────────────────────
composer config --global audit.abandoned ignore 2>/dev/null || true
composer config --global audit.block-insecure false 2>/dev/null || true
composer config --global allow-plugins true 2>/dev/null || true

rm -rf "${MAGENTO_DIR:?}/"*

# ─── Composer create-project ──────────────────────────────────────────────────
cd "${MAGENTO_DIR}"

# Get composer.json + lock without downloading vendor so we can inject
# version fixes before the first composer install.
composer create-project \
  --repository-url="${MIRROR_URL}" \
  --no-interaction \
  --no-progress \
  --no-install \
  "${PRODUCT_PACKAGE}:${PRODUCT_VERSION}" \
  . \
  2>&1

apply_version_fixes

if [[ -d "${VENDOR_CACHE_PATH}/vendor" ]]; then
  echo ""
  echo "=== Vendor cache hit — ${VENDOR_CACHE_KEY} ==="

  echo "[INFO] Restoring vendor from cache"
  cp -a "${VENDOR_CACHE_PATH}/vendor" "${MAGENTO_DIR}/vendor"

  rm -rf "${MAGENTO_DIR}/vendor/magento/magento2-base" \
         "${MAGENTO_DIR}/vendor/mage-os/magento2-base"
else
  echo ""
  echo "=== Installing ${PRODUCT_PACKAGE}:${PRODUCT_VERSION} (no vendor cache) ==="
fi

composer install \
  --no-interaction \
  --no-progress \
  2>&1

apply_patch_files

echo "[OK] Composer install complete"

# ─── setup:install ────────────────────────────────────────────────────────────
# Magento 2.4.4+ uses --opensearch-host/--opensearch-port when search engine is
# opensearch; earlier versions and elasticsearch use --elasticsearch-host/port.

echo ""
echo "=== Running setup:install ==="

if [[ "${SEARCH_TYPE}" == opensearch* ]]; then
  SEARCH_HOST_FLAG="--opensearch-host=${SEARCH_HOST}"
  SEARCH_PORT_FLAG="--opensearch-port=${SEARCH_PORT}"
else
  SEARCH_HOST_FLAG="--elasticsearch-host=${SEARCH_HOST}"
  SEARCH_PORT_FLAG="--elasticsearch-port=${SEARCH_PORT}"
fi

bin/magento setup:install \
  --base-url="${MAGENTO_BASE_URL}/" \
  --db-host="${DB_HOST}:${DB_PORT}" \
  --db-name="${DB_NAME}" \
  --db-user="${DB_USER}" \
  --db-password="${DB_PASSWORD}" \
  --search-engine="${SEARCH_TYPE}" \
  "${SEARCH_HOST_FLAG}" \
  "${SEARCH_PORT_FLAG}" \
  --cache-backend=redis \
  --cache-backend-redis-server="${CACHE_HOST}" \
  --cache-backend-redis-port="${CACHE_PORT}" \
  --cache-backend-redis-db=0 \
  --session-save=redis \
  --session-save-redis-host="${CACHE_HOST}" \
  --session-save-redis-port="${CACHE_PORT}" \
  --session-save-redis-db=1 \
  --amqp-host="${QUEUE_HOST}" \
  --amqp-port="${QUEUE_PORT}" \
  --amqp-user="${QUEUE_USER}" \
  --amqp-password="${QUEUE_PASSWORD}" \
  --amqp-virtualhost="/" \
  --admin-firstname="Admin" \
  --admin-lastname="User" \
  --admin-email="${MAGENTO_ADMIN_EMAIL}" \
  --admin-user="${MAGENTO_ADMIN_USER}" \
  --admin-password="${MAGENTO_ADMIN_PASSWORD}" \
  --backend-frontname="admin" \
  --language="en_US" \
  --currency="USD" \
  --timezone="UTC" \
  --use-rewrites=1 \
  --no-interaction \
  2>&1

echo "[OK] setup:install complete"

# ─── Post-install configuration ───────────────────────────────────────────────

echo ""
echo "=== Post-install configuration ==="

bin/magento deploy:mode:set developer --no-interaction

bin/magento module:disable Magento_TwoFactorAuth --no-interaction 2>/dev/null || \
  bin/magento module:disable MSP_TwoFactorAuth --no-interaction 2>/dev/null || \
  echo "[WARN] 2FA module not found — skipping disable" >&2

# ─── Save vendor cache (before sample data — cache key is version-only) ──────
if [[ ! -d "${VENDOR_CACHE_PATH}/vendor" ]] && [[ -w "${VENDOR_CACHE_DIR}" ]]; then
  echo ""
  echo "=== Saving vendor cache — ${VENDOR_CACHE_KEY} ==="
  mkdir -p "${VENDOR_CACHE_PATH}"
  cp -a "${MAGENTO_DIR}/vendor" "${VENDOR_CACHE_PATH}/vendor.tmp"
  mv "${VENDOR_CACHE_PATH}/vendor.tmp" "${VENDOR_CACHE_PATH}/vendor"
  echo "[OK] Vendor cache saved"
fi

# ─── Sample data ──────────────────────────────────────────────────────────────
if [[ "${INSTALL_SAMPLE_DATA}" == "1" ]]; then
  echo ""
  echo "=== Installing sample data ==="
  bin/magento sampledata:deploy --no-interaction 2>&1
  bin/magento setup:upgrade --no-interaction 2>&1
  echo "[OK] Sample data installed"
fi

echo "[OK] Installation complete"
