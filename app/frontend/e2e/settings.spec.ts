import { test, expect } from '@playwright/test';

test('settings view opens and form loads', async ({ page }) => {
  await page.goto('/');
  await page.waitForTimeout(2000);

  await page.click('text=Settings');
  await expect(page.locator('text=Settings')).toBeVisible();
  // The settings form should have router URL field
  await expect(page.locator('input, [data-testid="router-url"]')).toBeVisible();
});