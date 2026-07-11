#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../vendor-cache.sh"

TMP_ROOT="$(mktemp -d)"
LIB_PATH="${SCRIPT_DIR}/../vendor-cache.sh"

cleanup() {
  vendor_cache_release_lock
  rm -rf "${TMP_ROOT}"
}

trap cleanup EXIT

assert_eq() {
  local got="$1"
  local want="$2"
  local message="$3"
  if [[ "${got}" != "${want}" ]]; then
    echo "assert_eq failed: ${message}: got=${got} want=${want}" >&2
    exit 1
  fi
}

assert_ge() {
  local got="$1"
  local want="$2"
  local message="$3"
  if (( got < want )); then
    echo "assert_ge failed: ${message}: got=${got} want>=${want}" >&2
    exit 1
  fi
}

run_in_bash() {
  local code="$1"
  LIB_PATH="${LIB_PATH}" bash -lc "${code}"
}

test_save_waits_and_skips_when_cache_is_populated() {
  local cache_path="${TMP_ROOT}/save-cache"
  local source_dir="${TMP_ROOT}/save-source"
  mkdir -p "${source_dir}/vendor/phpmd/phpmd/src/main/php/PHPMD/Node"
  printf 'from-second\n' > "${source_dir}/vendor/phpmd/phpmd/src/main/php/PHPMD/source.txt"

  run_in_bash '
    set -euo pipefail
    source "${LIB_PATH}"
    cache_path="'"${cache_path}"'"
    vendor_cache_acquire_lock "${cache_path}"
    mkdir -p "${cache_path}/vendor/phpmd/phpmd/src/main/php/PHPMD/Node"
    printf "from-first\n" > "${cache_path}/vendor/phpmd/phpmd/src/main/php/PHPMD/source.txt"
    sleep 2
    vendor_cache_release_lock
  ' &
  local holder_pid=$!

  sleep 1

  local started_at status elapsed
  started_at="$(date +%s)"
  if vendor_cache_save "${cache_path}" "${source_dir}"; then
    status=0
  else
    status=$?
  fi
  elapsed=$(( $(date +%s) - started_at ))

  wait "${holder_pid}"

  assert_eq "${status}" "2" "save should skip when another run already populated the cache"
  assert_ge "${elapsed}" "1" "save should wait for the lock holder to finish"
  assert_eq "$(cat "${cache_path}/vendor/phpmd/phpmd/src/main/php/PHPMD/source.txt")" "from-first" "existing cache contents should be preserved"
}

test_restore_waits_until_cache_is_ready() {
  local cache_path="${TMP_ROOT}/restore-cache"
  local target_dir="${TMP_ROOT}/restore-target"
  mkdir -p "${target_dir}"

  run_in_bash '
    set -euo pipefail
    source "${LIB_PATH}"
    cache_path="'"${cache_path}"'"
    vendor_cache_acquire_lock "${cache_path}"
    sleep 2
    mkdir -p "${cache_path}/vendor/phpmd/phpmd/src/main/php/PHPMD/Node"
    printf "from-cache\n" > "${cache_path}/vendor/phpmd/phpmd/src/main/php/PHPMD/source.txt"
    vendor_cache_release_lock
  ' &
  local holder_pid=$!

  sleep 1

  local started_at status elapsed
  started_at="$(date +%s)"
  if vendor_cache_restore "${cache_path}" "${target_dir}"; then
    status=0
  else
    status=$?
  fi
  elapsed=$(( $(date +%s) - started_at ))

  wait "${holder_pid}"

  assert_eq "${status}" "0" "restore should succeed after the cache appears"
  assert_ge "${elapsed}" "1" "restore should wait for the lock holder to finish"
  assert_eq "$(cat "${target_dir}/vendor/phpmd/phpmd/src/main/php/PHPMD/source.txt")" "from-cache" "restored cache contents should match"
}

test_stale_lock_is_removed() {
  local cache_path="${TMP_ROOT}/stale-cache"
  local stale_lock="${cache_path}.lock"
  mkdir -p "${stale_lock}"
  printf '1\n' > "${stale_lock}/acquired_at"

  local started_at
  started_at="$(date +%s)"
  VENDOR_CACHE_LOCK_STALE_SECONDS=1 vendor_cache_acquire_lock "${cache_path}"
  local elapsed=$(( $(date +%s) - started_at ))

  assert_ge "${elapsed}" "0" "stale lock acquisition should not fail"
  vendor_cache_release_lock
  if [[ -d "${stale_lock}" ]]; then
    echo "stale lock was not cleaned up" >&2
    exit 1
  fi
}

test_save_waits_and_skips_when_cache_is_populated
test_restore_waits_until_cache_is_ready
test_stale_lock_is_removed

echo "vendor cache lock tests passed"
