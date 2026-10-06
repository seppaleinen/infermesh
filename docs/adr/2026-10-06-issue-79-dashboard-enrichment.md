# ADR: Dashboard Enrichment — Router Health, Worker Count, Popular Models, Local Worker Badge

**Status:** Accepted

**Date:** 2026-10-06

## Context

Issue #79 requested richer information in the InferMesh Wails desktop app Dashboard:
- Router health indicator (reachable / unreachable)
- Worker count summary
- Popular models list (model name, worker count, call count over 10-minute window)
- Local worker identification ("You" badge on the worker supervised by this desktop app)

The `/meta/models/popular` endpoint already exists on the router (added in Issue #31), returning `PopularModelsResponse{Models: []PopularModelInfo{Model, WorkerCount, CallCount}}` sorted by `worker_count` descending. The call counter uses a rolling 10-minute window implemented in `pkg/router/call_counter.go`.

## Decision

**Frontend-only v1 implementation** using existing endpoints — no router/backend changes.

- **Dev-mode HTTP only**: The desktop app talks to the router over plain HTTP (`http://127.0.0.1:8080` by default). No mTLS, no prod-mode changes. Prod-mode mTLS remains deferred to Issues #9/#10.
- **Local worker detection by port match**: A worker is "local" when `worker.Port == settings.WorkerPort`. This is reliable in dev mode where both router and supervised worker run on the same machine. Hostname-based detection was rejected as less reliable (mDNS can resolve to different hostnames; port is stable).
- **Popular models from `/meta/models/popular` as-is**: Reuse the existing router endpoint. The endpoint already sorts by `worker_count` descending. The frontend preserves this order (no re-sort).
- **Worker count shown as clients for v1**: The `worker_count` field represents number of workers advertising the model (regardless of `Loaded` state). Distinct-client counting is deferred to v2 (Q2 decision).
- **Follow existing patterns**: Mirrored `useWorkers` composable, `WorkerCard` component, and `RouterClient` Go service structure.

## Consequences

### In Scope (v1)
- Router health card (green/red indicator in Dashboard header)
- Worker count badge (number of connected workers)
- Popular models table (model, workers, calls-10m)
- "You" badge on the locally-supervised worker card
- All tests passing: `cd app && go test ./...` (180 passed), `npm run build`, `make lint`, `make build-app`

### Deferred to v2
- Distinct client counting (vs. worker count) — requires router changes
- Loaded-vs-advertised model distinction — requires `/v1/models` enrichment or new endpoint
- Prod-mode mTLS for the desktop app — requires cert management in Wails context
- Settings persistence for router URL (Issue #43) — currently in-memory only
- WebSocket-based live updates — polling at 5s interval for v1

### Alternatives Considered

| Alternative | Rejected Because |
|-------------|------------------|
| Backend changes (new endpoints) | Q7: Frontend-only for v1; router contract already sufficient |
| New `/meta/models/popular` shape | Q5: Reuse existing endpoint as-is |
| Hostname-based local detection | Q3: Port match is more reliable in dev mode (same machine, known port) |
| Client-side re-sort of popular models | Q6: Keep router's `worker_count` desc sort; frontend preserves order |

## Implementation Summary

### Go Layer (`app/`)
- `router_client.go`: Added `PopularModelView` struct, `popularModelsPath()`, `GetPopularModels(ctx)`, `parsePopularModelsResponse(body)`, `IsLocalWorker(w, workerPort)`
- `main_test.go`: Added `TestRouterClientPopularModelsURL`, `TestRouterClientPopularModelsParse`, `TestIsLocalWorker`

### Auto-generated Bindings
- `app/frontend/bindings/.../models.ts`: `PopularModelView` interface
- `app/frontend/bindings/.../routerclient.ts`: `GetPopularModels()` function
- `app/frontend/bindings/.../index.ts`: `PopularModelView` type export

### Vue Frontend (`app/frontend/src/`)
- `composables/usePopularModels.ts`: New composable mirroring `useWorkers` (polling, error states, retry)
- `components/PopularModelsTable.vue`: Presentational table (Model | Workers | Calls 10m)
- `components/WorkerCard.vue`: Added optional `isLocal` prop + "You" badge styling
- `App.vue`: Wired `usePopularModels`, `useSettings`, `PopularModelsTable`, local-worker detection via `isLocalWorker(w) => w.port === settings.worker_port`

## Verification

All gates pass:
- `cd app && go test ./...` — 180 passed
- `cd app/frontend && npm run build` — vue-tsc + vite clean
- `make lint` — 0 issues
- `make build-app` — clean build
- Code reviewer: [SUCCESS] (advisory MINOR: dead `IsLocalWorker` not bound to frontend; `firstLoadTimedOut` unused)
- Security auditor: [SUCCESS] (no BLOCK/MAJOR; MINOR: dead `httpTimeout` const; bounded error-body leakage pre-existing pattern)