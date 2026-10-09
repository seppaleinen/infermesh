# ADR: Model Alias / Normalization (Issue #30)

> **Status:** Proposed
> Related to `pkg/router/server.go`, `pkg/router/classifier.go`,
> `cmd/router/main.go`, `pkg/router/classifier_test.go`.

---

## Context

Workers in the InferMesh pool advertise model names that are often verbose,
versioned, or include quantization suffixes (e.g., `google/gemma-4-12b`,
`qwen2.5-coder-7b-instruct-mlx@4bit`). Operators and their clients benefit
from stable, short aliases that match how they want to refer to models in
their applications.

The current routing requires exact model name matching between the request
model string and the name a worker advertises. There is no way to express
"route to the model called gemma-4-12b" when the worker's canonical name is
`google/gemma-4-12b` or `gemma-4-12b@q8_0`.

Operator-declared aliases are best handled at the router level because:

1. The router is the single place that sees all incoming requests.
2. Worker naming varies across backends (llama-cpp, Ollama, LM Studio, vLLM).
3. Clients should never need to adapt to a specific worker's naming scheme.

---

## Decision

Add a static operator-declared alias map that rewrites friendly model names
to their canonical form before worker selection.

### Alias format

Aliases are specified as comma-separated `alias=canonical` pairs. The split
occurs on the first `=` character, so canonical names may contain `=` (e.g.,
quantization suffixes like `@4bit`).

Example:

```
--model-alias gemma-4-12b=google/gemma-4-12b,qwen-coder-7b=qwen2.5-coder-7b-instruct-mlx@8bit
```

### ParseModelAliases function

```go
func ParseModelAliases(s string) *ModelAliasConfig
```

Parsing rules:
- Split on commas; trim each part; skip empty parts.
- Split each part on the **first** `=` only, so a canonical containing `=`
  (e.g., `"foo=bar=baz"`) is preserved verbatim.
- Trim key and canonical; skip if either is empty.
- Duplicate keys: last write wins.
- Returns `nil` for empty/whitespace input or when no valid alias survives.

### Resolver: resolveAliasModel

```go
func (s *Server) resolveAliasModel(model string) string
```

The resolver operates after the `"auto"` model resolution and before worker
selection. It returns the canonical name when the request model matches an
alias key; otherwise it returns the model unchanged.

### `/v1/models` synthetic injection

When aliases are configured, the `/v1/models` endpoint includes synthetic
entries for each alias with `Loaded: true` and `Backend: "alias"`, allowing
clients to discover the friendly names:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gemma-4-12b",
      "object": "model",
      "created": 0,
      "owned_by": "organization_infermesh",
      "permission": [],
      "root": null,
      "parent": null
    }
  ]
}
```

---

## Enforcement Points

`pkg/router/server.go` — in `handleChatCompletions` and `handleCompletions`,
after the `"auto"` resolution and before `marshal` & `selectWorker`:

1. Parse request JSON.
2. Resolve `model="auto"` if enabled (tiered routing).
3. Resolve any configured model alias via `resolveAliasModel`.
4. Proceed with the existing `marshal → selectWorker → dispatch` pipeline.

Alias resolution happens in-line with the existing request flow at the same
point where the `"auto"` model is resolved.

---

## CLI surface

`cmd/router/main.go`:

```
--model-alias <pairs>    comma-separated alias=canonical model pairs
                         (empty = aliasing disabled)
```

Example usage:

```bash
./bin/infermesh-router --model-alias "gemma-4-12b=google/gemma-4-12b,qwen-coder-7b=qwen2.5-coder-7b-instruct-mlx@8bit"
```

---

## Rationale

- **Router-side only** — no worker or scheduler changes required. The feature
  is entirely in the HTTP handler layer, preserving the existing deterministic
  scheduling and failover semantics.

- **Literal matching (no fuzzy/regex)** — simple string lookup is fast,
  predictable, and avoids ambiguity. Operators know exactly which alias maps
  to which canonical name.

- **Off by default** — the alias list is empty unless explicitly configured,
  maintaining backward compatibility. Existing model names are unchanged.

- **Mirrors "auto" pattern** — follows the same design as auto routing:
  static configuration, disabled by default, resolved at the router layer.

---

## Consequences

### Positive

- **Friendly client-facing names** — operators can expose short, memorable
  aliases (`gemma-4-12b`, `qwen-coder-7b`) to clients while workers use
  verbose canonical names.

- **Quant pinning via @quant** — the `@` character and everything after it
  is preserved in the canonical name. An alias like `qwen-coder-7b=qwen2.5-coder-7b-instruct-mlx@8bit`
  lets operators pin specific quantized variants.

- **Zero worker/scheduler changes** — the feature slots into the existing
  router dispatch path; no modifications to worker discovery or scheduling.

- **Predictable** — alias resolution is a simple map lookup with no side effects.

### Negative

- **Static at startup** — alias configuration is set at router launch;
  changing aliases requires a restart. This matches the auto routing pattern.

- **Operator must know canonical names** — the operator is responsible for
  determining the correct canonical model name for each worker. There is no
  automatic discovery or validation of aliases.

- **No collision handling** — if two workers advertise the same model name
  under different aliases, the first alias wins during parsing. The operator
  must ensure uniqueness.

---

## Alternatives Considered

### A. Fuzzy/regex matching

Use pattern matching to route aliases to models. Rejected: adds complexity,
is harder to debug, and could lead to unexpected matches. Literal matching
is simpler and more maintainable.

### B. Auto-discovery of aliases

Monitor worker advertisements and suggest aliases. Rejected: out of scope
for MVP; requires additional logic and could produce confusing suggestions.
Operators have the clearest picture of their naming conventions.

### C. Worker-side aliases

Let workers define their own short names. Rejected: workers may use different
backends with different naming schemes; the router is the canonical source
of truth for client-facing names.

### D. Per-request header override

Allow clients to override the model via header. Rejected: the router is meant
to abstract worker details from clients; clients should use the documented
aliases, not hack the system with headers.

---

## References

- Issue #30 (Model alias / normalization)
- Issue #32 (Auto routing)
- Issue #31 (`/meta/models/popular`)
- `docs/contracts/auto-routing-v1.md` (style reference)
- `pkg/router/classifier.go` — `AliasTarget`, `ModelAliasConfig`, `ParseModelAliases`
- `pkg/router/server.go` — `SetModelAliases`, `resolveAliasModel`, handler wiring
- `cmd/router/main.go` — `--model-alias` flag definition