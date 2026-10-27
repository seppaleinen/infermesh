import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: 'html',
  use: {
    baseURL: 'http://127.0.0.1:8080',
    trace: 'on-first-retry',
  },
  webServer: {
    command: 'mkdir -p $PLAYWRIGHT_TEMP_HOME/.config/infermesh && echo "router_addr: http://127.0.0.1:8081" > $PLAYWRIGHT_TEMP_HOME/.config/infermesh/settings.yml && node ../mock/mockRouter.js & sleep 2 && go run -tags server,production .',
    url: 'http://127.0.0.1:8080',
    reuseExistingServer: !process.env.CI,
    timeout: 120000,
    cwd: '..',
    env: {
      HOME: process.env.PLAYWRIGHT_TEMP_HOME || '/tmp/infermesh-test-home',
      WAILS_SERVER_PORT: '8080',
    },
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});