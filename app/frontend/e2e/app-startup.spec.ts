import { test, expect } from '@playwright/test';

test('app loads, header renders, config loads', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('text=InferMesh')).toBeVisible();
  await expect(page.locator('nav')).toBeVisible();
  await expect(page.locator('text=Dashboard')).toBeVisible();
  await expect(page.locator('text=Settings')).toBeVisible();
});