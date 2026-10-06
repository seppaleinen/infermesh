# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Dashboard enrichment in the Wails desktop app (Issue #79):
  - Router health indicator (reachable/unreachable) in Dashboard header
  - Worker count badge showing number of connected workers
  - Popular models table displaying model name, worker count, and call count over a rolling 10-minute window (data from existing `/meta/models/popular` endpoint)
  - "You" badge on the worker card for the locally-supervised worker (identified by port match against `settings.WorkerPort`)
- Go layer: `PopularModelView` struct, `GetPopularModels()`, `parsePopularModelsResponse()`, `IsLocalWorker()` in `app/router_client.go`
- Vue composables: `usePopularModels` (mirrors `useWorkers` polling/error pattern)
- Vue components: `PopularModelsTable` (presentational), `WorkerCard` gained optional `isLocal` prop

### Changed
- `App.vue` wired in popular models polling, settings integration, and local-worker detection

### Testing
- Unit tests: `TestRouterClientPopularModelsURL`, `TestRouterClientPopularModelsParse`, `TestIsLocalWorker` added to `app/main_test.go`
- All verification gates pass: `go test ./...` (180), `npm run build`, `make lint`, `make build-app`

### Notes
- Frontend-only v1 implementation; no router/backend changes
- Dev-mode HTTP only; prod-mode mTLS deferred to Issues #9/#10
- Distinct-client counting, loaded-vs-advertised distinction deferred to v2