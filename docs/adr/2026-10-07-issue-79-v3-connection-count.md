# ADR: Active HTTP Connection Count — Issue #79 v3

**Status:** Accepted

**Date:** 2026-10-07

## Context

Issue #79 requested richer information in the Wails desktop app Dashboard, including "how many other clients are connected". Investigation showed the supervised desktop app is always the sole client, making "distinct clients" a misleading metric. Instead, an honest signal is the number of active TCP connections the router is currently holding — this reflects load and liveness, works in dev mode (no auth), and requires no new auth-dependent definitions.

The router already exposes `/meta/models/popular` as an in-memory stat endpoint. Following that pattern, a new endpoint `/meta/connections/count` returning `{ "connections": int }` is added, where the count is maintained via an `http.ConnState` hook on the router's `http.Server`. The hook increments on `http.StateNew` and decrements on `http.StateClosed` (covering plain HTTP and hijacked WebSocket connections); `http.StateIdle` and `http.StateHijacked` are deliberately untouched so keep-alive connections are not double-counted.

## Decision

**New endpoint**: `GET /meta/connections/count` returning `{ "connections": int }` (JSON).

- Router `Server` struct gains an `atomic.Int64` field `connections`
- `http.ConnState` hook attached BEFORE `ListenAndServe()`/`ListenAndServeTLS()` so no connections are missed
- Hook logic: `case http.StateNew: s.connections.Add(1); case http.StateClosed: s.connections.Add(-1);`
- Endpoint handler returns JSON `{ "connections": int(s.connections.Load()) }`, 405 for non-GET
- The count is TCP sockets — not a distinct-client count (documented in code comment)
- Backward compatibility: new endpoint is additive; existing clients unaffected

## Consequences

### In Scope (v3)
- `/meta/connections/count` endpoint exposes active TCP connection count
- Desktop app `RouterClient.GetConnectionCount()` mirrors the endpoint (same pattern as `GetPopularModels`)
- New `useConnectionCount` composable (5s poll, 4s first-load, loading/empty/ready/error state machine, retry)
- Dashboard badge now shows "router online · X workers · Y connections" next to the existing worker-count badge
- Unit tests verify the counter increments/decrements correctly under concurrent dials
- Integration test hits the endpoint and asserts live tracking
- All verification gates pass

### Out of Scope (v4 and later)
- Distinct client counting (requires auth-bound identity — mTLS CN or API key — deferred to v4)
- Prod-mode mTLS for the desktop app — requires cert management in Wails context
- Settings persistence for router URL (Issue #43) — currently in-memory only

## Implementation Summary

### Backend (`pkg/router/`)
- `server.go`: Added `connections atomic.Int64` field with doc comment "active HTTP connections; not a distinct-client count"; added `ConnState` hook in `Start()`; added `handleConnectionCount`; registered route `/meta/connections/count`
- `server_test.go`: Added `TestConnStateCounter` unit test simulating state transitions; added `TestClientCountEndpointIntegration` integration test with real TCP listener

### Go Layer (`app/`)
- `router_client.go`: Added `clientCountPath()` builder; added `GetClientCount(ctx)` mirroring `GetPopularModels`; added `parseClientCountResponse(body)` pure helper
- `main_test.go`: Added URL-builder tests and parse tests (valid counts, malformed JSON, unknown fields ignored)

### Frontend (`app/frontend/src/`)
- `composables/useConnectionCount.ts`: New composable mirroring `usePopularModels` (polling, error states, retry, cleanup)
- `App.vue`: Imports composable, starts/stops on mount, renders badge: `· {{ connectionCount.state.value.count }} connection<span v-if="... !== 1">s</span>`

## Verification

All gates pass:
- `go test ./pkg/router/...` — 146 passed
- `cd app && go test ./...` — 189 passed
- `npm run build` — vue-tsc + vite clean
- `make lint` — 0 issues
- `make build-app` — clean build

## Related ADRs
- ADR: Dashboard Enrichment — Router Health, Worker Count, Popular Models, Local Worker Badge (2026-10-06) — v1
- ADR: Loaded-vs-Advertised Model Distinction — Issue #79 v2 (2026-10-06) — v2