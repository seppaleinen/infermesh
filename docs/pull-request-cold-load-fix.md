# PR Description: Three-Phase Cold Model Load Fix (Issues #152, #197, #199, #201)

## Summary
Fixes the "stuck at 0%" cold model load flapping loop by making model advertisement honest, stopping the 5s stream grace from aborting engine switches, and raising the 60s relay hard cap to a configurable 300s default.

## Problem
When requesting a cold model (not in VRAM) via the router in relay mode:
1. Worker advertised all backend models as `Loaded=true` (dev-mode inflation)
2. Router dispatched to unloaded model → auto-load HTTP dial timed out (no inbound path)
3. Worker's 5s stream grace cancelled the engine switch → `Engine protocol startup was aborted`
4. Circuit breaker opened on the 400s → 30s of failed requests
5. 60s relay cap killed even valid cold loads before completion

## Solution (Three Phases)

### Phase 1 — Honest Model Advertisement
- **`pkg/worker/run.go`**: Removed dev-mode blanket `Loaded=true`. Only manual `--model-path` models and recently-completed-inference models (10-min rolling window via `InferenceTracker`) are advertised as loaded.
- **`pkg/worker/openai.go`**: `ListModels` preserves backend `loaded` field; absent → `false`.
- Router's existing scheduler weights (`LoadedModelBonusWeight`, `ColdLoadPenaltyWeight`) now naturally prefer warm models.

### Phase 2 — Non-Destructive Stream Fallback + Breaker Guard
- **`pkg/worker/ws_client.go`**: New `--stream-grace` flag (default 30s, was 5s const). On grace expiry with zero chunks: keep waiting on the **same stream** until backend idle timeout (`--backend-timeout`, 120s) or caller ctx cancels. No cancel+retry.
- **`pkg/worker/openai.go` / `breaker.go`**: `isBackendFailure()` guards `RecordFailure` — `context.Canceled` / `DeadlineExceeded` no longer trip the circuit breaker.
- Fixed pre-existing data race on `Server.chatHistory` (new `chatHistoryMu`).

### Phase 3 — Injectable Relay Call Timeout
- **`pkg/router/ws_relay.go` / `ws_hub.go`**: `maxRelayCallTimeout` const → var, injectable via `WSHub.SetRelayCallTimeout`.
- **`cmd/router/main.go`**: New `--relay-call-timeout` flag (default 300s; 0 = 60s for back-compat).
- Applied at all 3 relay call sites in `pkg/router/ws_stats.go` (Complete, Stream, LoadModel).
- Unloaded-model requests in relay mode now fail fast (<1s, 503) instead of hanging 60s.
- Timeout budget ordering documented: `proxy-timeout` (idle, 120s) < `relay-call-timeout` (total, 300s) < worker `backend-timeout` (120s idle).

### Security Fix
- Circuit-breaker error messages no longer leak internal backend URL in client-facing responses (URL retained in worker logs).

## New Flags
| Flag | Component | Default | Notes |
|------|-----------|---------|-------|
| `--stream-grace` | worker | 30s | 0 = keep default (30s), not disable (per contract for back-compat) |
| `--relay-call-timeout` | router | 300s | 0 = keep legacy 60s const |

## Behavior Changes
- Dev-mode `/v1/workers` now shows realistic `Loaded` state (only manual `--model-path` + recently used).
- Relay mode: requesting unloaded model → fast 503 (was 60s hang → "no worker has model").
- Cold model switches >5s no longer abort engine; wait up to `backend-timeout` (120s) on stream.
- Client aborts / router timeouts no longer open circuit breaker.

## Testing
- `make test` — green (all existing + new tests pass)
- `make test -race` — green (no data races; chatHistory mutex added)
- `make lint` — green
- Coverage: **65.6% overall** on `pkg/` (CI gate 50% satisfied; per-package varies, pre-existing state maintained)
- New test coverage:
  - `pkg/worker/run_test.go` — dev-mode honest advertisement
  - `pkg/worker/ws_client_test.go` — grace non-destructive, ctx-cancel
  - `pkg/worker/circuit_breaker_test.go` — breaker ctx-cancellation guard
  - `pkg/router/ws_relay_adversarial_test.go` — injectable relay cap (real cap test)
  - `pkg/router/server_chat_test.go` — unloaded model fails fast
  - `pkg/worker/server_streaming_test.go` — stream grace behavior
  - `pkg/worker/cold_load_adversarial_test.go` — race conditions

## Known Bundling Note
Commit `9205d01` also includes pre-existing unrelated changes for **Issue #204** (`pkg/protocol/api.go` `reasoning_content` field + `openai_test.go`) that were on disk before this work started. These are a separate logical change that landed in the same commit.

## Review Outcomes
- **code-reviewer**: Spec adherence confirmed. Required fixes applied (docs coherence, stale comment, dead lint exclusion removed). One suggestion (`--stream-grace=0` should disable) **REJECTED**: contract explicitly specifies 0 = keep default (30s) for back-compat.
- **ai-security-auditor**: 1 LOW finding fixed (URL leak in 2aaf29d). 2 pre-existing infra findings OUT OF SCOPE: Dockerfile.server runs as root; Helm chart defaults to image tag `latest` — filed as follow-ups.

## Related ADR
See `docs/adr/0007-cold-model-load-timeout-fix.md` for the full architectural decision record.

---

**Checklist for merge:**
- [ ] `make test` passes
- [ ] `make lint` passes
- [ ] `make test -race` passes
- [ ] ADR added (`docs/adr/0007-...`)
- [ ] CHANGELOG updated
- [ ] No unintended bundling (Issue #204 changes noted separately)