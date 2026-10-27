import { test, expect } from '@playwright/test';
import {
  waitForWorkerCount,
  waitForWorkerHostname,
  expandWorker,
  waitForDashboardReady,
  sel,
} from './helpers';

test('mock workers render in right panel', async ({ page }) => {
  await page.goto('/');
  await waitForDashboardReady(page);

  // The mock fixture ships two workers; the right panel must list both.
  await waitForWorkerCount(page, 2);

  // Click the first worker to expand its detail card.
  await expandWorker(page, 'alpha');

  // The expanded card must show the hostname and its loaded model. Scope to
  // the worker card: the hostname also appears in the requests table, and the
  // model name also appears in the popular-models table.
  const card = page.locator('css=.worker-card').filter({ hasText: 'alpha' }).first();
  await expect(card).toBeVisible();
  await expect(card.locator('.model-chip', { hasText: 'llama-3-8b' })).toBeVisible();
});