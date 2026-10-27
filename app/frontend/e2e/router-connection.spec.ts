import { test, expect } from '@playwright/test';
import { waitForRouterLabel, waitForWorkerHostname } from './helpers';
import { promises as fs } from 'fs';
import { join } from 'path';

// Path to the delay control file read by the mock router. The mock holds the
// workers endpoint in flight for this many ms, so the loading card is
// deterministically observable without a client-side sleep.
const delayFile = join(
  process.env.PLAYWRIGHT_TEMP_HOME || '/tmp/infermesh-test-home',
  '.config', 'infermesh', 'delay_ms'
);

async function setDelay(ms: number) {
  await fs.mkdir(join(delayFile, '..'), { recursive: true });
  await fs.writeFile(delayFile, String(ms));
}

test.afterEach(async () => {
  // Clear any delay set by the loading-state test so the next test runs
  // at full speed.
  try {
    await fs.writeFile(delayFile, '0');
  } catch {
    // file may not exist on first run
  }
});

test('app shows loading state initially', async ({ page }) => {
  // Stall the first workers poll so the loading card is observable.
  await setDelay(3000);

  await page.goto('/');
  await expect(page.locator('text=Connecting to router…')).toBeVisible({ timeout: 10_000 });

  // Clear the delay so the next poll resolves promptly.
  await setDelay(0);
  await expect(page.locator('text=Connecting to router…')).toBeHidden({ timeout: 15_000 });
});

test('router online badge appears with mock router', async ({ page }) => {
  await page.goto('/');
  // The router label only renders once the first poll succeeds.
  await waitForRouterLabel(page);
  // At least one worker hostname from the mock fixture must appear.
  await waitForWorkerHostname(page, 'alpha');
});