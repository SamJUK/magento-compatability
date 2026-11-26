#!/usr/bin/env bash
# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0

# Runs one service check inside the php-fpm container and hands Magento's
# runtime directories back to the web user, whatever the check's exit code.
# Exit 3 means the release has nothing to test; the runner records a skip.

: "${MAGENTO_DIR:=/var/www/html}"

cd "${MAGENTO_DIR}" || exit 1

start=$(date +%s)
timeout 870 "$@"
exit_code=$?

if [[ "${exit_code}" == 124 || "${exit_code}" == 143 ]]; then
  name=$(basename "${2:-check}")
  name=${name#check-}
  echo "[ERROR] ${name%.*}: timed out after 870s"
fi

chown -R www-data:www-data var generated pub 2>/dev/null ||
  chmod -R 777 var generated pub 2>/dev/null || true

echo "[check] exit ${exit_code} after $(( $(date +%s) - start ))s"
exit "${exit_code}"
