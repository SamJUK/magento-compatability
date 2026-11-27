/**
 * Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
 * SPDX-License-Identifier: OSL-3.0
 */

import { test, expect } from '@playwright/test';

// The reindex in smoke.sh only writes to the search engine. This reads it back.
test.describe('Search', () => {
  test('storefront search finds the seeded product and nothing for a nonsense term', async ({ page }) => {
    await page.goto('/', { waitUntil: 'domcontentloaded' });
    await page.locator('input#search, input[name="q"]').first().fill('E2E Checkout Product');
    await page.keyboard.press('Enter');
    await page.waitForURL(/catalogsearch\/result/, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('.product-item-link', { hasText: 'E2E Checkout Product' })).toBeVisible();

    // Without this a search that returns everything would pass.
    await page.goto('/catalogsearch/result/?q=zzqxjkv', { waitUntil: 'domcontentloaded' });
    await expect(page.locator('.message.notice').first()).toContainText(/no results|did not match/i);
    await expect(page.locator('.product-item-link')).toHaveCount(0);
  });
});
