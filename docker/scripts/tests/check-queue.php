<?php

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Proves the message queue delivers: schedules the bulk the admin mass attribute
// update schedules, runs the consumer once, and checks the attribute changed.
// Cron's consumers runner is switched off meanwhile so it cannot take the message.

require __DIR__ . '/check-lib.php';

const TOPIC = 'product_action_attribute.update';

$objectManager = checkObjectManager();

if (!interface_exists(\Magento\Framework\Bulk\BulkManagementInterface::class)) {
    checkSkip('queue', 'this release has no bulk management');
}
$consumers = $objectManager->get(\Magento\Framework\MessageQueue\Consumer\ConfigInterface::class);
$publishers = $objectManager->get(\Magento\Framework\MessageQueue\Publisher\ConfigInterface::class);
try {
    $consumerConnection = $consumers->getConsumer(TOPIC)->getConnection();
} catch (\Magento\Framework\Exception\LocalizedException $e) {
    checkSkip('queue', 'this release has no ' . TOPIC . ' consumer');
}
try {
    $publisher = $publishers->getPublisher(TOPIC)->getConnection();
} catch (\Magento\Framework\Exception\LocalizedException $e) {
    checkFail('queue', TOPIC . ' has a consumer but no publisher, so nothing can reach it');
}
if ($publisher->isDisabled()) {
    checkFail('queue', TOPIC . ' has a consumer but its publisher connection is disabled');
}
$publisherConnection = $publisher->getName();
if ($publisherConnection !== $consumerConnection) {
    checkFail('queue', TOPIC . " publishes over {$publisherConnection} but is consumed over {$consumerConnection}, so a message cannot reach its consumer");
}

$queueConfig = (array)$objectManager->get(\Magento\Framework\App\DeploymentConfig::class)->get('queue');
$broker = isset($queueConfig['stomp']) ? 'stomp' : 'amqp';
if (!isset($queueConfig[$broker])) {
    checkError('queue', 'env.php configures neither an amqp nor a stomp connection');
}
if ($publisherConnection !== $broker) {
    checkSkip('queue', TOPIC . " uses the {$publisherConnection} queue on this release, not {$broker}");
}

$productId = checkProductId($objectManager);
$resource = $objectManager->get(\Magento\Framework\App\ResourceConnection::class);
$db = $resource->getConnection();
$adminId = (int)$db->fetchOne($db->select()->from($resource->getTableName('admin_user'), 'MIN(user_id)'));

$productResource = $objectManager->get(\Magento\Catalog\Model\ResourceModel\Product::class);
$readMetaTitle = function () use ($productResource, $productId) {
    return $productResource->getAttributeRawValue($productId, 'meta_title', 0);
};

$marker = 'queue-check-' . time();
$bulkUuid = $objectManager->get(\Magento\Framework\DataObject\IdentityGeneratorInterface::class)->generateId();
$operation = $objectManager->get(\Magento\AsynchronousOperations\Api\Data\OperationInterfaceFactory::class)->create(['data' => [
    'bulk_uuid' => $bulkUuid,
    'topic_name' => TOPIC,
    'serialized_data' => $objectManager->get(\Magento\Framework\Serialize\SerializerInterface::class)->serialize([
        'meta_information' => 'Update product attributes',
        'product_ids' => [$productId],
        'store_id' => 0,
        'website_id' => 0,
        'attributes' => ['meta_title' => $marker],
    ]),
    'status' => \Magento\Framework\Bulk\OperationInterface::STATUS_TYPE_OPEN,
]]);
$bulkStatus = $objectManager->get(\Magento\Framework\Bulk\BulkStatusInterface::class);

$cronRunnerWasEnabled = cronConsumersRunnerEnabled($objectManager);
setCronConsumersRunner($objectManager, false);
register_shutdown_function('setCronConsumersRunner', $objectManager, $cronRunnerWasEnabled);

$scheduled = $objectManager->get(\Magento\Framework\Bulk\BulkManagementInterface::class)
    ->scheduleBulk($bulkUuid, [$operation], 'Service check: update product attributes', $adminId);
if (!$scheduled) {
    checkFail('queue', 'scheduleBulk returned false, the message was not published');
}
echo "[OK] queue: published bulk {$bulkUuid}\n";

$finished = \Magento\Framework\Bulk\BulkSummaryInterface::FINISHED_SUCCESSFULLY;
if ($readMetaTitle() === $marker || (int)$bulkStatus->getBulkStatus($bulkUuid) === $finished) {
    checkError('queue', 'the update was already applied before any consumer ran');
}

$help = (string)shell_exec('php ' . BP . '/bin/magento queue:consumers:start --help 2>&1');
$singleThread = strpos($help, '--single-thread') !== false ? ' --single-thread' : '';
passthru('timeout 120 php ' . BP . '/bin/magento queue:consumers:start ' . TOPIC . ' --max-messages=1' . $singleThread . ' 2>&1', $exitCode);
if ($exitCode !== 0) {
    checkFail('queue', "the consumer exited with {$exitCode} after the message was published (15 or 143 means a signal stopped it, probably the 120 second timeout)");
}

$actual = $readMetaTitle();
if ($actual !== $marker) {
    checkFail('queue', 'meta_title is ' . var_export($actual, true) . ' after the consumer ran, expected ' . $marker);
}

$status = (int)$bulkStatus->getBulkStatus($bulkUuid);
if ($status !== \Magento\Framework\Bulk\BulkSummaryInterface::FINISHED_SUCCESSFULLY) {
    checkFail('queue', "bulk status is {$status}, expected " . \Magento\Framework\Bulk\BulkSummaryInterface::FINISHED_SUCCESSFULLY);
}
$complete = (int)$bulkStatus->getOperationsCountByBulkIdAndStatus($bulkUuid, \Magento\Framework\Bulk\OperationInterface::STATUS_TYPE_COMPLETE);
if ($complete !== 1) {
    checkFail('queue', "expected 1 complete operation, found {$complete}");
}

echo "[OK] queue: the consumer applied the update and the bulk finished successfully\n";
