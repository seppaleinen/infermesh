# Research Brief: Code Quality Cleanup

**Task:** Scope and plan a large code-quality cleanup effort for the infermesh Go monorepo's core packages.
**Domain:** dev
**Date:** 2026-10-21

---

## Scope Decision

### IN scope (this effort)
- All Go source files under `pkg/` (12 packages: router, worker, scheduler, registry, security, discovery, capabilities, protocol, platform, wsutil, routeraddr).
- Elimination of duplicated types across packages (DRY).
- God-file decomposition (splitting files >500 lines into single-responsibility files).
- Dead code removal (unused methods, redundant adapters).
- Architectural design record (ADR) establishing size/complexity ceilings.
- golangci-lint configuration hardening.

### OUT of scope (deferred)
- `app/` (Wails desktop module) — separate Go module, separate conventions.
- `cmd/` entry points — thin wiring, low ROI for this pass.
- `tests/` (integration, e2e, security) — test refactoring is a follow-up.
- Test file decomposition (e.g., `server_test.go` at 3,708 lines) — out of scope per "core packages only" decision.
- Structural layout changes (`internal/` migration, `src/` discussion) — user explicitly deferred to a separate exercise; a GitHub issue will be created for tracking.

---

## Current-State Findings

### 1. Duplicated Types Across Packages (DRY Violation)

The most impactful finding. The following types are defined **identically** in both `pkg/router/server.go` and `pkg/worker/server.go`:

| Type | Router (lines) | Worker (lines) | Identical? |
|------|---------------|----------------|------------|
| `ChatMessage` | 255-258 | 64-67 | ✅ byte-for-byte |
| `ChatRequest` | 261-267 | 70-76 | ✅ byte-for-byte |
| `CompletionRequest` | 302-308 | 79-85 | ✅ byte-for-byte |
| `Message` | 270-273 | 88-91 | ✅ byte-for-byte |
| `Choice` | 276-281 | 100-106 | ✅ (router omits `Delta` field) |
| `CompletionChunk` | 284-290 | 109-119 | ✅ (worker adds `Usage` field) |
| `ChatChunk` | 293-299 | 122-132 | ✅ (worker adds `Usage` field) |
| `ModelsResponse` | 311-314 | 135-138 | ✅ byte-for-byte |

These types represent the OpenAI-compatible wire protocol. They belong in `pkg/protocol/` (which currently has zero API-level types — only `worker.go`, `ws.go`, `serialize.go`). Both packages import `pkg/protocol` already. This duplication means any wire-format change requires editing two places and risks drift.

### 2. God Files (KISS / Single Responsibility Violation)

| File | Lines | Functions | Problem |
|------|-------|-----------|---------|
| `pkg/router/server.go` | 2,232 | 57 | Mixes HTTP handlers (chat, completions, models, workers, aliases, auto-routing, queue stats, inference stats, connection count, inflight requests, dev register), server lifecycle (Start, Addr), failover dispatch logic, worker selection/scoring delegation, model loading, proxy streaming, middleware wrapping, config setters. At least 8 distinct responsibilities. |
| `pkg/router/ws_hub.go` | 2,138 | 86 | Mixes WebSocket connection management, relay dialing/routing, pending call lifecycle, rate limiting, rejection recording, queue metrics, snapshot helpers, handler registration, reader/writer loops. The relay subsystem alone (~200 lines) could be its own file. |
| `pkg/worker/server.go` | 1,045 | 39 | Mixes HTTP handlers, BackendAdapter (see #3), server lifecycle, model management, streaming SSE logic, health/metrics endpoints. |
| `pkg/worker/openai.go` | 843 | ~20 | Mixes health probes, circuit breaker integration, non-streaming chat/completions, streaming chat/completions, idle timeout implementation, SSE parsing, model listing. |
| `pkg/router/classifier.go` | 732 | ~15 | Mixes model alias config/parsing AND auto-routing classifier/tier resolution. Two features, one file. |

### 3. Dead Code: BackendAdapter (KISS Violation)

`pkg/worker/server.go` lines 140-222 define `BackendAdapter` which wraps `Backend` + a model name. It implements all 14 methods of the `Backend` interface as pure passthroughs:

```go
func (a *BackendAdapter) CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
    return a.Backend.CompleteChat(ctx, model, req)
}
// ... 13 more identical delegations
```

**No caller ever uses `BackendAdapter.ChatCompletion()` or `BackendAdapter.Completion()`** (the two non-interface methods). `SetBackend` wraps every backend in `NewBackendAdapter(backend, model)` but the stored model field is never read — every call site passes the model from the request anyway. This is 82 lines of dead delegation that violates KISS.

### 4. Duplicated Streaming Logic (DRY Violation)

`pkg/worker/server.go` contains near-identical streaming select loops in `chatCompletions` (lines 698-776) and `completions` (lines 854-932). The structure is:
1. Check errCh with default select
2. If pendingErr, write error and return
3. Select on chunkCh / ctx.Done
4. Handle close, marshal chunk, write SSE frame, flush

This ~70-line block is copy-pasted with only the channel type and variable names changed. A generic helper or extracted function would eliminate this.

Similarly, `pkg/router/server.go` `handleChatCompletions` (lines 792-876) and `handleCompletions` (lines 879-962) share an almost identical structure: parse → check registry → resolve auto model → resolve alias → marshal → branch on stream/non-stream → dispatch.

### 5. No golangci-lint Configuration File

The repo runs `golangci-lint run` via Makefile and CI (`golangci-lint-action@v9`, version v2.13.2) but has **no `.golangci.yml`**. This means only default linters run. No cyclomatic complexity ceiling, no file-length limit, no duplicate-code detection, no godot/godox enforcement.

### 6. Missing `internal/` Boundary

All packages live under `pkg/` (exported). There is no `internal/` directory. Packages like `routeraddr` (a single `Normalize` helper + `Mode` type) and `wsutil` are implementation details that external consumers should not depend on, but `pkg/` makes them part of the public API surface.

---

## Layout Recommendation

### Verdict: KEEP current `cmd/` + `pkg/` layout. Do NOT create `src/`.

**Rationale:**
1. **Go convention.** The standard Go project layout is `cmd/` (binaries) + `pkg/` or `internal/` (libraries) + root `go.mod`. A `src/` directory is a Java/Python/C++ convention that fights Go tooling, IDE support, and community expectations. Every Go developer joining the project would need to unlearn their instincts.
2. **Existing boundaries work.** The `cmd/{router,worker,relay}` → `pkg/*` split correctly separates binary entry points from library code. The `app/` separate-module pattern for Wails is idiomatic.
3. **The real problem isn't directories, it's files.** The issues identified above are all within-package: god files, duplicated types, dead code. Moving files to `src/` fixes none of these.
4. **`internal/` adoption is recommended as a future exercise** (user requested a separate GitHub issue for this). When done, packages like `routeraddr`, `wsutil`, and potentially `registry` (only consumed by `router`) should move there.

> 🧠 **From Hindsight memory (Conventions and patterns)** — The repo is split into `app/` (Wails Go backend and Vue frontend), `cmd/` for binary entry points, `pkg/` for shared packages, and `internal/` for private helpers. Note: `internal/` does not yet exist despite being documented as a convention.

---

## Proposed Architectural Design / Ceilings

To be codified as `docs/adr/0004-code-quality-ceilings.md`.

### File Size Ceilings

| Metric | Ceiling | Rationale |
|--------|---------|-----------|
| Max lines per production `.go` file | **500** | Forces single responsibility. Current worst offenders: server.go (2,232), ws_hub.go (2,138). |
| Max lines per test `_test.go` file | **800** | Test files can be larger but 3,708 is unmanageable. (Enforcement deferred — test files out of scope for this phase.) |
| Max functions per file | **20** | server.go has 57, ws_hub.go has 86. |

### Function Size Ceilings

| Metric | Ceiling | Enforcement |
|--------|---------|-------------|
| Max lines per function | **100** | golangci-lint `funlen` linter |
| Max cyclomatic complexity | **15** | golangci-lint `gocyclo` linter |
| Max nesting depth | **4** | golangci-lint `nestif` linter |

### Duplication Rules (DRY)

- Wire-protocol types (OpenAI-compatible request/response shapes) MUST live in exactly one place: `pkg/protocol/`.
- Shared helper functions used by ≥2 packages MUST live in `pkg/` (or `internal/` when adopted), not be copy-pasted.
- Streaming SSE loops that differ only by type parameter MUST be extracted into a shared generic or helper.

### Single Responsibility Per File

Each file should have a clear, one-sentence purpose stated in its package doc comment or file header. Recommended splits for the god files:

**`pkg/router/server.go` (2,232 → 5 files):**
| New File | Contents | Est. Lines |
|----------|----------|------------|
| `server.go` | Server struct, NewServer, Start, Addr, config setters | ~200 |
| `handlers_chat.go` | handleChatCompletions, handleCompletions, proxyStream, proxyChatStream, proxyCompletionStream | ~350 |
| `handlers_meta.go` | handleWorkersList, handleModelsList, handlePopularModels, handleSetAliases, handleAutoRouting, handleConnectionCount, handleInferenceStats, handleQueueStats, handleInflightRequests, handleDevRegister | ~350 |
| `dispatch.go` | selectWorker, selectWorkerExcluding, buildCandidates, scoreCandidates, dispatchWithFailover, writeFailoverError, loadModelOnWorker, isRetryableError | ~250 |
| `middleware.go` | wrapInferenceEndpoints, writeErrorResponse, writeRateLimitError | ~80 |

**`pkg/router/ws_hub.go` (2,138 → 4 files):**
| New File | Contents | Est. Lines |
|----------|----------|------------|
| `ws_hub.go` | WSHub struct, NewWSHub, config setters, Handler, connection lifecycle | ~400 |
| `ws_conn.go` | wsConnection struct, readerLoop, writerLoop, pending calls, addPending | ~500 |
| `ws_relay.go` | Relay dialing, relay reader/writer, registerRelayCall, registerRelayStream, routeRelayResponse, normalizeRelayURL | ~400 |
| `ws_stats.go` | QueueStats, connSnapshot, avgWaitFor, recordRejection*, recordTimeout*, relayInFlight*, relayRejected* | ~300 |

**`pkg/worker/server.go` (1,045 → 3 files):**
| New File | Contents | Est. Lines |
|----------|----------|------------|
| `server.go` | Server struct, NewServer, Start, Addr, SetBackend, model management, health, metrics, capabilities | ~350 |
| `handlers.go` | chatCompletions, completions, loadModelHandler, modelsList + shared streaming helper | ~400 |
| Remove `BackendAdapter` entirely | Dead code elimination | -82 lines |

**`pkg/router/classifier.go` (732 → 2 files):**
| New File | Contents | Est. Lines |
|----------|----------|------------|
| `classifier.go` | AutoRoutingConfig, ComplexityTier, Classify, ClassifyCompletion, estimateTokens, tier resolution | ~400 |
| `aliases.go` | ModelAliasConfig, AliasTarget, ParseModelAliases, modelAliasesActive, buildAliasConfig | ~300 |

### Protocol Type Consolidation

Move to `pkg/protocol/api.go` (new file):
- `ChatMessage`, `ChatRequest`, `CompletionRequest`
- `Message`, `Choice`, `Delta` (unify the Choice type — add Delta field)
- `ChatChunk`, `CompletionChunk` (unify — add Usage field)
- `ChatResponse`, `CompletionResponse` (from `pkg/worker/backend.go`)
- `ModelsResponse`
- `Usage` (already in worker, needed by both)

Both `pkg/router` and `pkg/worker` then import `protocol.ChatRequest` etc.

### golangci-lint Configuration

Create `.golangci.yml` enabling:
```yaml
linters:
  enable:
    - funlen        # max function length (100 lines)
    - gocyclo       # cyclomatic complexity (max 15)
    - nestif        # nested if depth (max 4)
    - dupl          # duplicate code detection
    - govet         # standard vet checks
    - errcheck      # unchecked errors
    - staticcheck   # comprehensive static analysis
    - unused        # unused code detection
    - gosimple      # simplification suggestions
    - revive        # replacement for golint
linters-settings:
  funlen:
    lines: 100
    statements: 50
  gocyclo:
    min-complexity: 15
  nestif:
    min-complexity: 4
  dupl:
    threshold: 100
```

### Test Coverage Expectations

- Maintain existing 80% target on `pkg/` (documented in AGENTS.md).
- Refactoring must not decrease coverage. Each file split must carry its tests.
- Table-driven tests required for >2 cases (existing convention).

---

## Sequenced Phased Plan

### Phase 1: Foundation — Linting & Protocol Types
**Goal:** Establish enforcement gates and eliminate the highest-impact DRY violation.

| Step | Action | Files Touched |
|------|--------|---------------|
| 1.1 | Create `.golangci.yml` with ceilings (initially with `issues.exclude-rules` to silence existing violations so CI stays green) | `.golangci.yml` (new) |
| 1.2 | Create `pkg/protocol/api.go` with unified wire types | `pkg/protocol/api.go` (new) |
| 1.3 | Update `pkg/router/server.go` to use `protocol.ChatRequest` etc., remove local definitions | `pkg/router/server.go` |
| 1.4 | Update `pkg/worker/server.go` to use `protocol.ChatRequest` etc., remove local definitions | `pkg/worker/server.go` |
| 1.5 | Update `pkg/worker/backend.go` to reference protocol types | `pkg/worker/backend.go` |
| 1.6 | Run `make lint`, `make test`, fix breakage | All affected |

**Exit Criteria:**
- `make lint` passes with new `.golangci.yml` (excluded rules for existing violations).
- `make test` passes (all unit tests green).
- Zero duplicated wire-protocol type definitions between router and worker.
- `go vet ./pkg/...` clean.

**Verification:** `make lint && make test && go vet ./pkg/...`

---

### Phase 2: Dead Code & Micro-Duplication
**Goal:** Remove BackendAdapter, extract shared streaming helpers, tighten linter excludes.

| Step | Action | Files Touched |
|------|--------|---------------|
| 2.1 | Delete `BackendAdapter` type and all 14 passthrough methods from `pkg/worker/server.go`. Change `SetBackend` to store the `Backend` directly. | `pkg/worker/server.go` |
| 2.2 | Extract the duplicated streaming select loop in `pkg/worker/server.go` (`chatCompletions` and `completions`) into a generic `streamSSE[T any]` helper. | `pkg/worker/server.go` or new `pkg/worker/stream.go` |
| 2.3 | Extract the duplicated request-handling skeleton in `pkg/router/server.go` (`handleChatCompletions` and `handleCompletions`) into a shared `handleInferenceRequest` helper that takes the parse/resolve/dispatch steps as callbacks or a strategy interface. | `pkg/router/server.go` |
| 2.4 | Narrow the `exclude-rules` in `.golangci.yml` — remove exclusions for files fixed in Phase 1-2. | `.golangci.yml` |

**Exit Criteria:**
- `BackendAdapter` grep returns zero results.
- Streaming loop body exists in exactly one place in `pkg/worker/`.
- `make lint` passes with fewer excluded rules than Phase 1.
- `make test` passes.

**Verification:** `grep -rn "BackendAdapter" pkg/ && make lint && make test`

---

### Phase 3: God File Decomposition — Router
**Goal:** Split `pkg/router/server.go` (2,232 lines) and `pkg/router/ws_hub.go` (2,138 lines) into focused files.

| Step | Action | Files Touched |
|------|--------|---------------|
| 3.1 | Split `classifier.go` into `classifier.go` (auto-routing) + `aliases.go` (model aliases) | `pkg/router/classifier.go`, `pkg/router/aliases.go` (new) |
| 3.2 | Extract `server.go` handlers into `handlers_chat.go`, `handlers_meta.go` | `pkg/router/server.go`, new files |
| 3.3 | Extract dispatch/failover logic into `dispatch.go` | `pkg/router/dispatch.go` (new) |
| 3.4 | Extract middleware/error helpers into `middleware.go` | `pkg/router/middleware.go` (new) |
| 3.5 | Split `ws_hub.go` into `ws_hub.go`, `ws_conn.go`, `ws_relay.go`, `ws_stats.go` | `pkg/router/ws_hub.go`, new files |
| 3.6 | Enable `funlen` and `gocyclo` linters fully (remove remaining excludes for router package) | `.golangci.yml` |

**Exit Criteria:**
- No production `.go` file in `pkg/router/` exceeds 500 lines.
- No file has more than 20 top-level functions.
- `make lint` passes with `funlen` and `gocyclo` active.
- `make test` passes.
- `git diff --stat` shows only moves/splits, no behavioral changes.

**Verification:** `find pkg/router -name "*.go" ! -name "*_test.go" -exec wc -l {} + | sort -rn | head && make lint && make test`

---

### Phase 4: God File Decomposition — Worker & Scheduler
**Goal:** Split remaining oversized files in `pkg/worker/` and review `pkg/scheduler/`.

| Step | Action | Files Touched |
|------|--------|---------------|
| 4.1 | Split `pkg/worker/server.go` (1,045 → ~350 after Phase 2 reductions) into `server.go` + `handlers.go` | `pkg/worker/server.go`, `pkg/worker/handlers.go` (new) |
| 4.2 | Review `pkg/worker/openai.go` (843 lines). Extract `idleTimeout` into its own file. Consider splitting streaming vs non-streaming methods. | `pkg/worker/openai.go`, potentially new files |
| 4.3 | Review `pkg/scheduler/scheduler.go` (598 lines). Currently well-structured (single responsibility: scoring). May stay as-is if under 500 after removing comments, or split filter functions into `filters.go`. | `pkg/scheduler/scheduler.go` |
| 4.4 | Enable all linters fully across all `pkg/` packages. | `.golangci.yml` |

**Exit Criteria:**
- No production `.go` file in `pkg/` exceeds 500 lines.
- `make lint` passes across all `pkg/...` with full linter suite.
- `make test` passes.
- Coverage remains ≥80% on `pkg/`.

**Verification:** `find pkg -name "*.go" ! -name "*_test.go" -exec wc -l {} + | sort -rn | head && make lint && make test && make coverage`

---

### Phase 5: ADR & Documentation
**Goal:** Codify the architectural decisions so they persist.

| Step | Action | Files Touched |
|------|--------|---------------|
| 5.1 | Write `docs/adr/0004-code-quality-ceilings.md` documenting file/function limits, SRP rule, protocol-type ownership, and linter config. | `docs/adr/0004-code-quality-ceilings.md` (new) |
| 5.2 | Update `AGENTS.md` repo layout section to reflect new file structure. | `AGENTS.md` |
| 5.3 | Create GitHub issue for future `internal/` migration (per user request). | GitHub (not code) |

**Exit Criteria:**
- ADR follows the template in `docs/adr/README.md`.
- AGENTS.md layout tree matches actual file tree.

---

## Suggested GitHub Issues

Do NOT create these — for team-lead review and creation.

| # | Title | Labels | Phase |
|---|-------|--------|-------|
| 1 | Add `.golangci.yml` with code quality ceilings | `tech-debt`, `tooling` | 1 |
| 2 | Consolidate duplicated OpenAI wire types into `pkg/protocol` | `tech-debt`, `dry` | 1 |
| 3 | Remove dead `BackendAdapter` passthrough in `pkg/worker` | `tech-debt`, `kiss` | 2 |
| 4 | Extract shared streaming SSE loop in `pkg/worker/server.go` | `tech-debt`, `dry` | 2 |
| 5 | Deduplicate `handleChatCompletions` / `handleCompletions` in router | `tech-debt`, `dry` | 2 |
| 6 | Decompose `pkg/router/server.go` (2,232 lines) into focused files | `tech-debt`, `refactor` | 3 |
| 7 | Decompose `pkg/router/ws_hub.go` (2,138 lines) into focused files | `tech-debt`, `refactor` | 3 |
| 8 | Split `pkg/router/classifier.go` into auto-routing + aliases | `tech-debt`, `refactor` | 3 |
| 9 | Decompose `pkg/worker/server.go` and `openai.go` god files | `tech-debt`, `refactor` | 4 |
| 10 | ADR: Code quality ceilings and architectural design rules | `documentation`, `architecture` | 5 |
| 11 | Evaluate `pkg/` → `internal/` migration for private packages | `architecture`, `future` | Deferred |

Issue #11 is the layout evaluation the user explicitly deferred.

---

## Risks and Open Questions

### Risks

1. **Merge conflicts during file splits.** Phase 3-4 touch the most actively developed files. These phases should be done on short-lived branches with fast PR turnaround. Coordinate with any in-flight feature work.

2. **Test coupling to file structure.** Some tests may rely on unexported symbols that become inaccessible after a split if they were in the same package but now are conceptually separated. Mitigation: all splits stay within the same Go package (`package router`), so unexported symbols remain accessible. No package boundary changes.

3. **Protocol type unification edge cases.** The `Choice` type differs slightly between router (no `Delta` field) and worker (has `Delta` field). The `CompletionChunk`/`ChatChunk` differ in `Usage` presence. Unification requires adding fields to the shared type. Since JSON `omitempty` is used, this is wire-compatible, but tests asserting exact struct equality may break.

4. **Linter noise flood.** Enabling `dupl`, `funlen`, `gocyclo` on the current codebase will produce hundreds of warnings. The phased exclude-rule approach mitigates this, but requires discipline to shrink the exclusion list each phase.

5. **"New dependencies prohibited" constraint.** All refactoring must use stdlib only. Generic streaming helpers (Phase 2) must use Go 1.27 generics (stdlib), not third-party libraries.

### Open Questions for User

1. **Should `pkg/protocol/api.go` types use generics?** The `ChatChunk` and `CompletionChunk` are nearly identical except for naming. A single generic `StreamChunk[T]` could replace both, but adds complexity. Recommendation: keep them as concrete types for now (KISS), unify only the shared fields.

2. **Is the 500-line file ceiling too aggressive for `ws_hub.go`?** After splitting into 4 files, the connection lifecycle file (`ws_conn.go`) may still approach 500 lines due to the inherent complexity of WebSocket reader/writer loops. Recommendation: 600-line hard cap with 500 as the soft target.

3. **Should the streaming helper extraction (Phase 2 step 2.2) use Go generics?** The chat and completion streaming loops differ only in chunk type (`ChatChunk` vs `CompletionChunk`). Go 1.27 generics handle this cleanly. Recommendation: yes, use `[T any]` constrained to a `StreamChunk` interface.

</content>