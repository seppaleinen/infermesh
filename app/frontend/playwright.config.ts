import { defineConfig } from '@playwright/test';

// Playwright config for InferMesh E2E tests against the Wails server build.
//
// The webServer command starts two processes:
//   1. The mock router (app/mock/mockRouter.js) on MOCK_ROUTER_PORT
//   2. The Wails server build (go build -tags server,production) on WAILS_SERVER_PORT
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
    command:
      `mkdir -p $PLAYWRIGHT_TEMP_HOME/.config/infermesh && ` +
      `echo "router_addr: http://127.0.0.1:${process.env.MOCK_ROUTER_PORT || 8081}" > ` +
      `$PLAYWRIGHT_TEMP_HOME/.config/infermesh/settings.yml && ` +
      `node mock/mockRouter.js & ` +
      `go build -tags server,production -o $PLAYWRIGHT_TEMP_HOME/infermesh-server . && ` +
      `exec $PLAYWRIGHT_TEMP_HOME/infermesh-server`,
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
    },
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});