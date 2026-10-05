# ADR: Auto Model Routing by Complexity (Issue #32)

> **Status:** Accepted
>
> Related to `pkg/router/server.go`, `pkg/router/classifier.go`,
> `cmd/router/main.go`, `pkg/router/classifier_test.go`,
> `pkg/router/server_auto_test.go`, and `.github/workflows/ci.yml`.

---

## Context

InferMesh routes requests to workers based on exact model name matching.
Operators have no built-in mechanism to route simple prompts to small/fast
workers and complex prompts to large/accurate workers. Some users expect a
`model="auto"` alias that the router interprets based on prompt complexity,
matching the convention of other LLM routers (LiteLLM, OpenRouter).

Without auto routing, operators must manually select a model for each request,
or run multiple workers with different model sizes and ask users to specify
the model name explicitly.

---

## Decision

Add a `model="auto"` logical alias that the router intercepts at request time.
Requests with `model="auto"` are classified by prompt complexity using a
heuristic (prompt token estimate + reasoning keywords + max-tokens signal), then
rewritten to a configured tier model before the existing `SelectWorker` pipeline
dispatches to a worker.

### Tier mapping (configurable via CLI flags)

| Tier       | Trigger                                               | Default model (if configured) |
|---|---|---|
| `simple`   | token estimate < `SimpleMaxTokens`                    | `--auto-tier-simple`          |
| `medium`   | `SimpleMaxTokens` <= tokens < `MediumMaxTokens`       | `--auto-tier-medium`          |
| `complex`  | tokens >= `MediumMaxTokens`                           | `--auto-tier-complex`         |

### Complexity classifier (MVP heuristic)

```go
func Classify(content string, maxTokens int, keywords []string) ComplexityTier
```

Heuristics (all stdlib, no new deps):

1. **Token estimate** — approximate `len(content) / 4` (4 chars ≈ 1 token).
2. **`+200` max-tokens bump** — if `maxTokens > 500`, add ~200 tokens to the
   estimate before threshold comparison.
3. **Reasoning-keyword bump** — if any of the configured keywords (e.g.
   ",let's analyze", "analyze", "reasoning") appear as a substring (case-
   insensitive bump, capped at `ComplexityTierComplex`), promote to complex.
4. **Thresholds** — configurable `SimpleMaxTokens` (default 200) and
   `MediumMaxTokens` (default 600). Below simple → tier simple; between
   simple and medium → tier medium; at or above medium → tier complex.

### Backward compatibility

- When **no `--auto-tier-*` flags are set**, the `auto` alias is a no-op: the
  router treats `model="auto"` as unknown and returns a standard error
  (`no_workers` / 404). Existing explicit model names are completely
  unaffected.
- When **at least one tier is configured**, the `auto` alias is active and
  routes according to the classifier. Non-`auto` requests are unchanged.

### Enforcement points

`pkg/router/server.go` — in `handleChatCompletions` and `handleCompletions`,
after JSON parsing and before `marshal` & `selectWorker`:

1. If `req.Model == "auto"`, run `Classify`.
2. Map tier → concrete model name via the configured tier mapping.
3. Rewrite `req.Model` to the concrete name.
4. Proceed with the existing `marshal → selectWorker → dispatch` pipeline.

The rewritten model name is what the call counter records (see Issue #31).

### `/v1/models` advertisement

When at least one `--auto-tier-*` flag is non-empty, the `/v1/models` endpoint
includes an `auto` model entry with `Loaded: true` so clients discover the
alias immediately. When no tiers are configured, `auto` is omitted from the
model list (or listed as unloaded — the behavior is an implementation detail).

### CLI surface (`cmd/router/main.go`)

```
--auto-tier-simple <model-name>      model to route to for simple prompts
--auto-tier-medium <model-name>      model to route to for medium prompts
--auto-tier-complex <model-name>     model to route to for complex prompts
--auto-simple-max-tokens <N>         token estimate threshold for simple (default 200)
--auto-medium-max-tokens <N>         token estimate threshold for medium (default 600)
--auto-reasoning-keywords <k1,k2,...> comma-separated keywords that promote
                                       a prompt to complex tier (case-insensitive)
```

Flags default to `0` / `""` (disabled). `DefaultAutoRoutingConfig()` has empty
model mappings and `DefaultAutoRoutingConfig().ResolveModel(...)` returns
`("auto", false)`.

### Enforcement depth (defense in depth)

- **Primary path**: `SelectWorker` (scheduler) sees the rewritten concrete model
  name, not `auto`. No opt-out filter is needed for `auto` since the router
  resolves it before dispatch.
- **Fallback paths**: The router's `selectWorkerExcluding` and failover paths
  also see the rewritten model name. If the tier model has no available workers,
  the router returns `no_workers` (404 / 503) as normal.

### Rationale

- **Router-side only** — no worker or scheduler changes required. The feature
  is entirely in the HTTP handler layer, preserving the existing deterministic
  scheduling and failover semantics.
- **Configurable tiers** — operator controls which models are "simple" vs
  "complex" without code changes. Empty flags = feature disabled, zero risk.
- **Heuristic, not ML** — 4 chars/token estimate + keyword bump keeps the
  stdlib-only constraint and YAGNI posture. Accurate enough for MVP; can be
  improved later.
- **Matches user expectation** — a single `model="auto"` name that "just works"
  matches the convention from LiteLLM, OpenRouter, and other LLM routers.

---

## Consequences

### Positive

- **Single `auto` model name** — users can specify `model="auto"` and let the
  router choose the tier, matching expectations from other LLM ecosystems.
- **Operator control via CLI** — tier models and thresholds are fully
  configurable at startup; no runtime API or database needed for MVP.
- **Zero worker/scheduler changes** — the feature slots into the existing
  router dispatch path; no Go version bumps, no new dependencies.
- **Backward compatible** — default-disabled; existing model names unchanged.
- **Predictable** — the classifier is pure, deterministic, and testable in
  isolation (9 unit tests cover thresholds, token estimation, keyword bump,
  max-tokens signal).
- **Counter accuracy** — the rewritten model name is what the `/meta/models/popular`
  counter records, preserving Issue #31's contract.

### Negative

- **Heuristic accuracy** — the 4 chars/token estimate and keyword bump are
  approximate; complex prompts with code or structured data may misclassify.
  Can be refined later with a better classifier.
- **No dynamic reconfiguration** — flags are static at startup; changing tier
  models requires a router restart. Dynamic mutation can be layered on later.
- **/v1/models disclosure** — the `auto` model appears only when tiers are
  configured; clients that parse the model list must handle its absence.
- **Streaming path** — the classifier runs before the first worker select, which
  is fine for non-streaming. Streaming requests currently select a worker once
  at the start; the classifier happens before that select, so it's compatible.

### Net

A minimal, router-side feature that adds a `model="auto"` alias with heuristic
complexity classification and configurable tier mapping, fully backward
compatible and without worker or scheduler changes.

---

## Alternatives Considered

### A. Worker-side model selection

Workers advertise multiple model names and the router dispatches based on
prompt analysis at the worker level. Rejected: breaks OpenAI API compatibility,
requires worker-level model awareness, and moves routing logic out of the
central router where failover and opt-out filtering are already implemented.

### B. Scheduler scoring based on prompt length

Extend `ModelRequest` with prompt metadata and adjust `WeightedScorer` to prefer
smaller models for short prompts. Rejected: invasive — changes the scheduler's
core scoring algorithm, affects all requests (not just `auto`), and adds
dependency on prompt metadata at the dispatch chokepoint.

### C. ML-based complexity classifier

Train or use a small ML model to classify prompt difficulty. Rejected: beyond
MVP scope, adds a dependency, and conflicts with the project's KISS/YAGNI
philosophy. Heuristic is sufficient for the initial adoption.

### D. Client-side model selection

Ask clients to select the model explicitly rather than providing an `auto`
alias. Rejected: does not match user expectations from other LLM routers and
defeats the purpose of a transparent routing layer.

---

## References

- Issue #32 (Thought. Should we have an "auto" model that routes based on complexity?)
- Issue #31 (`/meta/models/popular` — call counter)
- Issue #37 (Worker model opt-out — ADR for precedent on format and style)
- `pkg/router/classifier.go` — heuristic classifier implementation
- `pkg/router/server.go` — handler interception and model rewrite
- `cmd/router/main.go` — CLI flag surface
- `pkg/protocol/worker.go` — existing `Capabilities` type (no changes needed)
- `CONTEXT.md` — project architecture and constraints (stdlib only, Go 1.27+)