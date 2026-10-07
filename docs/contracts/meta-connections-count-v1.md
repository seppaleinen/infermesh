# Contract: GET /meta/connections/count (Issue #79 v3)

**Status:** Accepted

**Date:** 2026-10-07

## 1. Overview

The `/meta/connections/count` endpoint returns the number of active TCP connections currently held by the router's HTTP server. This reflects the router's current load and liveness, and is used by the Wails desktop app to augment the "router online" badge in the Dashboard.

The count is maintained via an `http.ConnState` hook that increments on new connections and decrements on closed connections. It is **not** a distinct-client count; a single client using keep-alive or a connection pool may hold multiple connections. The metric is honest for the supervised desktop app scenario where the app is typically the sole client.

## 2. Endpoint

```
GET /meta/connections/count
```

## 3. Request

No query parameters or request body.

## 4. Response

On success, returns HTTP 200 with JSON body:

```json
{
  "connections": int
}
```

- `connections`: number of active TCP sockets currently held by the router's `http.Server`

On invalid method (non-GET), returns HTTP 405 with JSON error body per the router's standard error format.

## 5. Semantics

- **What is counted**: each `net.Conn` accepted by the router's `http.Server` that reaches `StateNew` (connection established) increments the count; each connection that reaches `StateClosed` (connection closed) decrements the count.
- **What is NOT counted**: 
  - `http.StateIdle` (keep-alive connections waiting for next request) — left untouched to avoid double-count on subsequent requests
  - `http.StateHijacked` (WebSocket upgrades) — left untouched; the connection is still counted until the underlying TCP socket closes
- **Staleness**: the count is real-time, updated atomically on each connection state transition.
- **Prod/dev**: the endpoint is available in both dev and prod mode; no auth is required for this metrics endpoint.
- **Interpretation**: in the supervised desktop app, a baseline of 1 connection is expected (the app itself). Higher numbers indicate additional load (e.g. external API clients, worker registration traffic, probing).

## 6. Examples

### Example response (idle)
```json
{
  "connections": 0
}
```

### Example response (app connected)
```json
{
  "connections": 1
}
```

### Example response (under load)
```json
{
  "connections": 7
}
```

### Related endpoints
- `/meta/models/popular` (Issue #31): model popularity by worker count
- `/v1/queue/stats`: per-worker and pool queue depth metrics
- `/v1/workers`: per-worker detail including connection-local metrics