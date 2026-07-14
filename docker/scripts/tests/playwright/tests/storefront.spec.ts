import { test, expect } from '@playwright/test';

/**
 * Storefront smoke tests — verify the Magento storefront renders correctly.
 * These tests do NOT require login; they cover the guest-visible pages.
 */

test.describe('Storefront', () => {
  test('homepage renders a real Magento storefront shell', async ({ page }) => {
    const response = await page.goto('/', { waitUntil: 'domcontentloaded' });

    expect(response, 'homepage should return an HTTP response').not.toBeNull();
    expect(response?.ok(), 'homepage should respond successfully').toBeTruthy();

    await expect(page).toHaveTitle(/.+/);
    await expect(page.locator('body')).toHaveClass(/cms-index-index/);
    await expect(page.locator('.page-wrapper')).toBeVisible();
    await expect(page.locator('a.logo, .logo a')).toBeVisible();
    await expect(page.locator('input#search, input[name="q"]')).toBeVisible();
    await expect(page.locator('text=There has been an error processing your request')).toHaveCount(0);
    await expect(page.locator('text=Welcome to Magento Admin')).toHaveCount(0);
  });
});
