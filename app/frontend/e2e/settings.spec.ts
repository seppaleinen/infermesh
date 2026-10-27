import { test, expect } from '@playwright/test';
import { openSettings } from './helpers';

test('settings view opens and form loads', async ({ page }) => {
  await page.goto('/');
  await openSettings(page);

  // The form must render its Connection section fields.
  await expect(page.locator('#router_addr')).toBeVisible();
  await expect(page.locator('#worker_backend')).toBeVisible();
  await expect(page.locator('#worker_model_path')).toBeVisible();
  await expect(page.locator('#worker_port')).toBeVisible();
});