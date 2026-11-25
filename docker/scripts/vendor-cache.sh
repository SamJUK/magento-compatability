#!/usr/bin/env bash
# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0


# Shared vendor cache helpers for Magento install runs.
#
# The cache volume is intentionally shared across combinations, so these
# helpers serialize restore/save access per cache key to avoid partial copies
# and competing writes when multiple runs hit the same PHP/package/version.

: "${VENDOR_CACHE_LOCK_WAIT_SECONDS:=1800}"

VENDOR_CACHE_LOCK_FD=""

# Each entry is one zstd archive of vendor/ plus NonComposerComponentRegistration.php.
# Older entries are plain directories and are converted on their first restore.
VENDOR_CACHE_ARCHIVE="vendor.tar.zst"
# Level 12 with a 128MB window is within 4% of level 19 at a thirteenth of the time.
: "${VENDOR_CACHE_ZSTD_LEVEL:=12}"

vendor_cache_exists() {
  [[ -f "$1/${VENDOR_CACHE_ARCHIVE}" || -d "$1/vendor" ]]
}

vendor_cache_require_zstd() {
  command -v zstd >/dev/null && return 0
  echo "[ERROR] zstd is missing from the PHP image, rebuild it with docker compose build" >&2
  return 1
}

# Archives vendor/ (and the registration file, if present) from source_dir into cache_path.
vendor_cache_write_archive() {
  local source_dir="$1"
  local cache_path="$2"
  local tmp_file="${cache_path}/${VENDOR_CACHE_ARCHIVE}.tmp.$$"
  local files=(vendor)

  [[ -f "${source_dir}/app/etc/NonComposerComponentRegistration.php" ]] \
    && files+=(app/etc/NonComposerComponentRegistration.php)

  mkdir -p "${cache_path}"
  (set -o pipefail; tar -C "${source_dir}" -cf - "${files[@]}" \
    | zstd -q -f -T0 --long=27 "-${VENDOR_CACHE_ZSTD_LEVEL}" -o "${tmp_file}") \
    && mv "${tmp_file}" "${cache_path}/${VENDOR_CACHE_ARCHIVE}" \
    || { rm -f "${tmp_file}"; return 1; }
}

# The lock is a kernel flock on <entry>.flock, so it dies with its holder and
# never needs stale detection. Needs util-linux flock for -w.
vendor_cache_release_lock() {
  if [[ -n "${VENDOR_CACHE_LOCK_FD}" ]]; then
    exec {VENDOR_CACHE_LOCK_FD}>&-
    VENDOR_CACHE_LOCK_FD=""
  fi
}

vendor_cache_acquire_lock() {
  local lock_file="$1.flock"

  if ! flock --version 2>/dev/null | grep -q util-linux; then
    echo "[ERROR] util-linux flock is missing from the PHP image, rebuild it with docker compose build" >&2
    return 1
  fi

  mkdir -p "$(dirname "${lock_file}")"
  exec {VENDOR_CACHE_LOCK_FD}>>"${lock_file}" || { VENDOR_CACHE_LOCK_FD=""; return 1; }
  flock -n "${VENDOR_CACHE_LOCK_FD}" && return 0

  echo "[INFO] Waiting for vendor cache lock: ${lock_file}" >&2
  flock -w "${VENDOR_CACHE_LOCK_WAIT_SECONDS}" "${VENDOR_CACHE_LOCK_FD}" && return 0

  echo "[ERROR] Timed out waiting for vendor cache lock: ${lock_file}" >&2
  vendor_cache_release_lock
  return 1
}

vendor_cache_restore() {
  local cache_path="$1"
  local target_dir="$2"
  local status=0

  vendor_cache_require_zstd || return 2
  vendor_cache_acquire_lock "${cache_path}" || return 2

  if [[ -d "${cache_path}/vendor" && ! -f "${cache_path}/${VENDOR_CACHE_ARCHIVE}" ]]; then
    echo "[INFO] Converting vendor cache entry to ${VENDOR_CACHE_ARCHIVE}"
    vendor_cache_write_archive "${cache_path}" "${cache_path}" || status=2
  fi
  if (( status == 0 )) && [[ -f "${cache_path}/${VENDOR_CACHE_ARCHIVE}" ]]; then
    rm -rf -- "${cache_path}/vendor" "${cache_path}/app"
    mkdir -p "${target_dir}"
    (set -o pipefail; zstd -q -d -c --long=27 "${cache_path}/${VENDOR_CACHE_ARCHIVE}" | tar -C "${target_dir}" -xf -) || status=1
  elif (( status == 0 )); then
    status=1
  fi

  vendor_cache_release_lock
  return "${status}"
}

vendor_cache_save() {
  local cache_path="$1"
  local source_dir="$2"
  local status=0

  if [[ ! -d "${source_dir}/vendor" ]]; then
    return 1
  fi

  vendor_cache_require_zstd || return 3
  vendor_cache_acquire_lock "${cache_path}" || return 3
  if vendor_cache_exists "${cache_path}"; then
    vendor_cache_release_lock
    return 2
  fi

  vendor_cache_write_archive "${source_dir}" "${cache_path}" || status=3
  vendor_cache_release_lock
  return "${status}"
}

vendor_cache_invalidate() {
  local cache_path="$1"

  vendor_cache_acquire_lock "${cache_path}" || return 4
  rm -rf -- "${cache_path}"
  vendor_cache_release_lock
  return 0
}
