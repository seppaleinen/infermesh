# ADR: Loaded-vs-Advertised Model Distinction — Issue #79 v2

**Status:** Accepted

**Date:** 2026-10-06

## Context

Issue #79 requested richer information in the Wails desktop app Dashboard, including "what models are exposed from other hosts (sorted by most common) and maybe which are being used/loaded". The existing `/meta/models/popular` endpoint (Issue #31) returns `PopularModelInfo{Model, WorkerCount, CallCount}` where `WorkerCount` counts workers advertising the model regardless of `Loaded` state. To distinguish "model is known" from "model is live and serving", the popular view needs to show, per model, how many workers have it loaded versus how many merely advertise it.

The router already tracks the `Loaded` flag per `protocol.ModelInfo` in the capability cache. The `handlePopularModels` loop iterates over every `worker.Capabilities.Models[i]` and already increments `WorkerCount`. Adding a second counter for `Loaded` models is a trivial O(1) change in the existing cache lock — no new round-trips, no new data fetches.

## Decision

**Backend contract change**: enrich `/meta/models/popular` with a new `loaded_worker_count` field.

- `PopularModelInfo` gains `LoadedWorkerCount int` `json:"loaded_worker_count"`
- In `handlePopularModels`, inside the existing per-model aggregation loop, increment `loadedWorkerCount` when `m.Loaded`
- The JSON response is a strict superset — existing clients ignore the new field
- No version bump needed; backward compatibility is preserved

## Consequences

### In Scope (v2)
- `/meta/models/popular` response includes `loaded_worker_count` per model
- Desktop app `PopularModelView` mirrors the new field via Wails bindings
- `PopularModelsTable.vue` gains a "Loaded" column showing `loaded_worker_count / worker_count` (e.g. `2 / 5`) with tooltip "loaded on N of M workers"
- Pool summary line in `App.vue` appends "· Y loaded" where Y = count of models with `loaded_worker_count > 0`
- Unit tests verify the split (loaded vs advertised) across workers sharing a model
- All verification gates pass

### Out of Scope (v3 and later)
- Distinct client counting (TCP connection count handled separately in v3)
- Prod-mode mTLS for the desktop app — requires cert management in Wails context
- Settings persistence for router URL (Issue #43) — currently in-memory only

## Implementation Summary

### Backend (`pkg/router/`)
- `server.go`: Added `LoadedWorkerCount int` to `PopularModelInfo`; added `loadedCount` to local `modelStats`; increment in loop when `m.Loaded`; set in conversion.
- `server_test.go`: Added `TestPopularModelsLoadedVsAdvertised` asserting loaded-vs-advertised splits (2/1, 2/2, 1/0) for shared/both-loaded/only-advertised models.

### Go Layer (`app/`)
- `router_client.gp`: Added `LoadedWorkerCount int` to `PopularModelView`; mapped in `parsePopularModelsResponse`
- `main_test.go`: Added subtests for `loaded_worker_count` present (=1) and absent (=0, backward compat)

### Frontend (`app/frontend/src/`)
- `bindings/.../models.ts`: Regenerated — `PopularModelView` interface now includes `loaded_worker_count: number`
- `components/PopularModelsTable.vue`: Added "Loaded" column header; render `loaded_worker_count / worker_count` with tooltip
- `App.vue`: Extended workers-sub line to append `· Y loaded` where Y = count of models with `loaded_worker_count > 0`

## Verification

All gates pass:
- `go test ./pkg/router/...` — 146 passed
- `cd app && go test ./...` — 189 passed
- `npm run build` — vue-tsc + vite clean
- `make lint` — 0 issues
- `make build-app` — clean build

## Related ADRs
- ADR: Dashboard Enrichment — Router Health, Worker Count, Popular Models, Local Worker Badge (2026-10-06) — v1
- ADR: Active HTTP Connection Count — Issue #79 v3 (2026-10-07) — v3