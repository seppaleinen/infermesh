// Reusable Playwright helpers for InferMesh E2E specs.
//
// All waits go through Playwright's auto-wait (expect(...).toBeVisible / toHaveCount)
// rather than arbitrary page.waitForTimeout() sleeps. Each helper takes an
// explicit timeout so a slow mock router never flakes a test.

import { type Page, expect } from '@playwright/test';

// Selectors shared across specs. Centralised so a class rename is one edit.
export const sel = {
  brandName: 'css=.brand-name',
  nav: 'css=nav',
  dashboardTab: 'role=tab[name="Dashboard"]',
  settingsTab: 'role=tab[name="Settings"]',
  workersPanel: 'css=.workers-panel',
  workersTitle: 'css=.workers-title',
  workerList: 'css=.worker-hostname-list',
  workerButton: 'css=.worker-hostname-btn',
  workerHostname: 'css=.worker-hostname',
  modelsPanel: 'css=.models-panel',
  modelsTitle: 'css=.models-title',
  modelsTable: 'css=.popular-table',
  modelName: 'css=.model-name',
  pagination: 'css=.pagination',
  requestsPanel: 'css=.requests-panel',
  requestsTitle: 'css=.requests-title',
  requestsTable: 'css=.requests-table',
  requestId: 'css=.col-id',
  statusCard: 'css=.status-card',
  statusTitle: 'css=.status-title',
  routerLabel: 'css=.router-label',
  routerUrl: 'css=.router-url',
  supervisorValue: 'css=.supervisor-value',
} as const;

const DEFAULT_TIMEOUT = 30_000;

/** Wait until the app shell is mounted: brand name + nav are visible. */
export async function waitForAppShell(page: Page, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.brandName)).toBeVisible({ timeout });
  await expect(page.locator(sel.nav)).toBeVisible({ timeout });
}

/** Wait until the dashboard view (workers/models) is fully loaded:
 * the loading spinner card is gone and either the models panel or a
 * terminal status card (empty pool / router unreachable) is visible. */
export async function waitForDashboardReady(page: Page, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.statusCard)).not.toBeVisible({ timeout });
  const ready = page.locator(sel.modelsPanel).or(page.locator(sel.statusCard));
  await expect(ready.first()).toBeVisible({ timeout });
}

/** Wait for the worker list to contain at least `minCount` hostname buttons. */
export async function waitForWorkerCount(page: Page, minCount: number, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.workerButton)).toHaveCount(minCount, { timeout });
}

/** Wait for a specific worker hostname to appear in the list. */
export async function waitForWorkerHostname(page: Page, hostname: string, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.workerHostname).filter({ hasText: hostname })).toBeVisible({ timeout });
}

/** Click a worker row by hostname and wait for its expanded card to render. */
export async function expandWorker(page: Page, hostname: string, timeout = DEFAULT_TIMEOUT) {
  await page.locator(sel.workerButton).filter({ hasText: hostname }).click();
  // The expanded card renders a WorkerCard with the hostname in the body.
  await expect(page.locator('css=.worker-card')).toBeVisible({ timeout });
}

/** Wait for a specific model name to appear in the popular-models table. */
export async function waitForModel(page: Page, model: string, timeout = DEFAULT_TIMEOUT) {
  // Scope to the popular models table: `.model-name` also appears in the
  // requests table (the model column), so the bare class is ambiguous.
  await expect(
    page.locator('css=.popular-table').locator(sel.modelName).filter({ hasText: model })
  ).toBeVisible({ timeout });
}

/** Wait for the pagination control to be visible (only rendered when >1 page). */
export async function waitForPagination(page: Page, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.pagination)).toBeVisible({ timeout });
}

/** Wait for a specific request id to appear in the requests table. */
export async function waitForRequestId(page: Page, requestId: string, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.requestId).filter({ hasText: requestId })).toBeVisible({ timeout });
}

/** Navigate to the Settings view and wait for the form to load. */
export async function openSettings(page: Page, timeout = DEFAULT_TIMEOUT) {
  await page.click(sel.settingsTab);
  await expect(page.locator('css=.settings')).toBeVisible({ timeout });
  // The form renders multiple sections; wait for the first one.
  await expect(page.locator('css=.form-section').first()).toBeVisible({ timeout });
}

/** Wait for the router online label (only rendered once the first poll succeeds). */
export async function waitForRouterLabel(page: Page, timeout = DEFAULT_TIMEOUT) {
  await expect(page.locator(sel.routerLabel)).toBeVisible({ timeout });
}