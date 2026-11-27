#!/usr/bin/env bash
# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0

# Points Magento at Varnish: full page cache set to Varnish, and PURGE sent to it
# when a product is saved. The runner then loads
# tests/varnish.vcl into the varnish container and runs check-varnish.php.

set -euo pipefail

: "${MAGENTO_DIR:=/var/www/html}"

cd "${MAGENTO_DIR}"

bin/magento config:set system/full_page_cache/caching_application 2
bin/magento config:set system/full_page_cache/varnish/backend_host webserver
bin/magento config:set system/full_page_cache/varnish/backend_port 80
bin/magento config:set system/full_page_cache/varnish/access_list php-fpm
bin/magento setup:config:set --no-interaction --http-cache-hosts=varnish:80
bin/magento cache:flush >/dev/null
echo "[OK] varnish: full page cache is set to Varnish"
