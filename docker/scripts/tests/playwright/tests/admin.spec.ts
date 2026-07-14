import { test, expect, Page } from '@playwright/test';

const adminEnabled = process.env.PLAYWRIGHT_ADMIN === '1';
const adminPath = normaliseAdminPath(process.env.PLAYWRIGHT_ADMIN_PATH ?? '/admin');
const adminUser = process.env.PLAYWRIGHT_ADMIN_USER ?? 'admin';
const adminPassword = process.env.PLAYWRIGHT_ADMIN_PASSWORD ?? 'Admin123!';

function normaliseAdminPath(input: string): string {
  const trimmed = input.trim();

  if (!trimmed || trimmed === '/') {
    return '/admin';
  }

  return trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
}

async function loginToAdmin(page: Page): Promise<void> {
  const response = await page.goto(adminPath, { waitUntil: 'domcontentloaded' });
  expect(response, 'admin login page should return an HTTP response').not.toBeNull();
  expect((response?.status() ?? 500) < 400, 'admin login page should not hard-fail').toBeTruthy();

  await expect(page.locator('input#username, input[name="login[username]"]').first()).toBeVisible({
    timeout: 30_000,
  });
  await page.locator('input#username, input[name="login[username]"]').first().fill(adminUser);
  await page.locator('input#login, input[name="login[password]"]').first().fill(adminPassword);
  await page.locator('.actions .action-primary, .login-actions button[type="submit"]').first().click();

  await expect(page.locator('body')).toHaveClass(/adminhtml-dashboard-index/, {
    timeout: 30_000,
  });
}

async function dismissAdminModals(page: Page): Promise<void> {
  const usageModal = page.locator('.admin-usage-notification').first();
  const usageModalVisible = await usageModal
    .waitFor({ state: 'visible', timeout: 5_000 })
    .then(() => true)
    .catch(() => false);

  if (usageModalVisible) {
    const dontAllowButton = usageModal.locator('.action-secondary').first();
    await dontAllowButton.click({ force: true });
    await expect(usageModal).toBeHidden({ timeout: 15_000 });
    await page.waitForTimeout(500);
    return;
  }

  const closeButton = page.locator(
    '.modal-popup .action-close, .modal-slide .action-close, .modal-popup .action-secondary',
  ).first();
  if (await closeButton.isVisible().catch(() => false)) {
    await closeButton.click();
    await page.waitForTimeout(500);
  }
}

test.describe('Admin', () => {
  test.skip(!adminEnabled, 'requires PLAYWRIGHT_ADMIN=1');

  test('admin can create a CMS block', async ({ page }) => {
    test.slow();

    await loginToAdmin(page);

    const suffix = Date.now().toString(36);
    const title = `Playwright CMS Block ${suffix}`;
    const identifier = `playwright-cms-block-${suffix}`;

    await dismissAdminModals(page);

    await page.getByRole('link', { name: 'Content' }).click();
    await page.getByRole('link', { name: 'Blocks' }).click();
    await expect(page.locator('body')).toHaveClass(/cms-block-index/, {
      timeout: 30_000,
    });

    await page.getByRole('button', { name: /add new block/i }).click();
    await expect(page.locator('body')).toHaveClass(/cms-block-new|cms-block-edit/, {
      timeout: 30_000,
    });

    await page.locator('input[name="title"]').fill(title);
    await page.locator('input[name="identifier"]').fill(identifier);

    const saveButton = page.locator('#save, button[data-ui-id="save-button"], button.action-primary.save').first();
    await expect(saveButton).toBeVisible({ timeout: 20_000 });
    await saveButton.click();

    const successMessage = page.locator('.message-success').first();
    await expect(successMessage).toContainText(/saved/i, { timeout: 30_000 });
    await expect(page.locator('input[name="identifier"]')).toHaveValue(identifier);
  });
});
