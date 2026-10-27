import { test, expect } from '@playwright/test';
import { waitForAppShell } from './helpers';

test('app loads, header renders, config loads', async ({ page }) => {
  await page.goto('/');
  await waitForAppShell(page);
  // Nav tabs are present.
  await expect(page.locator('role=tab[name="Dashboard"]')).toBeVisible();
  await expect(page.locator('role=tab[name="Settings"]')).toBeVisible();
});