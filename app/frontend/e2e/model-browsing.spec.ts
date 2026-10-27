import { test, expect } from '@playwright/test';
import { waitForModel, waitForPagination, waitForDashboardReady, sel } from './helpers';

test('models table renders with popular models', async ({ page }) => {
  await page.goto('/');
  await waitForDashboardReady(page);

  // The mock fixture ships 12 models; the first 10 are on page 1.
  await waitForModel(page, 'llama-3-8b');
  await waitForModel(page, 'gpt-oss-20b');
  await waitForModel(page, 'phi-4-mini');
  await waitForModel(page, 'jamba-1.5-large');
});

test('models table pagination controls exist and advance', async ({ page }) => {
  await page.goto('/');
  await waitForDashboardReady(page);

  // 12 models at a page size of 10 => 2 pages, so the pagination control
  // renders. Wait for it explicitly rather than assuming it is present.
  await waitForPagination(page);

  // Page indicator starts at 1 / 2.
  await expect(page.locator('.page-indicator')).toHaveText('1 / 2');

  // Click next and assert the indicator advances and page-2 content renders.
  await page.locator('.page-btn').nth(1).click();
  await expect(page.locator('.page-indicator')).toHaveText('2 / 2');
  await expect(
    page.locator('css=.popular-table').locator(sel.modelName).filter({ hasText: 'granite-3.1-8b' })
  ).toBeVisible();
});