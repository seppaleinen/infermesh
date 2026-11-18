# Architecture Contract: Per-Model Concurrency Badge (Issue #206)

## STATUS
[SUCCESS]

## SUMMARY
Delivered an architecture contract for transitioning the worker's concurrency badge from a single worker-wide scalar to a per-model budget. The contract specifies changes across 7 files spanning protocol, worker estimator, scheduler, router dispatch, meta handlers, and the desktop app UI — preserving backward compatibility with old workers/routers.

## RATIONALE
Different models saturate GPU at different parallelism levels (e.g., 12B models at 1–2 concurrent, 4B at 4–8). The current single `ConcurrencyCap` per worker forces a one-size-fits-all limit that either under-utilizes small models or overloads large ones. Making the cap per-model lets the UI, scheduler, and worker enforcement reflect the true budget for each model independently. The existing per-worker estimator, persistence, and scheduler logic are the foundation — this contract extends them with model-keyed granularity.

## TECHNICAL PAYLOAD

---

### 1. Backend API Spec

#### Protocol Wire Format (`pkg/protocol/worker.go`)
```go
// ModelInfo gains an optional per-model cap
type ModelInfo struct {
    Name           string `json:"name"`
    Loaded         bool   `json:"loaded"`
    // ... existing fields ...
    ConcurrencyCap int    `json:"concurrency_cap,omitempty"` // NEW
}
```
- `omitempty` ensures old workers without the field are valid.
- `Capabilities.Models` is a `[]ModelInfo`; each entry carries its own `ConcurrencyCap`.
- `WorkerInfo.ConcurrencyCap` (top-level, existing) remains as **worker-wide default/fallback** for backward compat.

#### Worker Registration (`/v1/workers` registration payload from worker → router)
- Unchanged structure; `Capabilities.Models[i].ConcurrencyCap` now populated by worker before each registration/heartbeat.
- Old workers: field absent → router treats as 0.

#### Router Meta Endpoint: `GET /v1/workers`
Response: `WorkersResponse{Workers: []WorkerInfo}` where each `WorkerInfo` includes:
```go
type WorkerInfo struct {
    ID              string       `json:"id"`
    // ... existing ...
    ConcurrencyCap  int          `json:"concurrency_cap"`        // worker-wide fallback
    AdvertisedModels []ModelInfo `json:"advertised_models"`      // from #205; each ModelInfo has ConcurrencyCap
}
```
**Projection rule (server-side, in `handlers_meta.go`):**
- For each model in `AdvertisedModels`:
  - If `m.ConcurrencyCap > 0` → emit as-is.
  - Else if `worker-wide ConcurrencyCap > 0` → emit `m.ConcurrencyCap = worker-wide` (fallback fill).
  - Else omit (0 with omitempty).
- Worker-wide `ConcurrencyCap` always included (for UI fallback on old workers).

**Error states:** None new. Missing fields gracefully fall back.

---

### 2. Frontend Component Spec

#### `app/router_client.go` (Go bindings for `/v1/workers`)
```go
type ModelInfo struct {
    Name           string `json:"name"`
    Loaded         bool   `json:"loaded"`
    ConcurrencyCap int    `json:"concurrency_cap,omitempty"`
}

type WorkerInfo struct {
    ID              string      `json:"id"`
    // ... existing ...
    ConcurrencyCap  int         `json:"concurrency_cap"`        // worker-wide
    AdvertisedModels []ModelInfo `json:"advertised_models"`      // per-model caps
}
```

#### `WorkerCard.vue` (UI)
- **Existing:** Worker-wide "X/Y calls" row (in-flight / cap) — **keep unchanged**.
- **New:** Each model chip (from `AdvertisedModels`) renders an inline badge:
  - **Chip label:** `{modelName} • {concurrencyCap}` (e.g., `gemma-2-9b • 4`).
  - **Tooltip on hover:** "Concurrency budget for this model: {cap} parallel requests".
  - **Empty cap:** If `ConcurrencyCap === 0`, show nothing (or `• auto` if worker-wide > 0 — use worker-wide as display fallback).
- **Props:** `worker: WorkerInfo` (already passed). Reads `worker.advertised_models[]?.concurrency_cap`.
- **State:** None (pure projection).
- **Interaction:** None (read-only display).
- **Placement:** Inside the existing model-chip container (flex wrap). Badge is a small pill (same height as chip, muted background).

#### TypeScript check: `vue-tsc --noEmit` must pass.

---

### 3. Data Model Impact

#### `pkg/protocol/worker.go`
- Add `ConcurrencyCap int` to `ModelInfo` (line ~81 area). JSON tag `concurrency_cap,omitempty`.
- No migration needed — additive, optional field.

#### `pkg/worker/concurrency_estimator.go` — **Core change**
**New type:**
```go
type ModelConcurrencyEstimator struct {
    mu           sync.Mutex
    estimators   map[string]*ConcurrencyEstimator // key = model name (backend model name)
    persistPath  string
    defaultCap   int                              // worker-wide base cap (from capabilities/VRAM)
}
```

**Methods (all take `model string`):**
- `RecordRequest(model string, latency time.Duration, err error)` — records for that model; lazily creates estimator if absent.
- `RecordCircuitOpen(model string)` — circuit breaker trip for that model.
- `RecordCircuitClosed(model string)` — circuit recovery for that model.
- `Cap(model string) int` — returns cap for model; if model unknown or cap 0 → returns `defaultCap`.
- `DefaultCap() int` — returns `defaultCap` (used for worker-wide `WorkerInfo.ConcurrencyCap` registration field).
- `recompute(model string)` — recomputes single model's cap from its 429/latency history.
- `Start(ctx context.Context)` — ticker (e.g., 30s) that calls `recompute` for **all keys in `estimators`** + any models seen in `InferenceTracker`.

**Persistence format (`concurrency_cache.json`):**
```json
{
  "modelA": 4,
  "modelB": 2
}
```
- Single JSON object mapping model name → cap.
- **Migration:** On load, if file contains a legacy scalar (old format), treat that value as `defaultCap` and seed all known models with it. Write new map format on next persist.

**Model discovery signal:**
- At `RunWorker`/`startBackend` time, the backend's model list is known → pre-seed `estimators` map with entries for each model (cap = `defaultCap` initially).
- `InferenceTracker` (exposed via `server.GetInferenceTracker()`) provides observed model names → lazily create estimator entries on first `RecordRequest`.

#### `pkg/worker/server.go`
- `RunWorker` / `startBackend`: create `ModelConcurrencyEstimator` with `defaultCap` derived from GPU/VRAM (existing logic). Pre-seed from backend's advertised models.
- `handleRequest` / `streamOrFallback`: extract model name from request (**resolved backend model name**, not alias — aliases are router-side). Call:
  ```go
  est.RecordRequest(model, latency, err)
  if errors.Is(err, ErrCircuitOpen) { est.RecordCircuitOpen(model) }
  if wasOpen && err == nil { est.RecordCircuitClosed(model) }
  ```
- Before each registration/heartbeat (`buildRegistrationInfo` or equivalent): for each `Capabilities.Models[i]`, set `ConcurrencyCap = est.Cap(modelName)`.

#### `pkg/scheduler/scheduler.go`
- **Scheduler worker struct** (internal, not protocol): add `ModelMaxInFlight map[string]int`.
- **Fallback chain in `Score` / `scoreWorker` for a given `model`:**
  1. `ModelMaxInFlight[model]` if present and > 0.
  2. `MaxInFlight` (worker-wide) if > 0.
  3. Package constant `DefaultMaxInFlight` (existing fallback, currently 4).
- **In-flight ratio:** `inFlightRatio = min(float64(inFlight) / float64(cap), 1.0)` where `inFlight` is **worker-wide total in-flight** (unchanged). *Assumption: per-model in-flight tracking is out of scope; worker-wide in-flight + per-model cap is a conservative approximation clamped at 1.0.*
- `toSchedulerWorker` (in `dispatch.go`) populates `ModelMaxInFlight` from `w.Capabilities.Models`.

#### `pkg/router/dispatch.go`
- `toSchedulerWorker(w *protocol.WorkerInfo) SchedulerWorker`:
  ```go
  sw.ModelMaxInFlight = make(map[string]int)
  for _, m := range w.Capabilities.Models {
      if m.ConcurrencyCap > 0 {
          sw.ModelMaxInFlight[m.Name] = m.ConcurrencyCap
      }
  }
  sw.MaxInFlight = w.Capabilities.ConcurrencyCap // worker-wide fallback
  ```

#### `pkg/router/handlers_meta.go`
- `handleWorkersList` projection (see API spec above): for each worker's `AdvertisedModels`, apply fallback fill from worker-wide `ConcurrencyCap`.

---

### 4. Integration Verification Criteria

| Area | Test / Check |
|------|--------------|
| **Protocol** | `go build ./pkg/protocol` — `ModelInfo.ConcurrencyCap` serializes with `omitempty`; old JSON without field decodes cleanly. |
| **Worker estimator** | `go test ./pkg/worker -run ConcurrencyEstimator` — unit tests for: `RecordRequest` isolation (429 on model A lowers A's cap, B unchanged), `Cap(model)` fallback to default for unknown model, `Start` ticker recomputes all, persistence round-trip (`{"m": cap}`), legacy scalar migration. |
| **Worker server** | `go test ./pkg/worker -run TestWorkerRegistration` — registration payload includes per-model caps; request handlers call estimator with correct resolved model name; pre-seed from backend model list. |
| **Scheduler** | `go test ./pkg/scheduler -run TestScorePerModelCap` — per-model cap selected, fallback chain works, ratio clamp at 1.0. |
| **Router dispatch** | `go test ./pkg/router -run TestToSchedulerWorker` — `ModelMaxInFlight` map built from `Capabilities.Models`. |
| **Meta handlers** | `go test ./pkg/router -run TestWorkersListProjection` — `/v1/workers` JSON includes per-model caps; fallback fill applied. |
| **App Go bindings** | `go test ./app -run TestRouterClient` — `WorkerInfo`/`ModelInfo` unmarshal with per-model caps. |
| **Frontend** | `cd app && vue-tsc --noEmit` — no type errors; `WorkerCard.vue` renders badge on each chip. |
| **Integration** | `make test-integration` — two workers, one with 2 models (4B and 12B); dispatch to 12B model saturates at cap 2, 4B at cap 4; old-format worker (no per-model caps) falls back to worker-wide. |
| **E2E** | `make test-e2e` — full loop: worker starts → registers with per-model caps → router schedules respecting per-model caps → UI shows per-model badges. |
| **Backward compat** | Mixed old/new workers + router: no panics, no missing-field errors; old worker gets worker-wide scheduling; new worker with old router (ignores unknown JSON fields) works. |
| **Lint/vet** | `make lint` and `go vet ./...` pass. |

#### Edge Cases to Verify
1. **Model not in estimator map** (never seen before request) → `RecordRequest` lazily creates entry with `defaultCap`, then records.
2. **Alias handling** — worker uses **resolved backend model name** for estimator key; router registers backend model name (not alias) → keys match. Alias normalization is router-side (#207) and does not affect worker estimator.
3. **Worker heartbeat re-registration** — caps update dynamically; `Capabilities.Models[i].ConcurrencyCap` reflects latest `est.Cap(model)` at each heartbeat.
4. **Persistence corruption / missing file** → `defaultCap` used, new file written on next persist.
5. **Worker with zero models** → empty `ModelMaxInFlight`, falls back to worker-wide.
6. **InFlight = 0, cap = 0** → fallback to default; ratio = 0.

---

## TRACE
team-lead → dev-team-lead → dev-architect [SUCCESS]

## FAST_PATH_SKIP
N/A — this is a design contract, not an implementation.

## IMPORTANT NOTES
- **Assumption: in-flight remains worker-wide.** Per-model in-flight tracking would require changes to `request_tracker` and is explicitly out of scope (7-file limit). The clamp at 1.0 keeps the scheduler safe.
- **Estimator key = backend model name.** The router registers the backend model name (from `/v1/models`); aliases are a router-side mapping (#207) and do not affect worker-side estimator keys.
- **Worker-wide `ConcurrencyCap` on `WorkerInfo` stays** as the fallback/default for old workers and for the UI when per-model is missing.
- **Persistence migration is one-way.** Legacy scalar → map format on first load; subsequent writes are map format.
- **No changes to `Loaded` semantics, `InferenceTracker` heuristic, cold-load fix, or alias handling (#207).**