import { test, expect } from '@playwright/test';

test('app shows loading state initially', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('text=Connecting to router')).toBeVisible({ timeout: 10000 });
  // After mock router responds, loading should resolve
  await expect(page.locator('text=Connecting to router')).toBeHidden({ timeout: 15000 });
});

test('router online badge appears with mock router', async ({ page }) => {
  await page.goto('/');
  // Wait for workers to load
  await page.waitForTimeout(3000);
  // The router badge should show "Router running" or connection count
  await expect(page.locator('text=worker')).toBeVisible();
});