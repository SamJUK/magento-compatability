<?php

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Proves Varnish caches and Magento purges it, once check-varnish.sh has pointed
// Magento at Varnish and the runner has loaded the release's VCL into it.
// Magento's VCL reports HIT, MISS or UNCACHEABLE in X-Magento-Cache-Debug.

use Magento\Framework\App\Config\ScopeConfigInterface;

require __DIR__ . '/check-lib.php';

$objectManager = checkObjectManager();
$config = $objectManager->get(ScopeConfigInterface::class);

if ((int)$config->getValue('system/full_page_cache/caching_application') !== 2) {
    checkFail('varnish', 'full page cache is not set to Varnish');
}

// Every VCL probes this URL, and a backend that fails its probe is marked sick, so
// Varnish would answer 503 to everything.
[, $probeHeaders] = checkHttpGet('http://webserver/health_check.php', ['ignore_errors' => true, 'timeout' => 30]);
$probeStatus = preg_match('#^HTTP/\S+ (\d+)#', $probeHeaders[0] ?? '', $probeMatch) ? (int)$probeMatch[1] : 0;
if ($probeStatus === 0) {
    checkError('varnish', 'the web server did not answer /health_check.php');
}
if ($probeStatus !== 200) {
    checkFail('varnish', "the health probe /health_check.php answers {$probeStatus}, so Varnish would mark the backend sick");
}

$baseUrl = parse_url((string)$config->getValue('web/unsecure/base_url'));
$host = $baseUrl['host'] . (isset($baseUrl['port']) ? ':' . $baseUrl['port'] : '');

$get = function (string $path) use ($host) {
    [$body, $lines] = checkHttpGet('http://varnish' . $path, [
        'header' => "Host: {$host}\r\nAccept-Encoding: identity\r\n",
        'ignore_errors' => true,
        'follow_location' => 0,
        'timeout' => 90,
    ]);
    $headers = [];
    $status = 0;
    foreach ($lines as $line) {
        if (preg_match('#^HTTP/\S+ (\d+)#', $line, $m)) {
            $status = (int)$m[1];
        } elseif (strpos($line, ':') !== false) {
            [$name, $value] = explode(':', $line, 2);
            $headers[strtolower($name)] = trim($value);
        }
    }

    if ($status === 0) {
        checkError('varnish', "no answer from Varnish for {$path}");
    }
    $debug = $headers['x-magento-cache-debug'] ?? '';

    return ['status' => $status, 'hit' => $debug === 'HIT', 'miss' => $debug === 'MISS', 'body' => (string)$body, 'headers' => $headers];
};
$describe = function (array $r) {
    return "status {$r['status']}, x-magento-cache-debug '" . ($r['headers']['x-magento-cache-debug'] ?? '') . "'";
};

$product = '/e2e-checkout-product.html';

$first = $get($product);
$second = $get($product);
if ($first['status'] !== 200 || $second['status'] !== 200) {
    checkFail('varnish', "the product page did not answer 200 ({$describe($first)}; {$describe($second)})");
}
if ($first['hit']) {
    checkError('varnish', "the cache was not empty, the first GET was already a hit ({$describe($first)})");
}
if (!$second['hit']) {
    checkFail('varnish', "the second GET of the product page was not a cache hit ({$describe($second)})");
}
echo "[OK] varnish: the second GET of a cacheable page is a hit ({$describe($second)})\n";

// Pages with private data must pass through, or customers would share them.
foreach (['/checkout/cart/', '/customer/account/login/'] as $path) {
    $responses = [$get($path), $get($path)];
    foreach ($responses as $response) {
        if ($response['status'] !== 200) {
            checkFail('varnish', "{$path} did not answer 200 ({$describe($response)})");
        }
    }
    foreach ($responses as $response) {
        $debug = $response['headers']['x-magento-cache-debug'] ?? '';
        if ($debug === 'HIT') {
            checkFail('varnish', "{$path} was served from the cache ({$describe($response)})");
        }
        if (!in_array($debug, ['MISS', 'UNCACHEABLE'], true)) {
            checkError('varnish', "{$path} came back with X-Magento-Cache-Debug '{$debug}', so a cached copy could not be told from a fresh one");
        }
    }
}
echo "[OK] varnish: the cart and login pages are not served from the cache\n";

$marker = 'varnishmarker' . time();
if (strpos($second['body'], $marker) !== false) {
    checkFail('varnish', 'the marker was on the page before the product was saved');
}
$repository = $objectManager->get(\Magento\Catalog\Api\ProductRepositoryInterface::class);
$item = $repository->get('e2e-checkout-product', true, 0, true);
$item->setDescription($marker);
$repository->save($item);

$after = $get($product);
if ($after['status'] !== 200) {
    checkFail('varnish', "the product page did not answer 200 after the product was saved ({$describe($after)})");
}
if ($after['hit'] || strpos($after['body'], $marker) === false) {
    checkFail('varnish', "the product page was not purged when the product was saved ({$describe($after)}, marker " . (strpos($after['body'], $marker) === false ? 'absent' : 'present') . ')');
}
$again = $get($product);
if ($again['status'] !== 200) {
    checkFail('varnish', "the product page did not answer 200 after the purge ({$describe($again)})");
}
if (!$again['hit'] || strpos($again['body'], $marker) === false) {
    checkFail('varnish', "the page was not cached again with the saved marker after the purge ({$describe($again)}, marker " . (strpos($again['body'], $marker) === false ? 'absent' : 'present') . ')');
}

echo "[OK] varnish: saving the product purged the page, and the fresh page is cached again\n";
