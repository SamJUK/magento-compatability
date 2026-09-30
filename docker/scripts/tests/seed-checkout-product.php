<?php
// Creates the in-stock simple product the Playwright checkout spec buys.

use Magento\Catalog\Api\Data\ProductInterface;
use Magento\Catalog\Api\ProductRepositoryInterface;
use Magento\Framework\App\Bootstrap;
use Magento\Framework\App\State;

require getcwd() . '/app/bootstrap.php';

$objectManager = Bootstrap::create(BP, $_SERVER)->getObjectManager();
$objectManager->get(State::class)->setAreaCode('adminhtml');

$product = $objectManager->create(ProductInterface::class);
$product->setSku('e2e-checkout-product')
    ->setName('E2E Checkout Product')
    ->setUrlKey('e2e-checkout-product')
    ->setTypeId('simple')
    ->setAttributeSetId($product->getDefaultAttributeSetId())
    ->setWebsiteIds([1])
    ->setPrice(10)
    ->setWeight(1)
    ->setStatus(1)
    ->setVisibility(4)
    ->setStockData(['use_config_manage_stock' => 1, 'qty' => 100, 'is_in_stock' => 1]);

$objectManager->get(ProductRepositoryInterface::class)->save($product);

echo "[OK] Seeded e2e-checkout-product\n";
