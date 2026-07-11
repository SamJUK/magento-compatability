#!/usr/bin/env bash

# Shared vendor cache helpers for Magento install runs.
#
# The cache volume is intentionally shared across combinations, so these
# helpers serialize restore/save access per cache key to avoid partial copies
# and competing writes when multiple runs hit the same PHP/package/version.

: "${VENDOR_CACHE_LOCK_WAIT_SECONDS:=1800}"
: "${VENDOR_CACHE_LOCK_STALE_SECONDS:=3600}"

VENDOR_CACHE_HELD_LOCK_DIR=""

vendor_cache_copy_tree() {
  local source_root="$1"
  local source_name="$2"
  local target_root="$3"

  mkdir -p "${target_root}"
  tar -C "${source_root}" -cf - "${source_name}" | tar -C "${target_root}" -xf -
}

vendor_cache_release_lock() {
  if [[ -n "${VENDOR_CACHE_HELD_LOCK_DIR}" ]]; then
    rm -rf -- "${VENDOR_CACHE_HELD_LOCK_DIR}"
    VENDOR_CACHE_HELD_LOCK_DIR=""
  fi
}

vendor_cache_acquire_lock() {
  local cache_path="$1"
  local lock_dir="${cache_path}.lock"
  local started_at now acquired_at
  local wait_logged=0

  started_at="$(date +%s)"

  while ! mkdir "${lock_dir}" 2>/dev/null; do
    now="$(date +%s)"
    acquired_at=0
    if [[ -f "${lock_dir}/acquired_at" ]]; then
      acquired_at="$(cat "${lock_dir}/acquired_at" 2>/dev/null || echo 0)"
    fi

    if [[ "${acquired_at}" =~ ^[0-9]+$ ]] && (( acquired_at > 0 )) && (( now - acquired_at > VENDOR_CACHE_LOCK_STALE_SECONDS )); then
      echo "[WARN] Removing stale vendor cache lock: ${lock_dir}" >&2
      rm -rf -- "${lock_dir}"
      continue
    fi

    if (( wait_logged == 0 )); then
      echo "[INFO] Waiting for vendor cache lock: ${lock_dir}" >&2
      wait_logged=1
    fi

    if (( now - started_at >= VENDOR_CACHE_LOCK_WAIT_SECONDS )); then
      echo "[ERROR] Timed out waiting for vendor cache lock: ${lock_dir}" >&2
      return 1
    fi

    sleep 1
  done

  printf '%s\n' "$(date +%s)" > "${lock_dir}/acquired_at"
  printf '%s\n' "${HOSTNAME:-unknown}" > "${lock_dir}/owner"
  printf '%s\n' "$$" > "${lock_dir}/pid"

  VENDOR_CACHE_HELD_LOCK_DIR="${lock_dir}"
}

vendor_cache_restore() {
  local cache_path="$1"
  local target_dir="$2"

  vendor_cache_acquire_lock "${cache_path}" || return 2
  if [[ ! -d "${cache_path}/vendor" ]]; then
    vendor_cache_release_lock
    return 1
  fi

  vendor_cache_copy_tree "${cache_path}" "vendor" "${target_dir}"
  vendor_cache_release_lock
  return 0
}

vendor_cache_save() {
  local cache_path="$1"
  local source_dir="$2"
  local tmp_path=""

  if [[ ! -d "${source_dir}/vendor" ]]; then
    return 1
  fi

  vendor_cache_acquire_lock "${cache_path}" || return 3
  if [[ -d "${cache_path}/vendor" ]]; then
    vendor_cache_release_lock
    return 2
  fi

  mkdir -p "${cache_path}"
  tmp_path="${cache_path}/vendor.tmp.$$"
  rm -rf -- "${tmp_path}"
  vendor_cache_copy_tree "${source_dir}" "vendor" "${tmp_path}"
  mv "${tmp_path}/vendor" "${cache_path}/vendor"
  rmdir "${tmp_path}"

  vendor_cache_release_lock
  return 0
}
