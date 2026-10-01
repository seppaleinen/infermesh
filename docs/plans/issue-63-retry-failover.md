# Issue #63 — Router-Side Retry & Failover (Non-Streaming Only)

Status: **planned** — all 11 design tickets resolved in the issue body; ready to implement.

---

## Constraints (from AGENTS.md + #63 issue body)

- Go 1.27+, module `github.com/seppaleinen/infermesh`.
- Dependencies: stdlib + `github.com/hashicorp/mdns` + `gopkg.in/yaml.v3` — **no new dependencies**.
- CI pins: Go 1.27, `golangci-lint` v2.13.2, `go vet ./...`, `make build`, `go test ./... -v -cover -race`.
- Coverage target: **80% on `pkg/`** (`make coverage`, funccover).
- Build output to gitignored `bin/`; root-level binaries never committed.
- Single router (no HA), in-memory registry, deterministic scoring.
- Dev mode = plaintext `ws://`, no auth; prod mode = mTLS.
- Tests: unit in `pkg/*`, e2e in `tests/e2e/`, `make test` runs `go test ./pkg/...`.

---

## Current State (what exists on `main`)

- `pkg/scheduler/SelectWorker` + `WeightedScorer` wired into `Server.selectWorker` (#62 merged, commit `6e1ba84`).
- Three worker client implementations in `pkg/router/`:
  - `httpWorkerClient.Complete` — HTTP workers (mDNS/dev), has `postWithRetry` for dial retry only.
  - `wsWorkerClient.Complete` — WebSocket workers (dev/prod), **no retry today**.
  - `relayWorkerClient.Complete` — relay workers, **no retry today**.
- `proxyStream` in `server.go` handles both streaming and non-streaming via SSE — the single dispatch point.
- `handleChatCompletions` / `handleCompletions` both call `selectWorker` → `proxyChatStream` / `proxyCompletionStream` (both SSE paths).
- Registry marks unavailable at 60s, removes at 120s (heartbeat TTL 30s).
- Error sentinels: `errTimeout`, `ErrNoMatchingWorkers`, `ErrAllWorkersUnavailable`.

---

## Destination (from #63 issue body — all decisions pinned)

**Router-side failover to a different worker for non-streaming requests only, accepting non-idempotent retry.**

1. Dispatch a non-streaming (`stream:false`) request to best worker via `pkg/scheduler.SelectWorker` with `WeightedScorer`.
2. On transport error, timeout, 5xx, or 429 from that worker, re-select the next-best worker for the same model and re-dispatch — up to configurable max attempts.
3. Streaming (`stream:true`) requests are **out of scope** for failover: they fail fast with 503/504 and the client handles retry.
4. No "tokens already emitted" guard — failover allowed to produce different generation on backup worker.

---

## Config Surface (two router flags only)

| Flag | Default | Description |
|------|---------|-------------|
| `--max-attempts` | 3 | Primary + 2 fallbacks. `1` disables failover. |
| `--retry-budget` | 60s | Global deadline across all attempts. Per-attempt `proxyTimeout` (30s) stays as backstop. |

No backoff flag, no status-code flag, no worker-side flags. Same defaults in dev and prod.

---

## Retryable Taxonomy (pure function of error value)

**Retryable:**
- Transport dial failure
- Dispatch timeout (`errTimeout`)
- WS connection drop
- Worker 5xx
- Worker 429
- Pre-dispatch queue-full 429

**NOT retryable:**
- Other 4xx (400/401/403/404)
- Parse errors
- `selectWorker` exhaustion (`ErrNoMatchingWorkers` / `ErrAllWorkersUnavailable` / no candidates after exclusion)

---

## Implementation Plan

### T1 — Router flags & plumbing
**Files:** `cmd/router/main.go`, `pkg/router/server.go`

- Add `--max-attempts` (default 3) and `--retry-budget` (default 60s) to router flags.
- Store in `Server` struct: `maxAttempts int`, `retryBudget time.Duration`.
- Thread-safe accessor methods if needed.

### T2 — Non-streaming gate in handlers
**Files:** `pkg/router/server.go`

- In `handleChatCompletions` / `handleCompletions`: inspect `req.Stream` (already parsed).
- `stream:true` → existing `proxyStream` path (unchanged).
- `stream:false` → new `dispatchWithFailover(ctx, model, reqBody, maxAttempts, retryBudget)` function.

> **Why in handlers, not `proxyStream`**: `proxyStream` currently writes SSE headers immediately. The failover loop must write **nothing** to the client until first successful attempt. Gate at the handler level avoids "bytes on wire" complexity.

### T3 — `dispatchWithFailover` core loop
**Files:** `pkg/router/server.go` (new private method)

Signature:
```go
func (s *Server) dispatchWithFailover(ctx context.Context, model string, body []byte, kind string, maxAttempts int, budget time.Duration) ([]byte, error)
```

Logic:
1. Create request-scoped context with `budget` deadline: `ctx, cancel := context.WithTimeout(ctx, budget); defer cancel()`.
2. `excluded := make(map[string]struct{})` — per-request exclusion set of attempted worker IDs.
3. Loop `attempt := 1; attempt <= maxAttempts; attempt++`:
   a. `worker, err := s.selectWorkerExcluding(model, excluded)` — see T4.
   b. If error → if attempt == maxAttempts return 503 "all attempts failed"; else continue.
   c. `resp, err := s.clientFor(worker).Complete(ctx, worker, kind, body)`.
   d. If `err == nil` → return `resp, nil` (success, first attempt to write to client).
   e. If `isRetryableError(err)` → record rejection, add `worker.ID` to `excluded`, continue loop.
   f. Else (non-retryable) → return error immediately (fail fast).
4. After loop exhausted → return 503 "all attempts failed".

### T4 — `selectWorkerExcluding` filter
**Files:** `pkg/router/server.go` (new private method) or extend `selectWorker`

```go
func (s *Server) selectWorkerExcluding(model string, excluded map[string]struct{}) (protocol.WorkerInfo, error)
```

- Reuse existing candidate-building logic from `selectWorker` (lines 800-882).
- Before scoring, filter out any candidate whose `ID` is in `excluded`.
- If no candidates remain → return `ErrNoMatchingWorkers` (triggers failover exhaustion).
- Includes the "any available worker" fallback (lines 864-881) — also respects exclusion.

> **Why not a second selector**: #63 T5 decision — single primitive. `selectWorkerExcluding` is a thin pre-score filter, not a second algorithm.

### T5 — `isRetryableError` predicate
**Files:** `pkg/router/server.go` (new private function)

```go
func isRetryableError(err error) bool {
    // transport dial failure, errTimeout, WS drop, 5xx, 429, queue-full 429
    // NOT: other 4xx, parse errors, ErrNoMatchingWorkers, ErrAllWorkersUnavailable
}
```

- Centralize in one place so both `httpWorkerClient` and `wsWorkerClient` / `relayWorkerClient` paths use identical taxonomy.
- No config flag — pure function of error value.

### T6 — Rate-limit rejection integration
**Files:** `pkg/router/server.go`, `pkg/router/ws_hub.go`

- On pre-dispatch queue-full 429 (current `hub.isRateLimited` check in handlers): treat as retryable → add to `excluded`, continue failover loop.
- Do NOT write `Retry-After` to client on intermediate attempts; only on final failure if appropriate.

### T7 — Unit tests
**Files:** `pkg/router/server_test.go`, `pkg/scheduler/scheduler_test.go`

- `TestDispatchWithFailover_SuccessOnFirstAttempt`
- `TestDispatchWithFailover_SuccessOnSecondAttempt` (mock first worker error, second succeeds)
- `TestDispatchWithFailover_ExhaustedAttempts` (all return retryable errors)
- `TestDispatchWithFailover_NonRetryableErrorStops` (400 returns immediately)
- `TestSelectWorkerExcluding_FiltersExcluded`
- `TestIsRetryableError_Taxonomy`
- `TestRetryBudget_DeadlineEnforced`

### T8 — Integration test (e2e)
**Files:** `tests/e2e/retry_failover_test.go` (new)

- Start router + 2 workers (same model).
- Configure `--max-attempts=3 --retry-budget=10s`.
- Send non-streaming request to worker 1 that returns 500 → verify router fails over to worker 2 and returns 200.
- Verify streaming request (`stream:true`) fails fast with 503 (no failover).

---

## Out of Scope (explicitly decided)

- Streaming failover — cannot undo partial SSE output.
- Cross-model fallback — failover only within same model.
- Idempotency keys / request deduplication.
- Circuit breaker / worker quarantine — registry already marks unavailable at 60s.
- Backoff between attempts — failover re-dispatches immediately to a *different* worker.

---

## Acceptance Criteria

- [ ] `make test` passes (unit + integration).
- [ ] `make lint` clean.
- [ ] `make build-static` compiles (15 platform files, CGO_ENABLED=0).
- [ ] Non-streaming request with first worker failing → router fails over to second worker and returns 200.
- [ ] Streaming request with first worker failing → 503 returned immediately (no failover).
- [ ] `--max-attempts=1` disables failover (current behavior).
- [ ] `--retry-budget` enforces global deadline across attempts.
- [ ] 80% coverage on `pkg/` maintained.

---

## File Touch Map

| File | Changes |
|------|---------|
| `cmd/router/main.go` | 2 new flags, pass to Server |
| `pkg/router/server.go` | ~200 lines: flags, `dispatchWithFailover`, `selectWorkerExcluding`, `isRetryableError`, handler gate |
| `pkg/router/server_test.go` | ~150 lines unit tests |
| `tests/e2e/retry_failover_test.go` | new file, ~100 lines |
| `pkg/scheduler/scheduler.go` | possibly export `ErrNoMatchingWorkers` / `ErrAllWorkersUnavailable` if not already public |

---

## Traceability

| #63 Decision Ticket | Plan Section |
|---------------------|--------------|
| T1 — Max attempts | T1 |
| T2 — Retryable taxonomy | T5 |
| T3 — Backoff (none) | T3 (no inter-attempt delay) |
| T4 — Retry budget | T1 + T3 |
| T5 — Scheduler integration | T3 + T4 |
| T6 — Exclusion list | T3 + T4 |
| T7 — Non-streaming gate | T2 |
| T8 — Response buffering | T3 (Complete is atomic) |
| T9 — Config surface | T1 |
| T10 — Tests | T7 + T8 |
| T11 — Documentation | `docs/contracts/retry-failover-v1.md` (new) |

---

## Next Step

Dispatch `dev-team-lead` with this plan to begin implementation.