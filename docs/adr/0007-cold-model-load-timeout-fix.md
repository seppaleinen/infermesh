# ADR-0007: Cold Model Load Timeout Budget & Honest Loaded Advertisement

**Status:** Accepted

**Date:** 2026-11-15

## Context

The "stuck at 0%" symptom: when a single-slot LM Studio backend needed to cold-load a model (engine switch + prefill), the request would hang for minutes then time out or flapped. Root cause chain:

1. **Dev-mode Loaded inflation** — `buildRegistrationInfo` set `Loaded=true` for *all* backend-advertised models (LM Studio lists 12 with no `loaded` field). The router dispatched unloaded models.
2. **No auto-load path in relay mode** — `loadModelOnWorker` dials `http://workerIP:8081/v1/models/load`; relay workers have no inbound path, so the dial times out and the worker is silently excluded.
3. **Destructive stream fallback** — 5s `streamingGracePeriod` const; on expiry with zero chunks, `streamOrFallback` cancelled the in-flight stream and retried non-streaming. This aborted the LM Studio engine switch ("Engine protocol startup was aborted" 400), tripped the circuit breaker, and caused a flapping loop.
4. **60s relay hard cap** — `maxRelayCallTimeout = 60s const` applied to cold loads that need >60s (engine switch + prefill + generation).

## Decision

Three independent but complementary fixes:

### Phase 1 — Honest Model Advertisement
- Remove the dev-mode `Loaded` inflation in `buildRegistrationInfo`. Advertise `Loaded` exactly as the backend reports (via `ListModels`).
- `ListModels` now defaults `loaded := false` when the backend omits the field (was `true`). Backends that emit `loaded: true` (vLLM) are unaffected.
- Optional heuristic (dev mode only): if `InferenceTracker` has a recent sample (≥1 within 10-min rolling window) for a model, mark it `Loaded=true`. Backend truth wins; the heuristic is a fallback for backends that don't report load state.

### Phase 2 — Non-Destructive Stream Fallback
- Convert `streamingGracePeriod` from `const 5s` to package `var` default 30s, configurable via `--stream-grace` flag (0 = keep default 30s for back-compat).
- On grace expiry with zero chunks: **do not cancel the stream**. Keep waiting on the same stream until the backend's idle timeout (controlled by `--backend-timeout`, default 120s) or caller context cancels. The grace period is now a first-byte signal, not a hard deadline.
- New `isBackendFailure(err)` helper in `breaker.go`: returns `false` for `context.Canceled` / `context.DeadlineExceeded` (caller-side cancellations); `true` for all other errors (genuine backend failures). `CompleteChat` and `CompleteCompletions` guard `RecordFailure` with this helper.

### Phase 3 — Injectable Relay Call Timeout
- Convert `maxRelayCallTimeout` from `const 60s` to package `var` (60s default), injectable via `WSHub.SetRelayCallTimeout(d)` (d ≤ 0 → 60s for back-compat). Read at all 3 relay call sites in `ws_stats.go` (Complete, Stream, LoadModel).
- New `--relay-call-timeout` router flag (default 300s; 0 = keep 60s). Covers engine switch + prefill + generation for ~30B-class models on Apple Silicon.
- Unloaded-model requests in relay mode now fail fast (<1s, 503) instead of hanging to the 60s cap.

### Timeout Budget Ordering (Documented, No Code Change)
| Layer | Flag | Default | Type |
|-------|------|---------|------|
| Router streaming idle | `--proxy-timeout` | 120s | idle (resets per chunk) |
| Router relay hard cap | `--relay-call-timeout` | 300s | absolute (total) |
| Worker backend idle | `--backend-timeout` | 120s | idle (resets per SSE line) |
| Router retry budget | `--retry-budget` | 60s | absolute (across failover) |

Intended ordering: `proxy-timeout` (idle) fires first → 504; `relay-call-timeout` is the backstop for wedged workers.

## Consequences

### Positive
- Honest `Loaded` advertisement lets the router's `LoadedModelBonusWeight` / `ColdLoadPenaltyWeight` scorer work correctly.
- Non-destructive fallback stops aborting LM Studio engine switches mid-load.
- Circuit breaker no longer opens on client hangups / router timeouts (cascades eliminated).
- Relay call timeout is tunable per deployment/model size.

### Negative
- Dev-mode users who relied on auto-load via HTTP now get fast-fail 503 for unloaded models (breaking change for LAN dev mode; acknowledged).
- One more CLI flag per binary (`--stream-grace`, `--relay-call-timeout`); negligible maintenance.

### Future Considerations
- If the optional heuristic proves fragile (multi-model workers), it can be removed without protocol impact — back to trusting backend-reported `Loaded` only.
- A `--backend-timeout` default raise may be needed if the worker idle deadline races the router's proxy-timeout (both 120s today).

## Related Files
- `pkg/worker/run.go` — `buildRegistrationInfo` honest Loaded, InferenceTracker heuristic
- `pkg/worker/openai.go` — `ListModels` default loaded=false; `isBackendFailure` guard on breaker
- `pkg/worker/ws_client.go` — `streamingGracePeriod` var; non-destructive fallback in `streamOrFallback`
- `pkg/worker/breaker.go` — `isBackendFailure` helper
- `pkg/router/ws_relay.go` — `maxRelayCallTimeout` var
- `pkg/router/ws_hub.go` — `relayCallTimeout` field, `SetRelayCallTimeout`, `relayCallTimeoutValue`
- `pkg/router/ws_stats.go` — 3 call sites read hub timeout
- `cmd/worker/main.go` — `--stream-grace` flag
- `cmd/router/main.go` — `--relay-call-timeout` flag
- Tests: `pkg/worker/run_test.go`, `ws_client_test.go`, `circuit_breaker_test.go`, `server_streaming_test.go`, `cold_load_adversarial_test.go`; `pkg/router/server_chat_test.go`, `ws_relay_adversarial_test.go`

---

*Last updated: 2026-11-15*