# Plan: Remaining work after the three-phase cold-load fix

Status: [PROPOSED]
Baseline: main @ 50df125 (pushed 2026-11-15); latest tag v0.1.69
Scope: infermesh repo only (pkg/, cmd/, tests/). app/ excluded unless noted.

## 1. Tag + release the current push (v0.1.70)

`main` is green and pushed. The release workflow triggers on push to `main`
and creates the next patch tag + GHCR images on CI success. Verify the
pipeline actually completed:
- [ ] `ci.yml` green on `50df125`
- [ ] Tag `v0.1.70` created (bumps patch from `v0.1.69`)
- [ ] GHCR images pushed (`router:v0.1.70`, `worker:v0.1.70`)
- [ ] Fleet-infra image tags bumped; Flux reconciles; pods roll to new images

## 2. Usage.CompletionTokensDetails — reasoning token accounting

The `reasoning_content` fix (issue #204) restored the content path, but
`protocol.Usage` still lacks the token-detail breakdown. LM Studio reports
`completion_tokens_details.reasoning_tokens` in the non-streaming response;
the worker's unmarshal→marshal drops it, so clients can't see reasoning-token
cost. Small, isolated, directly tied to the reasoning work just shipped.
- Add `CompletionTokensDetails` (and, if needed, `PromptTokensDetails`) to
  `protocol.Usage` with OpenAI-compatible JSON tags + `omitempty`.
- Preserve the field through the worker round-trip.
- Regression test: non-streaming backend response with
  `completion_tokens_details.reasoning_tokens` survives to the router.

## 3. Spec-test GAP closure

The cold-load commit added many `spec_*.go` tests, all
`t.Skip("implementation pending")`. Some assert contracts the code now
implements; others remain genuinely open (registry heartbeat recovery,
terminal breaker transitions, model-removal detection, issue #152 event-drop
risk). Un-skip incrementally per contract as each becomes true — do NOT
un-skip in bulk. Track which GAPs are real vs. already-covered by the new
tests.

## 4. Coverage gate

Target: ≥80% on `pkg/`. Run `make coverage` and close gaps in the newly
touched packages (`pkg/worker`, `pkg/router`) left by the cold-load work.

## 5. Live verification (cold-load repro)

Re-run end-to-end: opencode → router → worker → LM Studio with a cold model
switch. Confirm no `Engine protocol startup was aborted`, no 60s relay-cap
teardown, and a successful completion. Verify via `/meta/requests/inflight`
`duration_ms` and router/relay pod logs.

## 6. Deferred (out of scope for now)
- Windows desktop app build (`package-app-windows` is compile-check only)
- Multi-router HA
- WAN workers / CPU-only workers
- Router-side real model-load over the relay (currently an in-memory flag)
