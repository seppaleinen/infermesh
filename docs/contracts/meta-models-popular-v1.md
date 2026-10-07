# Contract: GET /meta/models/popular (Issue #31)

**Status:** Accepted

**Date:** 2026-09-21 (per AGENTS.md notes)

## 1. Overview

The `/meta/models/popular` endpoint returns a rolling-window popularity summary of models across the InferMesh pool, sorted by how many workers advertise each model (worker count). It is used by the Wails desktop app to display a "Popular Models" table in the Dashboard.

The data is derived from the router's in-memory capability cache, which is updated on worker registry events (heartbeats, registrations, unregistrations). The window is not time-based; it reflects the current state of the pool at query time.

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
- **Call count**: maintained by `pkg/router/call_counter.go`; increments on each inference request, decays over a 10-minute sliding window.
- **Staleness**: the data is as fresh as the last capability-cache update (typically within the worker heartbeat TTL, 30s by default).
- **Prod/dev**: the endpoint is available in both dev and prod mode; no auth is required for this metrics endpoint.

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