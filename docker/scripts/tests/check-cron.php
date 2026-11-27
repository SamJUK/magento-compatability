<?php

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Proves cron and the schedule-mode indexers work: puts two indexers on "update by
// schedule" (database triggers fill their changelogs), changes the seeded product,
// runs cron and checks the change reached the price index and the search index.

require __DIR__ . '/check-lib.php';

const INDEXERS = ['catalog_product_price', 'catalogsearch_fulltext'];
const CRON_RUNS = 2;

$objectManager = checkObjectManager();
$resource = $objectManager->get(\Magento\Framework\App\ResourceConnection::class);
$db = $resource->getConnection();
$table = function (string $name) use ($resource) {
    return $resource->getTableName($name);
};
$productId = checkProductId($objectManager);

$registry = $objectManager->get(\Magento\Framework\Indexer\IndexerRegistry::class);
$originalModes = [];
foreach (INDEXERS as $id) {
    $originalModes[$id] = $registry->get($id)->isScheduled();
}
// The release default differs between versions, so put them back as found.
register_shutdown_function(function () use ($registry, $originalModes) {
    foreach ($originalModes as $id => $scheduled) {
        $registry->get($id)->setScheduled($scheduled);
    }
});
// Cron's consumers runner would spawn every consumer that has messages waiting.
$cronRunnerWasEnabled = cronConsumersRunnerEnabled($objectManager);
setCronConsumersRunner($objectManager, false);
register_shutdown_function('setCronConsumersRunner', $objectManager, $cronRunnerWasEnabled);

foreach (INDEXERS as $id) {
    $registry->get($id)->setScheduled(true);
}

$config = $objectManager->get(\Magento\Framework\App\Config\ScopeConfigInterface::class);
$engine = (string)$config->getValue('catalog/search/engine');
$storeId = (int)$objectManager->get(\Magento\Store\Model\StoreManagerInterface::class)->getDefaultStoreView()->getId();
$searchCount = function (string $term) use ($config, $engine, $storeId) {
    $host = $config->getValue("catalog/search/{$engine}_server_hostname");
    $port = $config->getValue("catalog/search/{$engine}_server_port");
    $prefix = $config->getValue("catalog/search/{$engine}_index_prefix");
    $body = @file_get_contents("http://{$host}:{$port}/{$prefix}_product_{$storeId}/_count?q=" . urlencode($term));
    $decoded = json_decode((string)$body, true);
    if (!is_array($decoded) || !isset($decoded['count'])) {
        checkError('cron', "could not count documents in the {$engine} index: " . var_export($body, true));
    }

    return (int)$decoded['count'];
};
$priceRows = function () use ($db, $table, $productId) {
    return $db->fetchAll(
        $db->select()->from($table('catalog_product_index_price'), ['website_id', 'customer_group_id', 'price'])
            ->where('entity_id = ?', $productId)->order(['website_id', 'customer_group_id'])
    );
};
$priceKeys = function (array $rows) {
    return array_map(function ($row) {
        return "{$row['website_id']}/{$row['customer_group_id']}";
    }, $rows);
};
$latestVersion = function (string $view) use ($db, $table) {
    return $db->isTableExists($table($view . '_cl'))
        ? (int)$db->fetchOne($db->select()->from($table($view . '_cl'), 'MAX(version_id)'))
        : 0;
};
$pendingVersions = function (string $view, int $since) use ($db, $table, $productId) {
    if (!$db->isTableExists($table($view . '_cl'))) {
        return 0;
    }
    $current = (int)$db->fetchOne($db->select()->from($table('mview_state'), 'version_id')->where('view_id = ?', $view));

    return (int)$db->fetchOne(
        $db->select()->from($table($view . '_cl'), 'COUNT(*)')->where('entity_id = ?', $productId)->where('version_id > ?', max($current, $since))
    );
};

$versionsBefore = [];
foreach (INDEXERS as $view) {
    $versionsBefore[$view] = $latestVersion($view);
}
$priceKeysBefore = $priceKeys($priceRows());
if (!$priceKeysBefore) {
    checkError('cron', 'the price index has no rows for the product before the change, so there is nothing to compare');
}

$marker = 'cronmarker' . time();
$price = round(11.11 + (time() % 1000) / 100, 2);
$product = $objectManager->get(\Magento\Catalog\Api\ProductRepositoryInterface::class)->get('e2e-checkout-product', true, 0, true);
$product->setPrice($price)->setDescription($marker);
$objectManager->get(\Magento\Catalog\Api\ProductRepositoryInterface::class)->save($product);

// Without these the check would pass on an index that was updated synchronously.
$savedVersion = [];
foreach (INDEXERS as $view) {
    if ($pendingVersions($view, $versionsBefore[$view]) === 0) {
        checkFail('cron', "saving the product added nothing to the {$view} changelog, so the triggers did not fire");
    }
    $savedVersion[$view] = (int)$db->fetchOne(
        $db->select()->from($table($view . '_cl'), 'MAX(version_id)')->where('entity_id = ?', $productId)
    );
}
$priceReached = array_filter($priceRows(), function ($row) use ($price) {
    return abs((float)$row['price'] - $price) < 0.001;
});
if ($priceReached || $searchCount($marker) !== 0) {
    checkError('cron', 'the indexes already held the change before cron ran');
}
// A reindex-all-invalid cron job would rebuild an invalid index from scratch and pass for the wrong reason.
foreach (INDEXERS as $id) {
    $indexer = $registry->get($id);
    if (!$indexer->isValid()) {
        checkError('cron', "the {$id} indexer is {$indexer->getStatus()} before cron ran, not valid");
    }
}
echo "[OK] cron: product saved, changelogs hold the change and the indexes do not yet\n";

// Due now, so the first cron:run runs this row; cron generates no duplicate for its minute.
$db->insert($table('cron_schedule'), [
    'job_code' => 'indexer_update_all_views',
    'status' => 'pending',
    'created_at' => new \Zend_Db_Expr('NOW()'),
    'scheduled_at' => new \Zend_Db_Expr("DATE_FORMAT(NOW(), '%Y-%m-%d %H:%i:00')"),
]);
$scheduleId = (int)$db->lastInsertId($table('cron_schedule'));
$jobRow = function () use ($db, $table, $scheduleId) {
    return $db->fetchRow(
        $db->select()->from($table('cron_schedule'), ['status', 'messages'])->where('schedule_id = ?', $scheduleId)
    );
};
$updated = false;
for ($run = 1; $run <= CRON_RUNS && !$updated; $run++) {
    passthru('timeout 300 php ' . BP . '/bin/magento cron:run 2>&1', $exitCode);
    if ($exitCode !== 0) {
        checkError('cron', "cron:run exited with {$exitCode} (15 or 143 means a signal stopped it, probably the 300 second timeout)");
    }
    // cron:run starts each group as a background process and does not wait for it.
    for ($waited = 0; $waited < 60 && !$updated; $waited++) {
        $row = $jobRow();
        if (in_array($row['status'], ['error', 'missed'], true)) {
            checkFail('cron', "cron job indexer_update_all_views ended as {$row['status']}: " . substr((string)$row['messages'], 0, 200));
        }
        $updated = $row['status'] === 'success';
        if (!$updated) {
            sleep(1);
        }
    }
}
if (!$updated) {
    checkFail('cron', 'indexer_update_all_views did not finish in ' . CRON_RUNS . ' cron runs');
}

$others = $db->fetchAll(
    $db->select()->from($table('cron_schedule'), ['job_code', 'status', 'messages'])
        ->where('status IN (?)', ['error', 'missed'])->where('job_code != ?', 'indexer_update_all_views')
);
if ($others) {
    $lines = array_map(function ($row) {
        return "{$row['job_code']} {$row['status']}: " . substr((string)$row['messages'], 0, 200);
    }, $others);
    echo '[INFO] cron: ' . count($others) . " other jobs errored or were missed, which this check does not judge\n" . implode("\n", $lines) . "\n";
}
echo "[OK] cron: indexer_update_all_views ran without error\n";

foreach (INDEXERS as $view) {
    $state = $db->fetchRow($db->select()->from($table('mview_state'))->where('view_id = ?', $view));
    if ($state['status'] !== 'idle' || (int)$state['version_id'] < $savedVersion[$view]) {
        checkFail('cron', "{$view} mview is {$state['status']} at version {$state['version_id']} after cron ran, expected idle at {$savedVersion[$view]} or later");
    }
}
$pricesAfter = $priceRows();
$stale = array_filter($pricesAfter, function ($row) use ($price) {
    return $row['price'] === null || abs((float)$row['price'] - $price) >= 0.001;
});
if ($priceKeys($pricesAfter) !== $priceKeysBefore || $stale) {
    checkFail('cron', 'the price index has ' . count($pricesAfter) . ' rows after cron ran and ' . count($stale) . " of them are not {$price}, it had " . count($priceKeysBefore) . ' rows before');
}
if ($searchCount($marker) < 1) {
    checkFail('cron', "the {$engine} index has no document for {$marker} after cron ran");
}

echo "[OK] cron: the change reached the price index and the {$engine} index\n";
