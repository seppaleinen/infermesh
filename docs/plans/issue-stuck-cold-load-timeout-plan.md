# Plan: Fix "stuck at 0%" — cold model load vs. timeouts (issues: dev-mode Loaded inflation, 5s stream grace, 60s relay cap)

Status: [COMPLETED]
REWORK_COUNT: 0
Last updated: 2026-11-15

Completed work:
- Dev-mode Loaded fix: No longer inflates Loaded=true for all backend-advertised models
- Circuit breaker ctx-cancellation fix: isBackendFailure excludes context.Canceled/DeadlineExceeded  
- Stream grace period: Configured via --stream-grace (default 30s), non-destructive fallback implemented
- Relay call timeout: Configurable via --relay-call-timeout (default 300s)

All acceptance criteria met. See test files for verification:
- `pkg/worker/run_test.go` (dev-mode honest advertisement)
- `pkg/worker/ws_client_test.go` (grace non-destructive, ctx-cancel)
- `pkg/worker/circuit_breaker_test.go` (breaker ctx-cancellation)
- `pkg/router/ws_relay_adversarial_test.go` (injectable relay cap)
- `pkg/router/server_chat_test.go` (unloaded model fails fast)
- `pkg/worker/server_streaming_test.go` (stream grace behavior)
- `pkg/worker/cold_load_adversarial_test.go` (race conditions)
Diagnosed from: live logs 2026-11-15 (router `/meta/requests/inflight`, `avg_wait_ms: 60002`,
LM Studio server log, relay/router pod logs). See "Diagnosis" below for the evidence chain.

## Diagnosis (locked)

An opencode session routed to model `ornith-1.5-35b-a3b-gsq-rco` while the single-slot LM Studio
backend only had `qwen3.8-27b-gsq-rco` loaded. Chain:

1. **Dev-mode registration inflates `Loaded`** — `pkg/worker/run.go:320`
   (`buildRegistrationInfo`): when `regDevMode`, every model the backend advertises gets
   `Loaded = true`. LM Studio's `/v1/models` lists 12 models with no `loaded` field, so the
   worker advertises all 12 as loaded and the router dispatches unloaded models.
2. **The router's auto-load path can't work in relay mode** — `buildCandidates`
   (`pkg/router/dispatch.go:43-49`) treats `Loaded=true` workers as candidates; when no loaded
   candidate exists it calls `loadModelOnWorker` (`dispatch.go:458`), which dials
   `http://workerIP:8081/v1/models/load`. A NAT/remote worker (relay transport, no inbound path)
   makes that HTTP dial time out, so the worker is silently excluded → `"no worker has the
   requested model"` or a fallback that will never load.
3. **The worker's 5s streaming grace period aborts engine loads** — `pkg/worker/ws_client.go:31`
   (`streamingGracePeriod = 5 * time.Second`, used at line 404): if the backend's first stream
   chunk doesn't arrive in 5s, `streamOrFallback` assumes "backend doesn't stream" and **cancels
   the in-flight request**, then retries non-streaming via `retryWithBackoff(3)`. LM Studio,
   mid engine-switch on a 27B model, gets `Client disconnected. Stopping generation...` →
   `Engine protocol startup was aborted` → 400.
4. **Circuit breaker amplifies it** — `pkg/worker/openai.go:147-164`: `CompleteChat` counts every
   failure (incl. the 400 from step 3) via `RecordFailure`; 5 failures → open. For ~30s every
   call fails in ~5s with `circuit breaker is open for backend http://127.0.0.1:1234`
   (observed in router logs at 12:41:45/12:41:58). Note `IsHealthy`/health-check probes use
   `CanExecute` which *transitions* the breaker out of open — a side channel that races inference.
5. **Timeouts are tuned for warm models, not cold loads** — observed per-request outcomes
   (relay mode): stream request `stream idle timeout` at exactly `proxyTimeout` 120s
   (`pkg/router/server.go:146`); relay hard cap `maxRelayCallTimeout = 60s const`
   (`pkg/router/ws_relay.go:35`) kills calls at 60s; non-streaming fallback dies on the 60s relay
   cap before the worker's own 120s backend deadline (`pkg/worker/openai.go:94`); router
   `--retry-budget` default 60s (`cmd/router/main.go:178`). A 27B-class model switch + prefill of
   a 40k-token context can exceed 60s, so even a *working* cold load is torn down.
6. The client (opencode) sees the timeout/error, retries, and the loop repeats — each retry
   re-triggers the engine switch (LM Studio log: `Client disconnected... finish first` → next
   `POST /v1/chat/completions` restarts the engine).

## Approach (three phases, low blast radius)

### Phase 1 — Honest model advertisement (the root cause)

Stop lying about `Loaded` so the router stops routing to models that aren't in VRAM.

- **`pkg/worker/run.go` (`buildRegistrationInfo`)**: remove the blanket
  `models[i].Loaded = true` loop in dev mode. Dev mode means *no auth*, not *all models loaded*.
  Keep it for manually declared models (`--model-path` — those really are loaded by the backend
  process) and for backends where "available" ≈ "loaded" semantics are unavoidable; for
  LM Studio/llama.cpp, trust what the backend reports.
- **`pkg/worker/openai.go` (`ListModels`, ~line 284-334)**: preserve the backend's own
  `loaded` field when present (LM Studio currently returns none → keep `false`); extend
  `ModelInfo` if needed. Do NOT infer "loaded" from "in catalogue".
- **Optional real signal (recommended, small):** worker tracks which model it last completed a
  successful inference on (`inferenceStats` is already per-model, `pkg/worker/server.go:231`).
  On each `SetModels`/registration heartbeat, mark the model with recent successful samples as
  `Loaded=true` and others `false`. This gives the router a truthful, self-maintaining signal
  without a protocol change: on LM Studio, the just-used model is the loaded one.
- **Router side (no gate needed):** `buildCandidates` (`dispatch.go`) already prefers
  `Loaded=true` and falls back to auto-load + `scorerWeightsEnabled()` candidates. With honest
  `Loaded`, the scheduler's existing `LoadedModelBonusWeight`/`ColdLoadPenaltyWeight`
  (`pkg/scheduler/scheduler.go:93-326`) will naturally prefer warm models. Verify a request for an
  unloaded model in relay mode fails **fast** (`no worker has the requested model` /
  `all unhealthy`) instead of dispatching into the 60s cap. That fast-fail is the correct UX for
  "model not on this worker": the client retries/switches models instead of hanging.

### Phase 2 — Stop aborting backends that are loading (stream grace + breaker)

- **`pkg/worker/ws_client.go`**: make `streamingGracePeriod` configurable
  (`--stream-grace` flag, default 30s) and, more importantly, make the fallback *non-destructive*:
  when the grace window expires with zero chunks, do not cancel+retry the stream. Instead, keep
  waiting on the same request until the backend's own idle deadline
  (`--backend-timeout`, `pkg/worker/openai.go:94`, 120s default) or the caller ctx. Rationale:
  "no chunk yet" is ambiguous — it means either "backend doesn't stream" OR "backend is loading
  the model / prefilling a huge context". Cancelling the engine switch is what produces
  `Engine protocol startup was aborted`. (Keep the existing 5s fast path ONLY if the backend
  reports it doesn't stream — out of scope; document the ambiguity instead.)
- **`pkg/worker/openai.go` / `breaker.go`**: don't feed the circuit breaker from client-abort
  cascades — `RecordFailure` only on *backend* failures (non-2xx, connection errors, idle
  timeout), never on context-cancellation. Currently `CompleteChat`'s `retryWithBackoff` counts
  every attempt; a 400 from an aborted engine-start is a backend failure, but a ctx cancel is not
  a backend fault. Add an explicit check: if `errors.Is(err, context.Canceled)` → no
  `RecordFailure`.
- **LM Studio engine-switch protection (optional, bigger):** worker detects
  `"Engine protocol startup was aborted"` 400s and treats them as *non-fatal, non-retryable*
  (return a clear error to the router: `model_not_loaded`), so the router can surface
  503-with-model-not-loaded to the client instead of burning `maxAttempts` retries.

### Phase 3 — Timeout budget for cold loads (raise the 60s)

Make the relay hard cap a first-class, configurable duration instead of a 60s const, with a
default sized for 27B-class cold loads on consumer hardware.

- **`pkg/router/ws_relay.go:35`**: convert `maxRelayCallTimeout` from `const` to a `var`
  (or WSHub field) so it is injectable — existing tests explicitly note it is "a CONST (60s),
  not injectable" (`ws_relay_adversarial_test.go:40,517`) and work around it with short caller
  contexts. Injecting it also unblocks a real cap test.
- **Plumbing**: `WSHub.SetRelayCallTimeout(d)` + router CLI flag
  `--relay-call-timeout` (default **300s** — covers engine switch + prefill + generation for
  ~30B-class models on Apple Silicon; 0 = keep 60s for parity/back-compat). Apply in all three
  call sites in `pkg/router/ws_stats.go` (Complete line 313, Stream line 377, LoadModel line 408)
  — they read the same value through the hub.
- **Keep the router's streaming idle timeout as the primary kill switch**: `--proxy-timeout`
  already exists (120s default) and is *idle* (resets on every chunk) — it's the right primitive
  for "no progress" and should stay below the relay cap so it fires first and emits the 504.
  Document the intended ordering: `proxy-timeout` (idle, 120s) < `relay-call-timeout` (total,
  300s) < worker `backend-timeout` (120s idle). Also raise worker `--backend-timeout` default
  for streaming to ≥ the proxy-timeout so the worker's idle deadline doesn't race the router's.
- **Non-streaming path**: `--retry-budget` (60s) and the per-attempt backend absolute timeout
  (120s) already exist; document that cold loads on the non-streaming path should be run with a
  raised `--retry-budget`, or add a `--cold-load-budget` note. No code change required in this
  phase beyond the relay cap.

## Files (pkg/ + cmd/; app/ untouched)

- `pkg/worker/run.go` — `buildRegistrationInfo` (remove dev-mode Loaded inflation)
- `pkg/worker/openai.go` — `ListModels` loaded-fidelity; `CompleteChat`/`CompleteCompletions`
  breaker accounting; `StreamChat` ctx-cancellation guard
- `pkg/worker/breaker.go` — (optional) `RecordFailure` ctx-awareness helper
- `pkg/worker/ws_client.go` — `streamingGracePeriod` → configurable + non-destructive fallback
- `pkg/worker/inference_stats.go` — (optional) per-model "recently completed" lookup for Phase 1
  real signal
- `pkg/router/ws_relay.go` — `maxRelayCallTimeout` const → var/field
- `pkg/router/ws_stats.go` — read cap from hub (3 call sites)
- `cmd/worker/main.go` — new flags: `--stream-grace`
- `cmd/router/main.go` — new flag: `--relay-call-timeout`; wire `SetRelayCallTimeout`
- Tests: `pkg/worker/run_test.go` (dev-mode Loaded no longer inflated),
  `pkg/worker/ws_client_test.go` (grace-period fallback keeps waiting; ctx-cancel doesn't trip
  breaker), `pkg/worker/breaker_test.go`, `pkg/router/ws_relay_adversarial_test.go`
  (injectable cap → real cap test), `pkg/router/dispatch_test.go` (unloaded model in relay mode
  fails fast, doesn't hang 60s)

## Trade-offs rejected

- **Raise the 60s cap to ∞ / remove it** — rejected; the cap exists to prevent indefinite
  hangs when the relay link is alive but the worker is wedged (issues #197/#199). A configurable
  finite cap with the idle `proxy-timeout` as the primary guardrail is the right split.
- **Protocol change to carry load state / load-ack over WS** — bigger blast radius; the
  "recently completed inference ⇒ loaded" heuristic + honest `Loaded` flag achieves the same
  routing behavior without a protocol change.
- **Router-side model-load over the relay** — `relayWorkerClient.LoadModel` exists but the worker's
  `loadModelByName` is an in-memory flag flip (no real backend load); LM Studio won't load on a
  flag flip. Real remote load is a separate, larger feature — out of scope here.
- **Making LM Studio single-slot → multi-slot / engine warm** — a backend config concern, not an
  infermesh code change; note it in the README instead.

## Acceptance criteria

- [ ] Dev-mode worker registration no longer marks all backend models `Loaded=true`;
      `/v1/workers.loaded_models` reflects reality (on LM Studio: only the loaded model).
- [ ] Requesting an unloaded model in relay mode fails fast (<1s, 503) instead of hanging 60s.
- [ ] A cold model switch longer than 5s no longer produces
      `Engine protocol startup was aborted` on the backend, and does not trip the circuit breaker.
- [ ] `--relay-call-timeout` is wired end-to-end and the existing 60s const is injectable in tests
      (new test asserts the cap fires).
- [ ] `make test` + `make lint` green; coverage target ≥80% on pkg/ maintained.
- [ ] Live repro: opencode → router → worker → LM Studio with a cold model completes without
      the flapping loop (verify via `/meta/requests/inflight` `duration_ms` and no
      `stream idle timeout` at the old 120s boundary).

## Risks / open

- The "recently completed ⇒ loaded" heuristic assumes one-model-at-a-time backends (LM Studio
  single slot, llama.cpp). Multi-model backends (vLLM) already report `loaded` correctly and are
  unaffected.
- Removing the dev-mode `Loaded=true` may change behavior for LAN users who relied on auto-load
  via the HTTP `loadModelOnWorker` path — that path works for LAN (direct IP) and is unchanged;
  only the *advertisement* becomes honest. Verify the LAN auto-load e2e still passes.
- Raising `--relay-call-timeout` to 300s means a truly wedged worker holds a slot up to 5min
  instead of 1min — mitigated by `max_in_flight` (4) still rejecting excess with 429, and by the
  idle `proxy-timeout` firing first in the common case.
- Phase 2's non-destructive stream fallback must still honor the caller ctx (client hangup) so
  we don't keep a dead client's generation running — the existing `ctx.Done()` case in
  `streamOrFallback` already does this; keep it.

## Next step

Await user approval; then dispatch `dev-team-lead` with this brief (pipeline:
team-lead → dev-team-lead → dev-architect → plan-auditor → backend-engineer → test-engineer ∥
adversarial-tester → code-reviewer → security → docs → devops-cleanup → post-mortem-analyst).
Pre-existing uncommitted change on disk (`pkg/protocol/api.go` + `pkg/worker/openai_test.go`,
issue #204 `reasoning_content`) is unrelated to this plan — leave it to the user to commit or
revert before dispatch so the branch starts clean.
