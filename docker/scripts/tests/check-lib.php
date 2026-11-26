<?php

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Shared by the service checks. Must run on PHP 7.4, which 2.4.2 needs.

use Magento\Framework\App\Bootstrap;
use Magento\Framework\App\DeploymentConfig;
use Magento\Framework\App\DeploymentConfig\Writer;
use Magento\Framework\App\ObjectManager;
use Magento\Framework\App\State;
use Magento\Framework\Config\File\ConfigFilePool;

const CHECK_EXIT_SKIP = 3;

function checkName(): string
{
    return preg_replace('/^check-/', '', basename($_SERVER['argv'][0] ?? 'check', '.php'));
}

set_exception_handler(function (Throwable $e) {
    checkFail(checkName(), 'uncaught ' . get_class($e) . ': ' . $e->getMessage());
});

register_shutdown_function(function () {
    $error = error_get_last();
    if ($error && in_array($error['type'], [E_ERROR, E_PARSE, E_CORE_ERROR, E_COMPILE_ERROR, E_RECOVERABLE_ERROR], true)) {
        fwrite(STDERR, '[FAIL] ' . checkName() . ": uncaught fatal error: {$error['message']} in {$error['file']}:{$error['line']}\n");
    }
});

require getcwd() . '/app/bootstrap.php';

function checkObjectManager(): ObjectManager
{
    $objectManager = Bootstrap::create(BP, $_SERVER)->getObjectManager();
    $objectManager->get(State::class)->setAreaCode('adminhtml');

    return $objectManager;
}

// PHP 8.5 deprecates $http_response_header, and http_get_last_response_headers() has
// replaced it since 8.4. The variable only exists where the request was made.
function checkHttpGet(string $url, array $http): array
{
    $body = @file_get_contents($url, false, stream_context_create(['http' => $http]));
    $variable = 'http_response_header';
    $headers = function_exists('http_get_last_response_headers') ? http_get_last_response_headers() : ($$variable ?? []);

    return [(string)$body, $headers ?? []];
}

function checkFail(string $check, string $message): void
{
    fwrite(STDERR, "[FAIL] {$check}: {$message}\n");
    exit(1);
}

// For a check that could not run, so its result says nothing about the release.
function checkError(string $check, string $message): void
{
    fwrite(STDERR, "[ERROR] {$check}: {$message}\n");
    exit(2);
}

function checkSkip(string $check, string $reason): void
{
    echo "[SKIP] {$check}: {$reason}\n";
    exit(CHECK_EXIT_SKIP);
}

function cronConsumersRunnerEnabled(ObjectManager $objectManager): bool
{
    return (bool)$objectManager->get(DeploymentConfig::class)->get('cron_consumers_runner/cron_run', true);
}

// Cron's consumers runner starts every consumer with messages waiting, so a check
// that runs its own consumer or counts what cron did has to keep it out of the way.
function setCronConsumersRunner(ObjectManager $objectManager, bool $enabled): void
{
    $objectManager->get(Writer::class)->saveConfig([
        ConfigFilePool::APP_ENV => ['cron_consumers_runner' => ['cron_run' => $enabled]],
    ]);
}

function checkProductId(ObjectManager $objectManager): int
{
    $resource = $objectManager->get(\Magento\Framework\App\ResourceConnection::class);
    $db = $resource->getConnection();
    $id = (int)$db->fetchOne(
        $db->select()->from($resource->getTableName('catalog_product_entity'), 'entity_id')->where('sku = ?', 'e2e-checkout-product')
    );
    if (!$id) {
        checkFail('setup', 'e2e-checkout-product is missing, so there is nothing to test with');
    }

    return $id;
}
