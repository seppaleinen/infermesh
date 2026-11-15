# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **Cold model load flapping loop (three-phase fix):**
  - Phase 1: Dev-mode worker registration no longer inflates `Loaded=true` for all backend-advertised models. Only manual `--model-path` models and recently-completed-inference models (10-min rolling window) are marked loaded. LM Studio `/v1/models` now correctly reflects reality (only the loaded model shows `Loaded=true`).
  - Phase 2: Worker `--stream-grace` (default 30s, was 5s) now uses **non-destructive fallback** — on grace expiry with zero chunks, keeps waiting on the same stream until backend idle timeout or caller cancellation. No longer cancels and retries non-streaming (which aborted LM Studio engine switches). Circuit breaker `isBackendFailure()` guard: `context.Canceled` / `DeadlineExceeded` no longer count as backend failures.
  - Phase 3: Router `--relay-call-timeout` (default 300s, was 60s const) makes the relay hard cap configurable and injectable. Unloaded-model requests in relay mode now fail fast (<1s, 503) instead of hanging 60s. Timeout budget ordering documented: `proxy-timeout` (idle, 120s) < `relay-call-timeout` (total, 300s) < worker `backend-timeout` (120s idle).
  - Security: Circuit-breaker error messages no longer leak internal backend URL in client-facing responses (retained in worker logs).
  - Fixed pre-existing data race on `Server.chatHistory` (new `chatHistoryMu`).

### Added
- Worker: `--stream-grace` flag (default 30s) for configurable streaming grace period with non-destructive fallback.
- Router: `--relay-call-timeout` flag (default 300s, 0 = legacy 60s) for configurable relay call total timeout.
- Worker: `--health-check-interval` flag for tuning the backend health check cadence (default 30s, mirrors `--model-refresh-interval`)
- Dashboard enrichment in the Wails desktop app (Issue #79 v1):
  - Router health indicator (reachable/unreachable) in Dashboard header
  - Worker count badge showing number of connected workers
  - Popular models table displaying model name, worker count, and call count over a rolling 10-minute window (data from existing `/meta/models/popular` endpoint)
  - "You" badge on the worker card for the locally-supervised worker (identified by port match against `settings.WorkerPort`)
- Dashboard enrichment (Issue #79 v2):
  - `/meta/models/popular` enriched with `loaded_worker_count` per model (strict JSON superset)
  - Popular models table gains "Loaded" column showing `loaded_worker_count / worker_count` (e.g. `2 / 5`)
  - Pool summary line appends "· Y loaded" (models where `loaded_worker_count > 0`)
- Dashboard enrichment (Issue #79 v3):
  - New endpoint `/meta/connections/count` returning active TCP connection count via `http.ConnState` hook
  - Router health badge now shows "router online · X workers · Y connections"
  - New `useConnectionCount` composable mirroring `usePopularModels`/`useWorkers`
- Go layer: `PopularModelView` struct, `GetPopularModels()`, `parsePopularModelsResponse()` in `app/router_client.go`
- Vue composables: `usePopularModels`, `useConnectionCount` (mirror `useWorkers` polling/error pattern)
- Vue components: `PopularModelsTable` (presentational), `WorkerCard` gained optional `isLocal` prop

### Changed
- `App.vue` wired in popular models polling, connection count polling, settings integration, and local-worker detection
- `pkg/router/server.go`: added `LoadedWorkerCount` to `PopularModelInfo` and atomic `connections` counter with `ConnState` hook

### Testing
- Unit tests: `TestRouterClientPopularModelsURL`, `TestRouterClientPopularModelsParse`, `TestRouterClientConnectionCountURL`, `TestRouterClientConnectionCountParse` added to `app/main_test.go`
- Unit tests: `TestPopularModelsLoadedVsAdvertised`, `TestConnStateCounter`, `TestClientCountEndpointIntegration` added to `pkg/router/server_test.go`
- All verification gates pass: `go test ./...` (189), `npm run build`, `make lint`, `make build-app`

### Notes
- v1: frontend-only, no router/backend changes
- v2: backward-compatible JSON superset; existing clients ignore `loaded_worker_count`
- v3: connection count is TCP sockets (not distinct clients); workers registering via `/v1/dev/register` are counted
- Dev-mode HTTP only; prod-mode mTLS deferred to Issues #9/#10
- Distinct-client counting, loaded-vs-advertised distinction deferred to v2 (now shipped)# Trigger CI for relay image build
# Trigger CI
