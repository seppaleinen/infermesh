# Plan: Issue #180 — E2E tests for GUI with mocked inference engines

Source issue: https://github.com/seppaleinen/infermesh/issues/180
Status: [DONE]
REWORK_COUNT: 0

## Clarification (locked)

- Goal: Playwright E2E tests against Wails v3 server build (`-tags server`) to test the Vue UI workflows.
- No real ML models; mocked inference engines only.
- Tests must run in CI on `ubuntu-latest` (the current `app-frontend` job already runs there).
- Playwright tests cover browser UI + HTTP service calls only; native tray, OS menus, file dialogs are explicitly out of scope (Wails docs limitation).
- The current `app/main.go` uses `SystemTray`, `supervisor`, `autostart` — all GUI-native features. Server mode must not break the desktop build.

## Approach (incremental phases, low blast radius)

### Phase 0 — Spike / Validation
1. Verify Wails v3 server mode works with the current `app/main.go`. Build `-tags server,production` and confirm it serves HTTP on default ports without panicking on `SystemTray`/native calls.
2. Confirm `application.Options.Server` fields are available in v3.0.0 and do not conflict with existing `Assets`/`Services` configuration.
3. Validate that the existing `Taskfile.yml` tasks `build:server` and `run:server` already exist and produce a usable binary at `bin/app/infermesh-app-server`.

### Phase 1 — App server configuration
1. Add `Server: application.ServerOptions{Host:"127.0.0.1",Port:8080}` to `application.New(...)` in `app/main.go`.
2. Guard SystemTray / native-only initialization code with compile-time checks. Options:
   - Split `main.go` into `main_gui.go` (`//go:build !server`) and `main_server.go` (`//go:build server`) keeping common setup in `main_common.go`, or
   - Runtime check `if !app.IsRunningServerMode()` before SystemTray/Autostart/InstanceLock usage (verify Wails API provides this; fallback to build tag).
3. Update `app/build/Taskfile.yml` `build:server` to ensure frontend is built with Vite production before `go build -tags server,production`.

### Phase 2 — Playwright setup
1. Add Playwright dev dependency to `app/frontend/package.json`:
   ```
   "devDependencies": {
     "@playwright/test": "^1.48.0"
   }
   ```
2. Create `app/frontend/playwright.config.ts` per Wails docs:
   - `testDir: './e2e'`
   - `webServer.command: 'go run -tags server,production .'` with `cwd: '..'` and `url: http://127.0.0.1:8080`
   - `baseURL: http://127.0.0.1:8080`
   - Use Chromium only for CI speed.
3. Create `app/frontend/e2e/` directory with:
   - `helpers.ts` – browser helpers, test data factories.
   - `mocks/mockRouter.ts` – lightweight Node HTTP server that mimics router's `/v1/workers`, `/meta/models/popular`, `/meta/requests/inflight`, `/v1/inference-stats`, `/meta/aliases`, `/meta/auto-routing` endpoints with canned JSON. Returns static workers and models.
   - `mocks/mockBackend.ts` – optional OpenAI-compatible mock for worker backend (used only if Phase 3 requires a real worker process).

### Phase 3 — Mock infrastructure
Two mock strategies are available; choose one for MVP:

**A. Pure frontend mock (no router/worker binaries)**
- RouterClient Wails service is bound to the frontend; Playwright can mock the `/api` endpoints at browser-level using Playwright route interception. This tests Vue reactivity without a live router.
- Pros: Fast, no Go binaries, single-process.
- Cons: Doesn't test actual RouterClient Go code.

**B. Real router + mock worker (recommended for Issue #180 intent)**
- CI job starts router binary (`bin/infermesh-router --dev-mode`) on port 8081.
- Start a mock worker process that registers with the router via `--router http://127.0.0.1:8081 --backend custom --backend-url http://127.0.0.1:9000` where port 9000 serves a tiny Node/Go OpenAI-compatible mock returning canned responses.
- The app server runs with RouterURL pointing at `http://127.0.0.1:8081`.
- Pros: Tests real RouterClient, real discovery, real scheduler paths.
- Cons: Adds process orchestration to Playwright webServer.

MVP chooses **A** for initial tests (build error detection + basic UI flows), then **B** is added in Phase 5.

### Phase 4 — Test scenarios
Create at least 3-5 critical user flows per definition of done:

1. `app-startup.spec.ts` – page loads, window renders, tray hidden in server mode, config loads.
2. `router-connection.spec.ts` – unreachable router shows error card; reachable router (mock) shows "Router running" status.
3. `worker-listing.spec.ts` – mock workers array renders cards with ID, hostname, loaded models, status, in-flight counts.
4. `model-browsing.spec.ts` – `/meta/models/popular` list renders, filtering works.
5. `request-monitoring.spec.ts` – in-flight requests table renders with status, duration.
6. `settings.spec.ts` – settings form loads, validation shows error on invalid URL.

Each test uses Playwright fixture data (JSON fixtures in `e2e/fixtures/`).

### Phase 5 — CI integration
Add new job `app-e2e` to `.github/workflows/ci.yml` parallel to `app-frontend`:
- Check out, setup Go 1.27.1, Node 22 (cache npm).
- `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0`
- `cd app && wails3 generate bindings -f '-tags=server'`
- `cd app/frontend && npm ci && npm run build`
- `cd app/frontend && npx playwright install --with-deps chromium`
- `cd app/frontend && npx playwright test` (Playwright launches the server via `webServer.command`).

Cache Playwright browsers between runs.

Also add `make test-app-e2e` target to root Makefile:
```make
test-app-e2e:
    cd app && wails3 task build-server && cd frontend && npx playwright test
```

### Phase 6 — Documentation
- Update `app/README.md` with E2E testing instructions.
- Add `docs/plans/issue-180-e2e-gui-tests.md` (this file).
- Document mock data format and how to extend.

## Files touched

- `app/main.go` — Add `Server Options`, guard SystemTray/autostart/instance-lock for server mode.
- `app/build/Taskfile.yml` — Verify `build:server` frontend deps.
- `app/frontend/package.json` — Add `@playwright/test` devDep.
- `app/frontend/playwright.config.ts` — New.
- `app/frontend/e2e/` — New directory with specs, helpers, mocks.
- `.github/workflows/ci.yml` — Add `app-e2e` job.
- `Makefile` — Add `test-app-e2e` target.
- `docs/plans/issue-180-e2e-gui-tests.md` — This plan.

No changes to `pkg/` router/worker code; no new Go dependencies.

## Implementation Summary

- Split `app/main.go` into `app/main_common.go`, `app/main_gui.go` (`//go:build !server`), and `app/main_server.go` (`//go:build server`) to allow native SystemTray/autostart code to be excluded from server builds.
- Added `Server.Options` with host 127.0.0.1 and port 8080; guarded SystemTray, supervisor, and autostart initialization behind build tags.
- Created Playwright configuration at `app/frontend/playwright.config.ts` with `webServer.command` that injects router settings and starts the Node mock router on port 8081 before launching a server-mode Wails build.
- Added mock router `app/mock/mockRouter.js` serving canned JSON for `/v1/workers`, `/meta/models/popular`, `/meta/requests/inflight`, `/v1/inference-stats`, `/meta/aliases`, `/meta/auto-routing`.
- Implemented six Playwright specs in `app/frontend/e2e/`:
  - `app-startup.spec.ts`
  - `router-connection.spec.ts`
  - `worker-listing.spec.ts`
  - `model-browsing.spec.ts`
  - `request-monitoring.spec.ts`
  - `settings.spec.ts`
- Added helpers at `app/frontend/e2e/helpers.ts`.
- Added Playwright dev dependency to `app/frontend/package.json`.
- Updated `.github/workflows/ci.yml` with `app-e2e` job running on `ubuntu-latest` with Go 1.27+, Node 22, wails3 v3.0.0.
- Added `make test-app-e2e` target in root Makefile.
- Updated `app/README.md` with E2E testing instructions.

Acceptance criteria are met:
- Playwright configured for Wails server build.
- Test suite runs in CI successfully.
- Six critical user flows tested.
- Mocked workers/inference engines used; no real ML models.
- Tests catch build errors via Playwright `webServer` startup validation.
- Documentation added.

## Trade-offs rejected

- Using a real worker with local llama-cpp/ollama – requires ML models and is too slow for CI.
- Testing native tray/OS menus with Playwright – impossible per Wails docs; out of scope.
- Using `wails3 run -tags server` instead of explicit `go build` – the Playwright `webServer.command` requires a manually controllable binary; `run` would conflict with Wails' own dev server.

## Acceptance Criteria (from #180)

- [ ] Playwright configured for Wails server build.
- [ ] Test suite runs in CI successfully.
- [ ] At least 3-5 critical user flows tested.
- [ ] Mocked workers/inference engines for testing (no real ML).
- [ ] Tests catch build errors (first priority).
- [ ] Documentation on running tests locally.

## Risks / open

- **SystemTray panic in server mode** – if `app.SystemTray.New()` panics for server builds, the server build will fail. Mitigation: build-tag guard or runtime check before SystemTray usage. Estimated risk: medium.
- **Port collisions in CI** – router mock on 8081, app server on 8080, mock backend on 9000. Use dynamic ports from env vars.
- **Wails server mode startup time** – Playwright's `webServer.timeout` must be >= 120s for first cold build.
- **Binding drift** – `wails3 generate bindings -f '-tags=server'` must be run before frontend build; mismatches cause TS compilation errors.
- **Wails version mismatch** – CI currently uses `wails3@v3.0.0-beta.24` in `app-bindings` job, but `app/go.mod` pins to `v3.0.0`. Ensure all CI jobs use the same version (v3.0.0).
- **Mock router fidelity** – RouterClient expects exact JSON shapes (`WorkersResponse`, `PopularModelsResponse`, etc.). Use real types from `pkg/router` to generate fixtures, or copy production responses from `router_client_test.go`.

## Next step

Await user approval of this plan (approach + files + trade-offs). Upon approval, dispatch `dev-team-lead` with this plan as the Research Brief and constraint summary. Pipeline: team-lead → dev-team-lead → dev-architect → backend-engineer/frontend-engineer → test-engineer → code-reviewer → devops-verificator (if CI changes). Post-verification: `devops-cleanup` + `post-mortem-analyst`.

> 🧠 **From Hindsight memory (kp-9b8bb40070e8468085ecfb3b1378920e)** — Testing conventions in this project include table-driven tests; E2E tests for the GUI should follow the existing smoke test pattern in `app/smoke_test.go`.
