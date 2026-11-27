#!/usr/bin/env bash
# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0


# Needs util-linux flock, so run it in the PHP image:
#   docker run --rm -v "$PWD/docker/scripts:/scripts:ro" m2test/php:8.3 bash /scripts/tests/vendor-cache-lock.sh

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

test_lock_dies_with_its_holder() {
  local cache_path="${TMP_ROOT}/dead-holder-cache"

  # exec, so the pid we kill is the process holding the lock.
  LIB_PATH="${LIB_PATH}" bash -c '
    source "${LIB_PATH}"
    vendor_cache_acquire_lock "'"${cache_path}"'"
    exec sleep 60
  ' &
  local holder_pid=$!
  sleep 1

  local status=0
  VENDOR_CACHE_LOCK_WAIT_SECONDS=1 vendor_cache_acquire_lock "${cache_path}" 2>/dev/null || status=$?
  assert_eq "${status}" "1" "a live holder should block the lock"

  kill -9 "${holder_pid}"
  wait "${holder_pid}" 2>/dev/null || true

  VENDOR_CACHE_LOCK_WAIT_SECONDS=5 vendor_cache_acquire_lock "${cache_path}"
  vendor_cache_release_lock
}

test_save_then_restore_round_trips_through_the_archive() {
  local cache_path="${TMP_ROOT}/round-trip-cache"
  local source_dir="${TMP_ROOT}/round-trip-source"
  local target_dir="${TMP_ROOT}/round-trip-target"
  mkdir -p "${source_dir}/vendor/acme/pkg" "${source_dir}/app/etc"
  printf 'vendored\n' > "${source_dir}/vendor/acme/pkg/file.txt"
  printf 'registration\n' > "${source_dir}/app/etc/NonComposerComponentRegistration.php"

  vendor_cache_save "${cache_path}" "${source_dir}"

  assert_eq "$(ls "${cache_path}")" "${VENDOR_CACHE_ARCHIVE}" "save should leave only the archive"
  vendor_cache_restore "${cache_path}" "${target_dir}"
  assert_eq "$(cat "${target_dir}/vendor/acme/pkg/file.txt")" "vendored" "vendor should be restored"
  assert_eq "$(cat "${target_dir}/app/etc/NonComposerComponentRegistration.php")" "registration" "registration file should be restored"
}

test_restore_converts_a_directory_entry() {
  local cache_path="${TMP_ROOT}/legacy-cache"
  local target_dir="${TMP_ROOT}/legacy-target"
  mkdir -p "${cache_path}/vendor/acme/pkg" "${cache_path}/app/etc"
  printf 'legacy\n' > "${cache_path}/vendor/acme/pkg/file.txt"
  printf 'registration\n' > "${cache_path}/app/etc/NonComposerComponentRegistration.php"

  vendor_cache_restore "${cache_path}" "${target_dir}"

  assert_eq "$(ls "${cache_path}")" "${VENDOR_CACHE_ARCHIVE}" "the directory entry should be replaced by an archive"
  assert_eq "$(cat "${target_dir}/vendor/acme/pkg/file.txt")" "legacy" "vendor should be restored"
  assert_eq "$(cat "${target_dir}/app/etc/NonComposerComponentRegistration.php")" "registration" "registration file should be restored"
}

test_corrupt_archive_restores_as_a_miss() {
  local cache_path="${TMP_ROOT}/corrupt-cache"
  local status=0
  mkdir -p "${cache_path}"
  printf 'not zstd' > "${cache_path}/${VENDOR_CACHE_ARCHIVE}"

  vendor_cache_restore "${cache_path}" "${TMP_ROOT}/corrupt-target" 2>/dev/null || status=$?

  assert_eq "${status}" "1" "a corrupt archive should restore as a miss"
}

test_save_waits_and_skips_when_cache_is_populated
test_restore_waits_until_cache_is_ready
test_save_then_restore_round_trips_through_the_archive
test_restore_converts_a_directory_entry
test_corrupt_archive_restores_as_a_miss
test_lock_dies_with_its_holder

echo "vendor cache lock tests passed"
