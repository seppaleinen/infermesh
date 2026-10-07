# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
- Distinct-client counting, loaded-vs-advertised distinction deferred to v2 (now shipped)