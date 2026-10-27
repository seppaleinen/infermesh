import { test, expect } from '@playwright/test';
import { waitForDashboardReady, sel } from './helpers';

test('requests table renders in-flight requests', async ({ page }) => {
  await page.goto('/');
  await waitForDashboardReady(page);

  // Scope to the requests table: the model column also uses `.model-name`,
  // and the status badge renders the lowercase status text, so bare text
  // selectors are ambiguous across the two tables.
  const requestsTable = page.locator(sel.requestsTable);
  await expect(requestsTable).toBeVisible();

  // The mock fixture ships two request rows; assert by request id.
  await expect(requestsTable.locator(sel.requestId).filter({ hasText: 'req-1' })).toBeVisible();
  await expect(requestsTable.locator(sel.requestId).filter({ hasText: 'req-2' })).toBeVisible();
});