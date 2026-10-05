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
#   SEARCH_HOST_FLAG_STYLE — opensearch | elasticsearch
#   SEARCH_HOST / SEARCH_PORT
#   CACHE_HOST / CACHE_PORT
#   QUEUE_TYPE — rabbitmq (AMQP) or artemis (STOMP)
#   QUEUE_HOST / QUEUE_PORT / QUEUE_USER / QUEUE_PASSWORD
#   MAGENTO_BASE_URL — e.g. http://localhost:32768

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/vendor-cache.sh"

# A one-off sweep (CACHE_SAVE=0) keeps the shared caches from growing.
if [[ "${CACHE_SAVE:-1}" != "1" ]]; then
  export COMPOSER_CACHE_DIR=/tmp/composer-cache
fi

ensure_composer_cache_health() {
  local cache_dir="${COMPOSER_CACHE_DIR:-/composer-cache}"
  local config_path="${cache_dir}/config.json"

  mkdir -p "${cache_dir}"

  if [[ -f "${config_path}" ]] && ! jq empty "${config_path}" >/dev/null 2>&1; then
    echo "[WARN] Removing invalid Composer cache config: ${config_path}" >&2
    rm -f -- "${config_path}"
  fi
}

ensure_composer_cache_health

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
: "${SEARCH_HOST_FLAG_STYLE:=}"
: "${SEARCH_HOST:=search}"
: "${SEARCH_PORT:=9200}"
: "${CACHE_HOST:=cache}"
: "${CACHE_PORT:=6379}"
: "${QUEUE_TYPE:=rabbitmq}"
: "${QUEUE_HOST:=queue}"
: "${QUEUE_PORT:=5672}"
: "${QUEUE_USER:=magento}"
: "${QUEUE_PASSWORD:=magento}"
: "${MAGENTO_BASE_URL:=http://localhost}"
: "${MAGENTO_ADMIN_USER:=admin}"
: "${MAGENTO_ADMIN_PASSWORD:=Admin123!}"
: "${MAGENTO_ADMIN_EMAIL:=admin@example.com}"
: "${INSTALL_SAMPLE_DATA:=0}"
: "${COMPOSER_PROCESS_TIMEOUT:=1200}"

export COMPOSER_PROCESS_TIMEOUT

CREATE_PROJECT_DIR=""

cleanup_create_project_dir() {
  if [[ -n "${CREATE_PROJECT_DIR}" && -d "${CREATE_PROJECT_DIR}" ]]; then
    rm -rf "${CREATE_PROJECT_DIR}"
  fi
}

cleanup() {
  vendor_cache_release_lock
  cleanup_create_project_dir
}

trap cleanup EXIT

# ─── Version-specific Composer constraint fixes ───────────────────────────────
# Some releases ship with broken package version constraints that prevent clean
# installs. These composer require aliases pin the correct versions before
# composer install runs, so the right packages are resolved on the first pass.
apply_version_fixes() {
  case "${PRODUCT_VERSION}" in
    2.4.4)
      echo "[INFO] Applying 2.4.4 version constraint fixes"
      echo "[WORKAROUND] magento-244-composer-aliases"
      composer require "magento/security-package:1.1.3-p1 as 1.1.3" \
        --no-update --no-interaction 2>&1
      composer require "magento/inventory-metapackage:1.2.4-p1 as 1.2.4" \
        --no-update --no-interaction 2>&1
      ;;
  esac

  if uses_elasticsearch8_module; then
    echo "[INFO] Swapping magento/module-elasticsearch-7 for magento/module-elasticsearch-8"
    echo "[WORKAROUND] elasticsearch8-module"
    php -r '$f = "composer.json"; $j = json_decode(file_get_contents($f), true);
      $j["replace"]["magento/module-elasticsearch-7"] = "*";
      file_put_contents($f, json_encode($j, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES) . "\n");'
    composer require magento/module-elasticsearch-8 --no-update --no-interaction 2>&1
    if pins_old_elasticsearch_client; then
      echo "[WORKAROUND] elasticsearch8-client-alias"
      composer require "elasticsearch/elasticsearch:8.17.1 as 8.5.3" --no-update --no-interaction 2>&1
    fi
  fi
}

# Adobe supports Elasticsearch 8 on 2.4.6/2.4.7 through an add-on module that
# replaces the bundled Elasticsearch 7 one.
uses_elasticsearch8_module() {
  [[ "${PRODUCT_PACKAGE}" == "magento/project-community-edition" ]] &&
    [[ "${SEARCH_TYPE}" == "elasticsearch8" ]] &&
    [[ "${PRODUCT_VERSION}" == 2.4.6* || "${PRODUCT_VERSION}" == 2.4.7* ]]
}

# Before 2.4.6-p10 / 2.4.7-p5, core pins elasticsearch/elasticsearch to ~8.5,
# below the ^8.15 the mirrored module-elasticsearch-8 builds need.
pins_old_elasticsearch_client() {
  local patch_level="${PRODUCT_VERSION#*-p}"
  [[ "${PRODUCT_VERSION}" == *-p* ]] || patch_level=0
  case "${PRODUCT_VERSION}" in
    2.4.6*) (( patch_level < 10 )) ;;
    2.4.7*) (( patch_level < 5 )) ;;
    *) return 1 ;;
  esac
}

# True when workarounds.json lists <id> for this product and version (and PHP
# version, when the entry names any). A listed version matches itself and its
# patch releases: "2.4.4" covers "2.4.4-p13", "3.0" covers "3.0.1".
workaround_applies() {
  WORKAROUND_ID="$1" php <<'PHP'
<?php
$product = strpos(getenv('PRODUCT_PACKAGE'), 'mage-os/') === 0 ? 'mageos' : 'magento';
$version = getenv('PRODUCT_VERSION');
$registry = json_decode(file_get_contents('/scripts/workarounds.json'), true)['workarounds'] ?? [];
foreach ($registry as $w) {
    if ($w['id'] !== getenv('WORKAROUND_ID') || $w['product'] !== $product) {
        continue;
    }
    if (isset($w['php']) && !in_array(getenv('PHP_VERSION'), $w['php'], true)) {
        continue;
    }
    foreach ($w['versions'] as $v) {
        if ($version === $v || strpos($version, "$v-p") === 0 || strpos($version, "$v.") === 0) {
            exit(0);
        }
    }
}
exit(1);
PHP
}

# ─── Patch application ────────────────────────────────────────────────────────
# Applies .patch files from /scripts/patches/ to the Magento installation.
# Each one used is reported as [WORKAROUND] <id>; see workarounds.json.
# Only patches workarounds.json lists for this version are tried, and each is
# checked with `patch --dry-run` first.
apply_patch_files() {
  local patches_dir="/scripts/patches"
  [[ -d "${patches_dir}" ]] || return 0

  local applied=0 skipped=0
  for patch_file in "${patches_dir}"/*.patch; do
    [[ -f "${patch_file}" ]] || continue
    local name
    name="$(basename "${patch_file}")"
    if ! workaround_applies "${name%.patch}"; then
      echo "[INFO] Skipped patch (not listed for ${PRODUCT_VERSION}): ${name}"
      (( skipped++ )) || true
      continue
    fi
    if patch --dry-run -p1 -d "${MAGENTO_DIR}" < "${patch_file}" &>/dev/null; then
      patch -p1 -d "${MAGENTO_DIR}" < "${patch_file}" > /dev/null
      echo "[OK] Applied patch: ${name}"
      echo "[WORKAROUND] ${name%.patch}"
      (( applied++ )) || true
    elif patch -R --dry-run -p1 -d "${MAGENTO_DIR}" < "${patch_file}" &>/dev/null; then
      echo "[INFO] Patch already applied (vendor cache): ${name}"
      echo "[WORKAROUND] ${name%.patch}"
    else
      echo "[INFO] Skipped patch (not applicable): ${name}"
      (( skipped++ )) || true
    fi
  done
  echo "[INFO] Patching complete: ${applied} applied, ${skipped} skipped"
}

# Catches a vendor tree missing whole packages or required files. PSR-4 dirs are
# not checked: packages may declare dirs (e.g. tests) their dist omits.
validate_composer_autoload_files() {
  VENDOR_COMPOSER_DIR="${MAGENTO_DIR}/vendor/composer" php <<'PHP'
<?php
$dir = getenv('VENDOR_COMPOSER_DIR');
$missing = [];

if (is_file("$dir/autoload_files.php")) {
    foreach (require "$dir/autoload_files.php" as $file) {
        if (!file_exists($file)) {
            $missing[] = $file;
        }
    }
}

if (is_file("$dir/installed.php")) {
    $installed = require "$dir/installed.php";
    foreach ($installed['versions'] as $name => $package) {
        $path = $package['install_path'] ?? null;
        if ($path !== null && ($package['type'] ?? '') !== 'metapackage' && !is_dir($path)) {
            $missing[] = "$path ($name)";
        }
    }
}

foreach ($missing as $file) {
    fwrite(STDERR, "[ERROR] Missing Composer autoload file: {$file}\n");
}
exit($missing ? 1 : 0);
PHP
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
uses_elasticsearch8_module && VENDOR_CACHE_KEY+="-es8"
VENDOR_CACHE_PATH="${VENDOR_CACHE_DIR}/${VENDOR_CACHE_KEY}"

# Clear both normal files and dotfiles from previous runs before create-project.
find "${MAGENTO_DIR:?}" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +

# ─── Composer create-project ──────────────────────────────────────────────────
# Create the project in a fresh temporary directory, then move it into the
# shared web root. Some Composer/project package combinations populate the
# target directory before the create-project flow completes, which makes "."
# look non-empty and aborts the install on re-runs.
CREATE_PROJECT_DIR="$(mktemp -d /tmp/magento-create-project.XXXXXX)"

# Get composer.json + lock without downloading vendor so we can inject
# version fixes before the first composer install.
composer create-project \
  --repository-url="${MIRROR_URL}" \
  --no-interaction \
  --no-progress \
  --no-install \
  "${PRODUCT_PACKAGE}:${PRODUCT_VERSION}" \
  "${CREATE_PROJECT_DIR}" \
  2>&1

find "${MAGENTO_DIR}" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
find "${CREATE_PROJECT_DIR}" -mindepth 1 -maxdepth 1 -exec mv -- {} "${MAGENTO_DIR}/" \;
rmdir "${CREATE_PROJECT_DIR}"
CREATE_PROJECT_DIR=""

cd "${MAGENTO_DIR}"

apply_version_fixes

if [[ -d "${VENDOR_CACHE_PATH}/vendor" ]]; then
  echo ""
  echo "=== Vendor cache hit — ${VENDOR_CACHE_KEY} ==="

  echo "[INFO] Restoring vendor from cache"
  if ! vendor_cache_restore "${VENDOR_CACHE_PATH}" "${MAGENTO_DIR}"; then
    restore_status=$?
    if [[ "${restore_status}" -ne 1 ]]; then
      echo "[ERROR] Failed to restore vendor cache (${restore_status})" >&2
      exit "${restore_status}"
    fi
  fi

  if [[ -d "${MAGENTO_DIR}/vendor" ]]; then
    rm -rf "${MAGENTO_DIR}/vendor/magento/magento2-base" \
           "${MAGENTO_DIR}/vendor/mage-os/magento2-base"
  fi

  if ! validate_composer_autoload_files; then
    echo "[WARN] Vendor cache ${VENDOR_CACHE_KEY} is incomplete — invalidating and reinstalling from scratch" >&2
    rm -rf "${MAGENTO_DIR}/vendor"
    vendor_cache_invalidate "${VENDOR_CACHE_PATH}" || true
  fi
else
  echo ""
  echo "=== Installing ${PRODUCT_PACKAGE}:${PRODUCT_VERSION} (no vendor cache) ==="
fi

composer install \
  --no-interaction \
  --no-progress \
  2>&1

validate_composer_autoload_files

apply_patch_files

echo "[OK] Composer install complete"

# ─── setup:install ────────────────────────────────────────────────────────────
# Some Magento releases expose OpenSearch via the opensearch search engine but
# still require the legacy --elasticsearch-host/port options.

uses_legacy_magento_opensearch_install() {
  [[ "${PRODUCT_PACKAGE}" == "magento/project-community-edition" ]] || return 1
  [[ "${SEARCH_TYPE}" == opensearch* ]] || return 1

  case "${PRODUCT_VERSION}" in
    2.4.2*|2.4.3*|2.4.4*|2.4.5*)
      return 0
      ;;
  esac

  return 1
}

echo ""
echo "=== Running setup:install ==="

if uses_legacy_magento_opensearch_install; then
  echo "[INFO] Using legacy Elasticsearch 7 install flags for ${PRODUCT_VERSION} with ${SEARCH_HOST}:${SEARCH_PORT}"
  SEARCH_TYPE="elasticsearch7"
  SEARCH_HOST_FLAG_STYLE="elasticsearch"
fi

if [[ -z "${SEARCH_HOST_FLAG_STYLE}" ]]; then
  if [[ "${SEARCH_TYPE}" == opensearch* ]]; then
    SEARCH_HOST_FLAG_STYLE="opensearch"
  else
    SEARCH_HOST_FLAG_STYLE="elasticsearch"
  fi
fi

if [[ "${SEARCH_HOST_FLAG_STYLE}" == "opensearch" ]]; then
  SEARCH_HOST_FLAG="--opensearch-host=${SEARCH_HOST}"
  SEARCH_PORT_FLAG="--opensearch-port=${SEARCH_PORT}"
else
  SEARCH_HOST_FLAG="--elasticsearch-host=${SEARCH_HOST}"
  SEARCH_PORT_FLAG="--elasticsearch-port=${SEARCH_PORT}"
fi

if [[ "${QUEUE_TYPE}" == "artemis" ]]; then
  QUEUE_FLAGS=(
    --queue-default-connection=stomp
    --stomp-host="${QUEUE_HOST}"
    --stomp-port="${QUEUE_PORT}"
    --stomp-user="${QUEUE_USER}"
    --stomp-password="${QUEUE_PASSWORD}"
  )
else
  QUEUE_FLAGS=(
    --amqp-host="${QUEUE_HOST}"
    --amqp-port="${QUEUE_PORT}"
    --amqp-user="${QUEUE_USER}"
    --amqp-password="${QUEUE_PASSWORD}"
    --amqp-virtualhost="/"
  )
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
  "${QUEUE_FLAGS[@]}" \
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

disable_optional_admin_modules() {
  local modules=()

  for module in \
    Magento_AdminAdobeImsTwoFactorAuth \
    Magento_AdminAnalytics \
    Magento_PageBuilderAdminAnalytics \
    Magento_TwoFactorAuth \
    MSP_TwoFactorAuth
  do
    local status_output
    status_output="$(bin/magento module:status "${module}" 2>&1 || true)"
    if [[ "${status_output}" != *"does not exist"* ]]; then
      modules+=("${module}")
    fi
  done

  if [[ "${#modules[@]}" -eq 0 ]]; then
    echo "[WARN] Optional admin modules not found — skipping disable" >&2
    return 0
  fi

  # module:disable clears generated classes and can fail on partially populated
  # trees, so clear them up front before disabling the known admin-only modules
  # that interfere with deterministic local E2E flows.
  rm -rf generated/code/* generated/metadata/* 2>/dev/null || true
  bin/magento module:disable "${modules[@]}" --no-interaction
}

disable_optional_admin_modules

# ─── Save vendor cache (before sample data — cache key is version-only) ──────
if [[ "${CACHE_SAVE:-1}" == "1" ]] && [[ ! -d "${VENDOR_CACHE_PATH}/vendor" ]] && [[ -w "${VENDOR_CACHE_DIR}" ]]; then
  echo ""
  echo "=== Saving vendor cache — ${VENDOR_CACHE_KEY} ==="
  if vendor_cache_save "${VENDOR_CACHE_PATH}" "${MAGENTO_DIR}"; then
    echo "[OK] Vendor cache saved"
  else
    save_status=$?
    if [[ "${save_status}" -eq 2 ]]; then
      echo "[INFO] Vendor cache already populated by another run"
    else
      echo "[ERROR] Failed to save vendor cache (${save_status})" >&2
      exit "${save_status}"
    fi
  fi
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
