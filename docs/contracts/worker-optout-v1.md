# ADR: Worker Model Opt-Out (Issue #37)

> **Status:** Accepted
>
> Related to `pkg/protocol/worker.go`, `pkg/worker/server.go`, `pkg/worker/run.go`,
> `pkg/scheduler/scheduler.go`, `pkg/router/server.go`, and `cmd/worker/main.go`.

---

## Context

Workers in the InferMesh pool may be unwilling to serve particular models even
when those models are loaded in their backend. Concrete motivations:

- A worker runs on metered GPU bandwidth and wants to reserve itself for a
  private/high-priority model, declining all other traffic.
- An operator wants to drain a specific model from a node without unloading
  the backend (e.g. during a rolling model update or capacity re-balance).
- A shared machine hosts a worker whose owner does not want it advertised for
  models it merely knows-about but has not consented to serve.

Without an opt-out mechanism, the router's scheduler treats every loaded model
as fair game: `SelectWorker` scores any worker that advertises the model and
has sufficient VRAM, and the fallback paths in the router (`advertising-model`,
`any-available-worker`) will route to a worker that does not want the load. The
only existing levers are load-based (queue depth, VRAM, quantization) — there
is no operator-asserted *willingness* signal.

---

## Decision

Add an explicit, operator-declared **willingness filter** carried in
`Capabilities` and enforced by the scheduler. Two complementary lists:

- `AllowedModels` — a **whitelist** of models the worker *is* willing to serve.
- `ExcludedModels` — a **blacklist** of models the worker *is not* willing to
  serve.

### Precedence rule

1. If `AllowedModels` is non-empty, the worker serves **only** models in that
   list; `ExcludedModels` is ignored entirely.
2. If `AllowedModels` is empty and `ExcludedModels` is non-empty, the worker
   serves all models **except** those listed.
3. If both are empty/nil, the worker is willing to serve everything
   (backward compatible — no behavior change for existing workers).

### Schema (`pkg/protocol/worker.go`)

```go
type Capabilities struct {
    GPU     GPUInfo     `json:"gpu"`
    Models  []ModelInfo `json:"models"`
    Engines []string   `json:"engines"`
    VRAM    MemoryInfo  `json:"vram"`
    System  MemoryInfo  `json:"system"`

    // AllowedModels is a whitelist of models this worker is WILLING to serve.
    // When non-empty, the router only schedules requests for models in this list;
    // it takes precedence over ExcludedModels (which is then ignored).
    AllowedModels  []string `json:"allowed_models,omitempty"`
    // ExcludedModels is a blacklist of models this worker is NOT WILLING to
    // serve. Applies only when AllowedModels is empty.
    ExcludedModels []string `json:"excluded_models,omitempty"`
}
```

Both fields use `omitempty`, so workers that do not set them serialize
identically to today — no protocol breakage.

### CLI surface (`cmd/worker/main.go`)

```
--allowed-models <m1,m2,...>   comma-separated whitelist; empty = all
--excluded-models <m1,m2,...>  comma-separated blacklist; applies only when
                                --allowed-models is empty
```

Flags are comma-separated; values are split and trimmed of whitespace inside
`pkg/worker/run.go` (`splitModelList`). They flow through `RunConfig` into
`Server.SetModelFilters` / `ApplyModelFilters`.

### Advertisement path

Filters are declared **statically** at worker startup and are immutable for the
lifetime of the process:

- Emitted in the `/capabilities` HTTP response.
- Copied into `WorkerInfo.Capabilities` on registration — through mDNS TXT
  records, dev-mode HTTP registration, prod-mode mTLS registration, and the
  relay WebSocket path (`pkg/router/server.go` `toSchedulerWorker` copies both
  flags for every registration path).

### Enforcement point

The scheduler applies opt-out filtering **before scoring**, so an unwilling
worker is never even considered for scoring:

```go
// pkg/scheduler/scheduler.go
func SelectWorker(...) {
    available := FilterUnavailable(workers, unavailableTTL)
    available = FilterByAllowedModels(request, available) // <-- opt-out here
    // ... then Score()
}
```

`FilterByAllowedModels(request, workers)` implements the precedence rule
exactly as above: whitelist-governs when set, blacklist-governs when whitelist
is empty, pass-through otherwise. Matching is exact and case-sensitive,
matching the model-name equality already used by `FilterByModel` and `Score`.

### Router fallback paths

When the primary model-specific candidate list is empty, the router attempts two
fallbacks (`pkg/router/server.go`):

- **advertising-model** — workers that advertise the requested model but have
  not loaded it yet. Opt-out filters are applied so the router will not hand
  out a model the worker declined.
- **any-available-worker** — last-resort selection across all workers with
  capacity. Opt-out filters are applied so an unwilling worker is never returned
  here either.

This guarantees no code path routes a request to a worker that opted out.

### Testing

- `pkg/scheduler/scheduler_test.go` — `TestFilterByAllowedModels` (whitelist,
  blacklist, both-empty, both-set, unknown-model).
- `pkg/worker/server_test.go` — `TestCapabilitiesHandler_IncludesModelFilters`,
  `TestCapabilitiesHandler_NoFiltersWhenUnset`.
- `pkg/worker/run_test.go` — `TestRunWorker_AllowedExcludedModels`.
- `tests/e2e/worker_optout_test.go` — 13 end-to-end subtests covering
  whitelist-only, blacklist-only, combined, fallback-path filtering, and
  backward-compatible (unset) behavior.

---

## Consequences

### Positive

- **Operator control** — a node's willingness to serve a model is an explicit
  decision, not inferred from what happens to be loaded.
- **No router-side config** — the opt-out travels with the worker's
  `Capabilities`, so the router needs no per-model ACL table.
- **Backward compatible** — empty lists (the zero value) reproduce today's
  behavior; legacy workers that don't set `AllowedModels`/`ExcludedModels` are
  unaffected.
- **Defense in depth** — filtering runs before scoring (not as a tie-breaker)
  and is re-applied in every router fallback path, so an unwilling worker can
  never be selected.
- **Zero new dependencies** — stdlib + the existing `pkg/protocol` plumbing;
  no change to go.mod.

### Negative

- **Static, not dynamic** — filters are set at startup and cannot be changed
  without restarting the worker. A live toggle (e.g. via a runtime API) is
  intentionally out of scope for this MVP.
- **Exact-match only** — model names must match exactly (case-sensitive).
  Globbing / prefix matching is deferred.
- **Whitelist blindness** — when `--allowed-models` is set, the worker still
  advertises all loaded models in `/capabilities` for discovery purposes; the
  router must consult the filter lists rather than assuming "advertised =
  willing." This is by design (the model may be loaded for another reason) but
  is a subtle invariant callers must respect.

### Net

A minimal, protocol-additive mechanism that lets operators constrain the model
surface a worker exposes, enforced at the single dispatch chokepoint
(`SelectWorker`) plus the router's fallback paths, without changing the
scoring algorithm or adding dependencies.

---

## Alternatives Considered

### A. Router-side model ACL

Maintain a per-worker model allowlist on the router (e.g. static config or an
`Admit/Reject` RPC). Rejected: reintroduces a centralized ACL that the design
brief (#3, discovery-v1.md) explicitly avoids in favor of worker self-
declaration; adds router state that does not survive restarts.

### B. Dynamic runtime filter via HTTP API

Add `POST /models/filter` to mutate the filter at runtime. Rejected for MVP:
the worker's model set is already declared statically alongside
`--model-path`, so a static flag is the minimal consistent surface. Dynamic
mutation can be layered on later without protocol changes (same
`Capabilities.AllowedModels` schema).

### C. Per-request load-decline (backend returns 429 / 503)

Let the worker accept the request and push back if it does not want to serve
the model. Rejected: wastes router dispatch effort and creates latency before
the rejection; also impossible to signal on fallback paths that route to a
worker advertising-but-not-loaded a model. Opt-out at the scheduler level is
cheaper than recover-at-the-backend.

### D. Glob / regex model matching

Match `--allowed-models` against model-name patterns. Rejected: introduces a
dependency on glob/regex semantics and ambiguity in matching priority. Exact
names are unambiguous and can be extended to pattern matching later without
changing the precedence rule.

---

## References

- `docs/contracts/discovery-v1.md` — discovery contract that defines the
  `Capabilities` type this ADR extends.
- `pkg/scheduler/scheduler.go` — `FilterByAllowedModels`, `SelectWorker`.
- `pkg/worker/run.go` — `RunConfig.AllowedModels`/`ExcludedModels`,
  `splitModelList`, `ApplyModelFilters`.
- Issue #37 (worker opt-out of serving models).
