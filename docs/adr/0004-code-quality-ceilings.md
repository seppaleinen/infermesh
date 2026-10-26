# ADR-0004: Code Quality Ceilings

**Status:** Accepted

**Date:** 2026-10-26

## Context

Issue #157 (code quality cleanup: KISS/DRY compliance for `pkg/`). At the start of the cleanup the
codebase had grown several "god files" mixing many responsibilities in one file, duplicated
wire-protocol types in two packages, dead passthrough code, and no lint enforcement of any
size or complexity ceilings:

- `pkg/router/server.go` — 2,232 lines, ~57 functions, ~8 mixed responsibilities
- `pkg/router/ws_hub.go` — 2,138 lines, 86 functions (WS connections, relay, stats, rate limiting)
- `pkg/worker/server.go` — 1,045 lines, 39 functions
- `pkg/worker/openai.go` — 843 lines (health probes, streaming, non-streaming, SSE parsing, idle timeout)
- `pkg/router/classifier.go` — 756 lines, two unrelated features (auto-routing + model aliases)
- 8 OpenAI-compatible wire types defined byte-for-byte in both `pkg/router/server.go` and `pkg/worker/server.go`
- `BackendAdapter` in `pkg/worker/server.go` — 82 lines of pure passthrough delegation, zero callers
- No `.golangci.yml` existed: `make lint` ran golangci-lint with default linters only

Without codified ceilings, refactoring gains erode over time: the next feature lands in the biggest
file again, and duplicated types reappear. This ADR records the ceilings agreed in
`research_brief.md` and the structural decisions made to reach them.

## Decision

### 1. File size ceilings

| Metric | Ceiling | Linter |
|--------|---------|--------|
| Lines per production `.go` file | 500 soft / 600 hard | `funlen` (indirect), manual review |
| Top-level functions per file | 20 | manual review |
| Lines per function | 100 | `funlen` (lines: 100, statements: 50) |
| Cyclomatic complexity | 15 | `gocyclo` (min-complexity: 15) |
| Nested if depth | 4 | `nestif` (min-complexity: 4) |

Test files (`_test.go`) are excluded from the ceilings for now (test-file decomposition is a
deferred follow-up; `server_test.go` is 3,708 lines today).

### 2. Wire-protocol type ownership

OpenAI-compatible request/response shapes (`ChatRequest`, `ChatMessage`, `Choice`, `ChatChunk`,
`CompletionChunk`, `ChatResponse`, `CompletionResponse`, `ModelsResponse`, `Usage`, ...) live in
**exactly one place**: `pkg/protocol/api.go`. `pkg/router` and `pkg/worker` import them; they must
never be redefined locally. Any wire-format change is a single-package edit.

### 3. DRY rules

- No copy-pasted types across packages — shared shapes go in `pkg/protocol/` (or a new `pkg/` package).
- A code block of ~20+ lines appearing twice is a duplication to extract, not to live with.
- Streaming SSE loops and request-handling skeletons are extracted helpers, not per-endpoint copies.
- `dupl` (threshold 100) is enabled in `.golangci.yml`.

### 4. Single responsibility per file

Each file has one purpose, stated in a header comment. The decomposition performed under #157:

**`pkg/router/`** (was server.go 2,232 + ws_hub.go 2,138 + classifier.go 756):
- `server.go` — Server struct, NewServer, Start, Addr, config setters
- `handlers_chat.go` — chat/completions endpoints, shared `handleInference[T]` skeleton
- `handlers_meta.go` — meta endpoints (workers, models, popular, aliases, auto-routing, stats)
- `dispatch.go` — worker selection, scoring, failover dispatch, model load
- `middleware.go` — error responses, rate-limit wrapper, inference endpoint middleware
- `ws_hub.go` — WSHub struct, lifecycle, handler registration
- `ws_conn.go` — wsConnection, reader/writer loops, pending calls
- `ws_relay.go` — relay dialing, relay reader/writer, relay call registration
- `ws_stats.go` — queue stats, snapshots, rejection/timeout recording
- `classifier.go` — auto-routing tier classification
- `aliases.go` — model alias config and parsing
- `routing_config.go` — auto-routing config DTOs and conversion

**`pkg/worker/`** (was server.go 1,045 + openai.go 843):
- `server.go` — Server struct, lifecycle, model management, health, metrics
- `handlers.go` — HTTP handlers, shared `streamSSE[T]` / `dispatchNonStreaming[T]`
- `openai.go` — non-streaming OpenAI backend calls, model listing
- `openai_stream.go` — streaming OpenAI backend calls, shared `streamOpenAI` helper
- `idle.go` — idle timeout type

### 5. Linting gate and ratchet policy

`.golangci.yml` enables `funlen`, `gocyclo`, `nestif`, `dupl`, `govet`, `errcheck`, `staticcheck`,
`unused`, `revive`. Pre-existing violations are suppressed with **explicit per-file/per-linter
exclusion rules** (no blanket presets). **Ratchet policy:** the exclusion list must shrink, never
grow. Each refactoring phase removes the exclusions it obsoletes; a PR that adds a new exclusion
must justify it in the description. New code is fully linted from day one.

### 6. Test invariants

- Refactors are behavior-preserving: `git diff` should read as moves, not logic changes.
- Coverage on `pkg/` must not regress (target 80%; current aggregate ~58% — see #157).
- Table-driven tests for >2 cases (existing convention).

## Consequences

**Easier:**
- Navigability: no file over 600 lines in `pkg/router` or `pkg/worker` (except pre-existing `run.go`, 776, flagged below).
- A wire-format change is one edit in `pkg/protocol/api.go` instead of two synchronized edits.
- `make lint` now fails on new complexity/duplication rather than default-linting only.

**Harder / accepted costs:**
- More files per package (17 in `pkg/router`, 13 in `pkg/worker`); the exclusion list in
  `.golangci.yml` is non-empty (81 rules as of this ADR) and must be shrunk over time.
- File splits of actively-developed files raise merge-conflict risk; land them in short-lived branches.
- `dupl` false-positives on table-driven test bodies; test files are excluded until decomposition.

**Known deferred items (tracked, not fixed by this ADR):**
- Test-file decomposition (`server_test.go` 3,708 lines, `ws_hub_test.go`, `server_auto_test.go`).
- `pkg/` → `internal/` migration for private packages (`routeraddr`, `wsutil`, maybe `registry`) —
  see issue #167 follow-up / separate layout-evaluation issue.
- `pkg/worker/run.go` (776 lines, pre-existing) — split when next touched.
- `pkg/router/ws_conn.go` ↔ `ws_relay.go` share a ~41-line `pc.handle` message-dispatch closure —
  extract when next touched.
- Pre-existing broken test `pkg/router/server_race_test.go:113` (`SetScorerWeights` called with 9
  args, signature takes 8) — breaks `go test -race ./pkg/router/`; fix when next touched.

## Related Files

- `research_brief.md` (issue #157) — evidence and phased plan
- `.golangci.yml` — the lint gate
- `pkg/protocol/api.go` — single home for wire types
- Issue #157 (umbrella) and sub-issues #158–#167
