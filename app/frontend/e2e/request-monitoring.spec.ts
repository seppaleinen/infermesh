import { test, expect } from '@playwright/test';

test('requests table renders in-flight requests', async ({ page }) => {
  await page.goto('/');
  await page.waitForTimeout(3000);

  await expect(page.locator('text=Requests')).toBeVisible();
  // Check for request IDs from mock
  await expect(page.locator('text=req-1').or(page.locator('text=inflight'))).toBeVisible();
});