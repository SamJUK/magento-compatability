import { test, expect, Page } from '@playwright/test';

// Buys the product seeded by scripts/tests/seed-checkout-product.php as a guest.

async function addProductToCart(page: Page): Promise<void> {
  const response = await page.goto('/e2e-checkout-product.html', { waitUntil: 'domcontentloaded' });
  expect(response?.ok(), 'seeded product page should load successfully').toBeTruthy();

  // Wait for the server-rendered product form — no JS required.
  await page.waitForSelector('#product_addtocart_form', { timeout: 30_000 });

  // Drive the real storefront interaction. On cold Magento instances the
  // button can stay disabled until RequireJS finishes bootstrapping, so allow
  // a generous timeout before failing.
  const addToCartButton = page.locator('#product-addtocart-button, button.tocart').first();
  await expect(addToCartButton).toBeEnabled({ timeout: 120_000 });
  await addToCartButton.click();

  const successMessage = page.locator('[data-ui-id="message-success"], .message-success').first();
  await expect(successMessage).toContainText(/added/i, { timeout: 30_000 });

  // After Magento redirects back (usually to the product page), confirm the
  // cart now contains the item by navigating to the cart page.
  await page.goto('/checkout/cart/');
  const cartItem = page.locator('.cart.item, .cart-item, .items.data.table');
  await expect(cartItem.first()).toBeVisible({ timeout: 20_000 });
}

test.describe('Checkout', () => {
  test('guest can place an order', async ({ page }) => {
    test.slow();

    // Logged so a failure in the results log shows which request or script broke.
    page.on('response', (r) => { if (r.status() >= 400) console.log(`HTTP ${r.status()} ${r.url()}`); });
    page.on('console', (m) => { if (m.type() === 'error') console.log(`console error: ${m.text()}`); });

    await addProductToCart(page);

    // Go to checkout
    await page.goto('/checkout/', { waitUntil: 'domcontentloaded' });
    await expect(page).toHaveURL(/\/checkout/);

    // ── Step 1: Shipping ────────────────────────────────────────────────────
    // Wait for the shipping form
    const emailField = page.locator('#customer-email:visible').first();
    await expect(emailField).toBeVisible({ timeout: 20_000 });
    await emailField.fill('test@example.com');

    // Fill shipping address fields
    const firstName = page.locator('input[name="firstname"]');
    await expect(firstName).toBeVisible();
    await firstName.fill('Test');

    await page.locator('input[name="lastname"]').fill('User');
    await page.locator('input[name="street[0]"]').fill('123 Main St');
    await page.locator('input[name="city"]').fill('Austin');

    // Country — default is US; select state
    const countrySelect = page.locator('select[name="country_id"]');
    await countrySelect.selectOption('US');

    // Region/state
    const regionSelect = page.locator('select[name="region_id"]');
    if (await regionSelect.isVisible()) {
      await regionSelect.selectOption({ label: 'Texas' });
    } else {
      await page.locator('input[name="region"]').fill('TX');
    }

    await page.locator('input[name="postcode"]').fill('78701');
    await page.locator('input[name="telephone"]').fill('5125550100');

    // ── Shipping method ─────────────────────────────────────────────────────
    // Wait for shipping methods to load and select the first available
    const shippingMethods = page.locator('.table-checkout-shipping-method .col-method input[type="radio"]');
    await expect(shippingMethods.first()).toBeVisible({ timeout: 20_000 });
    await shippingMethods.first().check();

    // Next: go to payment
    const nextButton = page.locator('button.button.action.continue.primary');
    await nextButton.click();

    // ── Step 2: Payment ─────────────────────────────────────────────────────
    // Check / Money order is the only method enabled on a fresh install, so
    // Magento usually preselects it and hides the radio.
    const checkmo = page.locator('#checkmo');
    await expect(checkmo).toBeAttached({ timeout: 20_000 });
    if (await checkmo.isVisible()) {
      await checkmo.check();
    }

    const placeOrderButton = page.locator('.payment-method._active button.action.primary.checkout');
    await expect(placeOrderButton).toBeVisible({ timeout: 20_000 });
    await expect(placeOrderButton).toBeEnabled({ timeout: 20_000 });
    await placeOrderButton.click();

    // ── Order success page ───────────────────────────────────────────────────
    await page.waitForURL(/checkout\/onepage\/success|checkout\/success/, {
      timeout: 30_000,
    });

    const successHeading = page.locator('h1.page-title, .page-title').first();
    await expect(successHeading).toContainText(/thank you/i);
    await expect(page.locator('.checkout-success')).toContainText(/order/i);
  });
});
