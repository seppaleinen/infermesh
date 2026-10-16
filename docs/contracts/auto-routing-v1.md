# ADR: Auto Model Routing by Complexity (Issue #32)

> **Status:** Accepted
>
> Updated for Issue #106: multi-signal semantic classifier + optional
> 4-tier mode. Related to `pkg/router/server.go`, `pkg/router/classifier.go`,
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

Issue #106 extended this: the classifier must semantically distinguish
**reasoning-heavy prompts** (prove, analyze, why/how, code, math) from
**simple read/write tasks** (summarize, translate, what/when lookup), not
just by length. It also asked for an optional fourth tier
(simple/medium/hard/hardest).

---

## Decision

Add a `model="auto"` logical alias that the router intercepts at request
time. Requests with `model="auto"` are classified by prompt complexity using
a **multi-signal scorer** (token estimate + semantic signals + max-tokens
signal + optional reasoning keywords), then rewritten to a configured tier
model before the existing `SelectWorker` pipeline dispatches to a worker.

### Tier structure

Two modes share one classifier. The **3-tier mode is the default** and is
fully backward compatible with the original `--auto-tier-simple` /
`--auto-tier-medium` / `--auto-tier-complex` flags. The **4-tier mode is
opt-in** via `--auto-use-four-tier`.

**3-tier mode (default):**

| Tier       | Trigger                                                | Flag                        |
|---|---|---|
| `simple`   | token baseline simple, not demoted by simple phrases  | `--auto-tier-simple`        |
| `medium`   | token baseline medium, or bumped up one tier          | `--auto-tier-medium`        |
| `complex`  | token baseline complex, or bumped up to the top       | `--auto-tier-complex`       |

**4-tier mode (`--auto-use-four-tier`):**

| Tier      | Trigger                                              | Flag                        |
|---|---|---|
| `simple`  | token baseline simple                                | `--auto-tier-simple`        |
| `medium`  | token baseline medium                                | `--auto-tier-medium`        |
| `hard`    | medium baseline bumped up by semantic signals        | `--auto-tier-hard`          |
| `hardest` | complex baseline, or bumped up to the top            | `--auto-tier-hardest`       |

In Go, `TierComplex` is a **named alias of `TierHardest`** (same underlying
value, `ComplexityTier(3)`), so every existing 3-tier config, map key, and
test keeps working unchanged. `TierHard` is the new `ComplexityTier(2)`; in
3-tier mode it is unreachable (the tier ladder is `simple → medium →
complex`), in 4-tier mode it sits between medium and hardest.

### Complexity classifier (multi-signal, stdlib only)

The classifier computes a **token-based baseline tier** exactly as before,
then applies a bounded **semantic adjustment** (roughly in the range
`-2..+3`) from cheap, pre-compiled signals. The result is mapped through the
active mode's tier ladder and capped at its top tier. No ML, no embeddings,
no new dependencies — `strings`, `regexp`, `unicode` only. All regexes are
compiled once in `init()`.

**Baseline (unchanged from v1):**

1. **Token estimate** — `len(content) / 4` (4 chars ≈ 1 token) summed across
   all message contents.
2. **`+200` max-tokens bump** — if `max_tokens > 500`, add ~200 to the
   estimate before the threshold comparison.
3. **Thresholds** — `SimpleMaxTokens` (default 200) and `MediumMaxTokens`
   (default 600). Below simple → simple baseline; between → medium; at or
   above → complex baseline.

**Semantic signals (additive, per signal):**

| Signal family  | Detection (pre-compiled)                                  | Effect      |
|---|---|---|
| Structural   | fenced code block, math notation (`$…$`, `\[…\]`, `\begin{`), numbered/bullet list, deep indentation | `+1` each (code block, math, list, nesting) |
| Instruction type | reasoning phrase list vs. simple phrase list (shipped defaults; operator-overridable). Reasoning wins → up; simple wins → down. Skipped when the legacy `--auto-reasoning-keywords` list is configured (avoids double-counting the same prompt). | `+1` / `-1` |
| Question depth | `why/how (does|do|did|would|…)` at line start → reasoning; `what/when/where/who/which (is|are|…)` → simple. | `+1` / `-1` |
| Context       | few-shot pattern (`example 1:`, `input:`, `output:`), role framing (`act as`, `you are a`, `as an expert`) | `+1` each |

**Legacy signal (backward compatible):** if the operator configured
`--auto-reasoning-keywords`, a case-insensitive substring match still bumps
the prompt one tier (on top of the semantic adjustment), as before.

**Default phrase lists** ship with the code (reasoning: "prove that",
"explain why", "compare and contrast", "think step by step", "how would", …;
simple: "summarize", "translate to", "what is", "list the", "fix the typo",
…). Operators can override/extend reasoning detection via
`--auto-reasoning-keywords`.

**Example classifications (4-tier intent; 3-tier lands on the merged top
tier):**

| Prompt | v1 (length only) | v2 (multi-signal) |
|---|---|---|
| "What is 2+2?" | Simple | Simple (what-question, no structure) |
| "Prove that P≠NP." | Simple | bumped (prove + reasoning phrase) |
| "Translate 'Hello' to French." | Simple | Simple (simple phrase, no bump) |
| "Debug this code: ```…```" + long body | Complex | complex/hardest (code + debug) |

**Performance target:** < 50 ms per classification. The scorer is a handful
of substring scans plus ~8 pre-compiled regexes over the prompt text; no
allocation-heavy parsing. Deterministic and side-effect free.

### Backward compatibility

- **No `--auto-tier-*` flags set** → the `auto` alias is a no-op: the router
  treats `model="auto"` as unknown (`no_workers` / 503). Explicit model
  names are completely unaffected.
- **3-tier flags only** (the v1 invocation) → identical behavior to the
  original classifier for prompts with no semantic signals, and strictly
  better (semantic bumps/demotions) for prompts with signals. The 24
  existing auto-routing tests pass unchanged.
- **`TierComplex`** remains a valid map key and error-message tier; it equals
  `TierHardest`.
- **Unconfigured tier** → the request fails with
  "no model configured for complexity tier …" (503), same as v1. No silent
  misrouting, no fallback to a different tier's model.

### Runtime Configuration

Auto-routing is reconfigurable at runtime without a router restart via two
HTTP endpoints on the router. These endpoints are protected by the same
API-key middleware as `/meta/aliases` and `/meta/models/popular`.

**Endpoints:**

| Method | Path               | Description                                       |
|---|---|---|
| `GET`  | `/meta/auto-routing`  | Returns the current config as a DTO.             |
| `POST` | `/meta/auto-routing`  | Atomically replaces the config; returns `{"ok": true}`. |

`GET` returns `200` with the current DTO even when auto-routing is disabled
(`enabled: false`). `POST` with an empty body or `enabled: false` clears the
runtime config (the router falls back to CLI-flag defaults, if any). Invalid
JSON returns `400`; unsupported methods return `405`.

**DTO shape:**

```json
{
  "enabled": true,
  "use_four_tier": false,
  "simple_max_tokens": 200,
  "medium_max_tokens": 600,
  "models": {
    "simple": "small-model",
    "medium": "medium-model",
    "hardest": "large-model"
  }
}
```

- **String tier names** — the DTO uses `"simple"`, `"medium"`, `"hard"`,
  `"hardest"` (and the backward-compatible alias `"complex"`, which maps to
  `hardest`) instead of serializing `ComplexityTier` integers. This keeps the
  wire format stable across tier-ladder changes.
- **Fields:** `enabled` (bool), `use_four_tier` (bool),
  `simple_max_tokens` (int, default 200), `medium_max_tokens` (int, default
  600), `models` (map of tier name → model name).
- Tier names that do not match a known tier are silently skipped on `POST`,
  and entries with an empty model name are dropped — the router never stores a
  blank mapping.

**Backward compatibility with runtime config:**

- **CLI flags remain the startup defaults.** The router builds its initial
  `AutoRoutingConfig` from `--auto-tier-*` flags exactly as before
  (`cmd/router/main.go`). The runtime endpoint does not read or merge the
  flags; it replaces the in-memory config atomically.
- **Runtime endpoint overrides CLI config.** A `POST` after startup takes
  effect immediately for new requests. There is no merge: the caller must
  send the full desired configuration.
- **3-tier default unchanged.** When no flags are set and no `POST` has been
  received, the router behaves exactly as the original v1 deployment
  (`model="auto"` is a no-op / 503).
- **Nil/empty config disables auto routing.** A `POST` with `enabled: false`
  or an empty `models` map clears the runtime config. With no runtime config
  and no CLI flags, `model="auto"` is treated as a literal unknown model name
  (`no_workers` / 503), not as an alias.

**Desktop app integration (`app/`):**

- `AutoRoutingSettings` struct in `app/config.go` is persisted to YAML under
  the `auto_routing` key. A `nil` pointer means auto routing is disabled.
- `RouterClient.GetAutoRouting` / `RouterClient.SetAutoRouting` in
  `app/router_client.go` call the endpoints above (GET and POST respectively),
  applying the same auth and error-handling patterns as
  `GetModelAliases` / `SetModelAliases`.
- `buildRouterArgs` in `app/args.go` generates `--auto-tier-*` CLI flags from
  `Settings.AutoRouting` so the router starts with the persisted config
  already applied. Empty/whitespace-only tier values are skipped, mirroring
  the router's own `buildAutoRoutingConfig` trimming behavior.
- `SettingsForm.vue` includes a collapsible "Auto Routing" section that edits
  `enabled`, `use_four_tier`, the five tier model inputs, and the two
  max-tokens thresholds. On save, if the auto-routing block changed, the form
  calls `RouterClient.SetAutoRouting` to push the new config live; a failure
  is non-fatal (the persisted YAML takes effect on the next router restart).

**Supervisor wiring:**

- CLI flags set the config at router startup (`cmd/router/main.go` →
  `buildAutoRoutingConfig` → `server.SetAutoRouting`).
- After the router process starts, the desktop supervisor may call
  `RouterClient.SetAutoRouting()` to confirm the live state matches the
  persisted settings. This is idempotent: the endpoint atomically replaces
  the config, so re-issuing the same payload is a no-op.
- Both paths are idempotent. `SetAutoRouting` replaces atomically; there is no
  compare-and-swap, so operators that want a "patch" semantics must read
  (`GET`), modify, and `POST` the full object.

### Enforcement points

`pkg/router/server.go` — in `handleChatCompletions` and `handleCompletions`,
after JSON parsing and before `marshal` & `selectWorker`:

1. If `req.Model == "auto"`, run `Classify` / `ClassifyCompletion`.
2. Map tier → concrete model name via the configured tier mapping.
3. Rewrite `req.Model` to the concrete name.
4. Proceed with the existing `marshal → selectWorker → dispatch` pipeline.

The rewritten model name is what the call counter records (see Issue #31).

### `/v1/models` advertisement

When at least one `--auto-tier-*` flag is non-empty, the `/v1/models`
endpoint includes an `auto` model entry with `Loaded: true` so clients
discover the alias immediately. When no tiers are configured, `auto` is
omitted from the model list.

### CLI surface (`cmd/router/main.go`)

```
--auto-tier-simple <model-name>      model to route to for simple prompts
--auto-tier-medium <model-name>      model to route to for medium prompts
--auto-tier-complex <model-name>     model for complex prompts (3-tier top;
                                     in 4-tier mode, top = --auto-tier-hardest)
--auto-tier-hard <model-name>        model for hard prompts (4-tier mode only)
--auto-tier-hardest <model-name>     model for the hardest prompts (4-tier top)
--auto-use-four-tier                 enable the 4-tier ladder (default: 3-tier)
--auto-simple-max-tokens <N>         token threshold for simple (default 200)
--auto-medium-max-tokens <N>         token threshold for medium (default 600)
--auto-reasoning-keywords <k1,k2,...> comma-separated substrings that bump a
                                     prompt one tier (legacy signal; also
                                     disables the built-in reasoning-phrase
                                     signal to avoid double counting)
```

Flags default to `0` / `""` / `false` (disabled). When both
`--auto-tier-complex` and `--auto-tier-hardest` are set, the hardest value
wins (they map to the same tier constant).

### Enforcement depth (defense in depth)

- **Primary path**: `SelectWorker` (scheduler) sees the rewritten concrete
  model name, not `auto`. No opt-out filter is needed for `auto` since the
  router resolves it before dispatch.
- **Fallback paths**: the router's `selectWorkerExcluding` and failover paths
  also see the rewritten model name. If the tier model has no available
  workers, the router returns `no_workers` (404 / 503) as normal.

### Rationale

- **Router-side only** — no worker or scheduler changes. The feature is
  entirely in the HTTP handler + classifier layer, preserving the existing
  deterministic scheduling and failover semantics.
- **Semantic, not just length** — reasoning phrases, structural cues,
  question depth, few-shot, and role framing correct the v1 length-only bias
  (short proofs under-routed, long boilerplate over-routed) while the token
  baseline keeps the original thresholds and their tests intact.
- **3-tier default, 4-tier opt-in** — the existing CLI surface and every
  existing deployment behave exactly as before; operators with four models
  gain granularity behind one flag.
- **Heuristic, not ML** — pre-compiled stdlib regexes + phrase lists keep the
  no-new-dependency constraint and the YAGNI posture. Rule-based routing
  trades some accuracy (≈65–75% on diverse prompts) for zero latency
  overhead, no model download, and full determinism. An ML/embedding-based
  classifier remains a possible future enhancement.

---

## Consequences

### Positive

- **Single `auto` model name** — users specify `model="auto"` and the router
  chooses the tier.
- **Semantic discrimination** — reasoning-heavy prompts route up even when
  short; simple read/write prompts route down even when long.
- **Backward compatible** — default-disabled, 3-tier by default,
  `TierComplex` alias unchanged; all 24 pre-existing auto-routing tests pass
  unchanged.
- **Zero worker/scheduler changes** — no Go version bumps, no new
  dependencies.
- **Predictable** — the classifier is pure, deterministic, side-effect free,
  and fast (<50 ms target); all regexes compiled once in `init()`.
- **Counter accuracy** — the rewritten model name is what the
  `/meta/models/popular` counter records, preserving Issue #31's contract.

### Negative

- **Heuristic accuracy ceiling** — phrase/regex heuristics miss novel
  phrasings ("I think this is…" no longer false-positives because the legacy
  keyword list is opt-in, but an unconfigured deployment can only rely on the
  shipped defaults). ML-based routing would do better; documented as a
  future enhancement.
- **4-tier requires 4 models** — an unconfigured tier still fails with
  "no model configured" by design (no silent misrouting). Operators with 2–3
  models should stay in 3-tier mode.
- **No dynamic reconfiguration** — flags are static at startup; changing tier
  models requires a router restart.
- **`/v1/models` disclosure** — `auto` appears only when tiers are
  configured; clients must handle its absence.
- **Streaming path** — the classifier runs before the first worker select,
  so it is compatible with both streaming and non-streaming requests.

### Net

A minimal, router-side feature that adds a `model="auto"` alias with a
multi-signal semantic classifier and an opt-in 4th tier, fully backward
compatible and without worker or scheduler changes.

---

## Alternatives Considered

### A. Worker-side model selection

Workers advertise multiple model names and the router dispatches based on
prompt analysis at the worker level. Rejected: breaks OpenAI API
compatibility, requires worker-level model awareness, and moves routing
logic out of the central router where failover and opt-out filtering are
already implemented.

### B. Scheduler scoring based on prompt length

Extend `ModelRequest` with prompt metadata and adjust `WeightedScorer` to
prefer smaller models for short prompts. Rejected: invasive — changes the
scheduler's core scoring algorithm, affects all requests (not just `auto`),
and adds dependency on prompt metadata at the dispatch chokepoint.

### C. ML-based / embedding-based complexity classifier

Train or use a small ML model (e.g. a finetuned classifier) or embed the
prompt and match against tier prototypes. Rejected for MVP: adds a
dependency, requires model hosting, and conflicts with the project's
KISS/YAGNI and stdlib-only posture. Kept as a documented future enhancement;
the multi-signal heuristic covers the common patterns it would target.

### D. Client-side model selection

Ask clients to select the model explicitly rather than providing an `auto`
alias. Rejected: does not match user expectations from other LLM routers and
defeats the purpose of a transparent routing layer.

### E. Renaming `TierComplex` → `TierHard` (breaking)

Rename the existing top tier and add two new ones. Rejected: breaks every
existing config, map key, and test. Instead, `TierComplex` is kept as a
named alias of the new top tier `TierHardest` and `TierHard` is inserted
between medium and hardest, so all existing code compiles and behaves the
same in 3-tier mode.

---

## References

- Issue #32 (Thought. Should we have an "auto" model that routes based on complexity?)
- Issue #106 (Investigate creating a "smart" router — semantic classification, 4 tiers)
- Issue #31 (`/meta/models/popular` — call counter)
- Issue #37 (Worker model opt-out — ADR for precedent on format and style)
- `pkg/router/classifier.go` — multi-signal classifier implementation
- `pkg/router/server.go` — handler interception and model rewrite
- `cmd/router/main.go` — CLI flag surface
- `pkg/protocol/worker.go` — existing `Capabilities` type (no changes needed)
- `CONTEXT.md` — project architecture and constraints (stdlib only, Go 1.27+)
