import { test, expect } from '@playwright/test';

test('models table renders with popular models', async ({ page }) => {
  await page.goto('/');
  await page.waitForTimeout(3000);

  await expect(page.locator('text=Models')).toBeVisible();
  await expect(page.locator('text=llama-3-8b')).toBeVisible();
  await expect(page.locator('text=gpt-oss-20b')).toBeVisible();
});

test('models table pagination controls exist', async ({ page }) => {
  await page.goto('/');
  await page.waitForTimeout(3000);

  const pagination = page.locator('.pagination');
  await expect(pagination).toBeVisible();
});