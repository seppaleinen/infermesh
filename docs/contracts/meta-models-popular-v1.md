# Contract: GET /meta/models/popular (Issue #31)

**Status:** Accepted

**Date:** 2026-09-21 (per AGENTS.md notes)

## 1. Overview

The `/meta/models/popular` endpoint returns a rolling-window popularity summary of models across the InferMesh pool, sorted by how many workers advertise each model (worker count). It is used by the Wails desktop app to display a "Popular Models" table in the Dashboard.

The data is derived from the router's in-memory capability cache, which is
updated on worker registry events (heartbeats, registrations,
unregistrations). The popularity summary is computed over a rolling 10-minute
window maintained by `pkg/router/call_counter.go`; events older than the
window are pruned (not decayed) on each record and on a periodic ticker.

## 2. Endpoint

```
GET /meta/models/popular
```

## 3. Request

No query parameters or request body.

## 4. Response

On success, returns HTTP 200 with JSON body:

```json
{
  "Models": [
    {
      "model": "string",
      "worker_count": int,
      "call_count": int
    }
  ]
}
```

- `model`: model identifier (e.g. `"gpt-oss-20b"`)
- `worker_count`: number of workers advertising this model (regardless of whether the model is currently loaded on that worker)
- `call_count`: number of inference requests for this model in the last 10 minutes (rolling window)

The array is sorted in descending order by `worker_count`. Ties are broken by model name lexicographically (ascending) for deterministic output.

## 5. Semantics

- **Worker count**: reflects how many workers have this model in their `Capabilities.Models` advertisement. A worker may advertise a model it does not currently have loaded (e.g. due to eviction or prefetch).
- **Call count**: maintained by `pkg/router/call_counter.go`; increments on
  each inference request and is pruned (events older than the 10-minute
  rolling window removed, counts decremented) on each record and on a
  periodic ticker — it does not decay.
- **Staleness**: the data is as fresh as the last capability-cache update (typically within the worker heartbeat TTL, 30s by default).
- **Prod/dev**: the endpoint is available in both dev and prod mode. In
  prod mode the router wraps the entire HTTP mux with
  `security.NewMTLSMiddleware` (`pkg/router/server.go:562`), so every request
  — including this metrics endpoint — must present a valid client
  certificate signed by the CA. No *additional* API-key auth is required
  (this endpoint is not in `wrapInferenceEndpoints`), but mTLS alone is not
  sufficient to reach it in prod mode. In dev mode no auth is required.

## 6. Examples

### Example response
```json
{
  "Models": [
    {
      "model": "gpt-oss-20b",
      "worker_count": 5,
      "call_count": 1243
    },
    {
      "model": "llama3.1-8b",
      "worker_count": 3,
      "call_count": 892
    }
  ]
}
```

### Related endpoints
- `/meta/connections/count` (Issue #79 v3): active TCP connection count
- `/v1/models`: currently loaded models only (aggregated across workers)
- `/v1/workers`: per-worker detail including loaded model names