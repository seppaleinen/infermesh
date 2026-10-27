# ADR-018: E2E GUI Tests with Playwright for Wails Server Mode

**Status:** Accepted

**Date:** 2026-10-27

## Context

The InferMesh desktop app uses a Vue 3 frontend built with Wails v3. Manual testing of UI workflows is slow and cannot run in CI. The UI depends on the router service for worker listing, model browsing, request monitoring, and settings persistence. Real ML models and workers are too heavy for CI, and native tray/OS features are out of scope for automated GUI testing. We need automated tests that exercise the Vue UI against a headless Wails server build on ubuntu-latest, without real inference engines.

## Decision

Split `app/main.go` into build-tag variants:
- `app/main_common.go` holds shared initialization
- `app/main_gui.go` (`//go:build !server`) contains SystemTray, supervisor, autostart, and instance-lock code
- `app/main_server.go` (`//go:build server`) provides minimal server configuration

Add Playwright E2E tests against the Wails server build (`-tags server,production`):
- Playwright config at `app/frontend/playwright.config.ts` starts a mock Node HTTP router on port 8081 and the Wails server on 8080.
- Mock router `app/mock/mockRouter.js` serves canned JSON matching `pkg/router` types for workers, models, in-flight requests, stats, aliases, and auto-routing.
- Six specs cover app startup, router connection, worker listing, model browsing, request monitoring, and settings validation.

CI integration:
- New `app-e2e` job in `.github/workflows/ci.yml` runs on ubuntu-latest with Go 1.27+, Node 22, wails3 v3.0.0.
- `make test-app-e2e` target added to root Makefile.

## Consequences

Positive:
- UI workflows are validated automatically in CI without real ML models or GPUs.
- Build errors are caught early via Playwright `webServer` startup validation.
- Mock router isolates tests from router/worker implementation changes.

Negative / accepted costs:
- Server mode requires settings injection for router URL via temporary `settings.yml` written by the Playwright `webServer.command`. The injection path is ` $PLAYWRIGHT_TEMP_HOME/.config/infermesh/`.
- Build tags increase complexity: three main files instead of one, and bindings must be generated with `-tags=server` before frontend build.
- Playwright tests run in headless Chromium only; native tray, OS menus, and file dialogs remain untested by design.
- Mock router fidelity must be maintained as RouterClient types evolve.

Related files:
- `app/main_common.go`, `app/main_gui.go`, `app/main_server.go`
- `app/frontend/playwright.config.ts`
- `app/frontend/e2e/`
- `app/mock/mockRouter.js`
- `docs/plans/issue-180-e2e-gui-tests.md`
