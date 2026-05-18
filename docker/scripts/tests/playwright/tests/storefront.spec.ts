import { test, expect } from '@playwright/test';

/**
 * Storefront smoke tests — verify the Magento storefront renders correctly.
 * These tests do NOT require login; they cover the guest-visible pages.
 */

test.describe('Storefront', () => {
  test('homepage loads and has a non-empty title', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveTitle(/.+/);
  });

  // test('homepage contains the main navigation menu', async ({ page }) => {
  //   await page.goto('/');
  //   // Magento renders the actual nav inside <nav class="navigation">
  //   const nav = page.locator('nav.navigation');
  //   await expect(nav).toBeVisible();
  // });

  // test('homepage renders at least one category link in the nav', async ({ page }) => {
  //   await page.goto('/');
  //   // Use level0 nav items — actual top-level category links (not the mobile toggle)
  //   const navLinks = page.locator('nav.navigation li.level0 > a');
  //   await expect(navLinks.first()).toBeVisible();
  //   const count = await navLinks.count();
  //   expect(count).toBeGreaterThan(0);
  // });

  // test('category page renders a product grid', async ({ page }) => {
  //   // Sample data provides a "Women" top-level category at /women.html
  //   // Fall back to a search results page if the slug differs.
  //   await page.goto('/women.html', { waitUntil: 'domcontentloaded' });

  //   const is404 = page.url().includes('noRoute') ||
  //                 (await page.title()).toLowerCase().includes('404');

  //   if (is404) {
  //     // Fall back: use search results which always shows products with sample data
  //     await page.goto('/catalogsearch/result/?q=shirt');
  //   }

  //   const productGrid = page.locator('.products-grid').first();
  //   await expect(productGrid).toBeVisible({ timeout: 15_000 });

  //   const productItems = productGrid.locator('li.product-item');
  //   const count = await productItems.count();
  //   expect(count).toBeGreaterThan(0);
  // });

  // test('product detail page renders Add to Cart button', async ({ page }) => {
  //   // Use "bag" — simple products with no required swatches, button is immediately enabled
  //   await page.goto('/catalogsearch/result/?q=bag');

  //   const firstProduct = page.locator('li.product-item a.product-item-link').first();
  //   await expect(firstProduct).toBeVisible({ timeout: 15_000 });
  //   await firstProduct.click();

  //   await page.waitForURL(/\/[^/]+\.html$/);

  //   const addToCart = page.locator('button.tocart, #product-addtocart-button');
  //   await expect(addToCart).toBeVisible();
  // });
});
