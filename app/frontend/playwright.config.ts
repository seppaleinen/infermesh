import { defineConfig } from '@playwright/test';

// Playwright config for InferMesh E2E tests against the Wails server build.
//
// The webServer command launches a single launcher script (app/scripts/launch-e2e.sh)
// which:
//   1. Writes a temp settings.yml from MOCK_ROUTER_PORT / WAILS_SERVER_PORT env vars
//   2. Starts app/mock/mockRouter.js on MOCK_ROUTER_PORT and waits for TCP ready
//   3. Pre-builds (or reuses) the Wails server binary with go build -tags server,production
//   4. Starts the server, waits for HTTP 200 on http://127.0.0.1:$WAILS_SERVER_PORT/
//   5. Cleans up the mock router on exit
//
// Both ports are read from env so parallel CI runs and local development
// never collide (see #181). Defaults are 8081 (mock) and 8080 (server).
//
// The server runs with HOME set to PLAYWRIGHT_TEMP_HOME so it reads its
// settings from an isolated config dir (never the developer's real
// ~/.config/infermesh). GOMODCACHE/GOCACHE are pinned back to the real home
// so the go build inside the webServer does not try to use the temp home as
// the Go module cache.
export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: 'html',
  use: {
    baseURL: `http://127.0.0.1:${process.env.WAILS_SERVER_PORT || 8080}`,
    trace: 'on-first-retry',
  },
  webServer: {
    // The launcher script does its own health probes (TCP for mock, HTTP for server).
    // Playwright's url polling serves as a backstop.
    command: 'bash scripts/launch-e2e.sh',
    url: `http://127.0.0.1:${process.env.WAILS_SERVER_PORT || 8080}`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    cwd: '..',
    env: {
      HOME: process.env.PLAYWRIGHT_TEMP_HOME || '/tmp/infermesh-test-home',
      GOMODCACHE: `${process.env.HOME}/go/pkg/mod`,
      GOCACHE: `${process.env.HOME}/.cache/go-build`,
      WAILS_SERVER_PORT: String(process.env.WAILS_SERVER_PORT || 8080),
      MOCK_ROUTER_PORT: String(process.env.MOCK_ROUTER_PORT || 8081),
      // Allow pre-built binary override for CI speed
      // INFERMESH_SERVER_BIN: <path>,
    },
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});