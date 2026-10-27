# Add Playwright E2E tests for desktop app (Issue #180)

## Summary

Implements end-to-end GUI tests for the InferMesh desktop app using Playwright against a Wails v3 server build. App entry point is split into build-tag variants to exclude native SystemTray/autostart code from server mode. A lightweight Node mock router serves canned router API responses so tests run without real ML models or workers. CI job `app-e2e` runs the suite on ubuntu-latest. No changes to `pkg/` code.

## Changes

### App build split
- `app/main_common.go` – shared application initialization
- `app/main_gui.go` – `//go:build !server`; SystemTray, supervisor, autostart, instance lock
- `app/main_server.go` – `//go:build server`; Server options with host 127.0.0.1:8080

### Playwright setup
- `app/frontend/package.json` – added `@playwright/test` dev dependency
- `app/frontend/playwright.config.ts` – webServer starts mock router on 8081 and Wails server build with settings injection; baseURL http://127.0.0.1:8080; Chromium only
- `app/frontend/e2e/helpers.ts` – browser helpers and test data factories
- `app/frontend/e2e/app-startup.spec.ts`
- `app/frontend/e2e/router-connection.spec.ts`
- `app/frontend/e2e/worker-listing.spec.ts`
- `app/frontend/e2e/model-browsing.spec.ts`
- `app/frontend/e2e/request-monitoring.spec.ts`
- `app/frontend/e2e/settings.spec.ts`

### Mock infrastructure
- `app/mock/mockRouter.js` – Node HTTP server mimicking router endpoints with static fixtures matching `pkg/router` types

### CI and tooling
- `.github/workflows/ci.yml` – added `app-e2e` job parallel to `app-frontend`; Go 1.27+, Node 22, wails3 v3.0.0, Playwright install, test execution
- `Makefile` – added `test-app-e2e` target: builds server and runs Playwright tests

### Documentation
- `app/README.md` – added E2E Testing with Playwright section with prerequisites, run commands, settings injection notes, spec coverage, mock router notes
- `docs/adr/ADR-018-e2e-gui-tests-playwright.md` – architecture decision record
- `docs/plans/issue-180-e2e-gui-tests.md` – updated status to DONE and added Implementation Summary

## Testing

Local run:

```bash
cd app/frontend
npx playwright install --with-deps chromium
npx playwright test
```

With UI:

```bash
npx playwright test --ui
```

Using Makefile:

```bash
make test-app-e2e
```

Prerequisites: Node 22, npm, Go 1.27+, wails3 v3.0.0.

The Playwright `webServer.command` writes `router_addr: http://127.0.0.1:8081` to `$PLAYWRIGHT_TEMP_HOME/.config/infermesh/settings.yml`, starts `app/mock/mockRouter.js` on 8081, then runs `go run -tags server,production .` to serve the Wails app on 8080.

CI runs the same flow on ubuntu-latest in the `app-e2e` job.

## Acceptance criteria verification

- [x] Playwright configured for Wails server build (`-tags server,production`)
- [x] Test suite runs in CI successfully via `app-e2e` job
- [x] Six critical user flows tested: app startup, router connection, worker listing, model browsing, request monitoring, settings
- [x] Mocked workers/inference engines used; no real ML models required
- [x] Tests catch build errors via `webServer` startup validation
- [x] Documentation on running tests locally added to `app/README.md`

No changes to `pkg/router` or `pkg/worker` code. No new Go dependencies.
