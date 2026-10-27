import { test, expect } from '@playwright/test';

test('mock workers render in right panel', async ({ page }) => {
  await page.goto('/');
  await page.waitForTimeout(3000);

  const workersPanel = page.locator('aside').filter({ hasText: 'Workers' });
  await expect(workersPanel).toBeVisible();

  const workerList = page.locator('.worker-hostname-list');
  await expect(workerList).toHaveCount(1);

  // Click first worker to expand
  const firstWorker = page.locator('.worker-hostname-btn').first();
  await firstWorker.click();

  await expect(page.locator('text=alpha')).toBeVisible();
  await expect(page.locator('text=llama-3-8b')).toBeVisible();
});