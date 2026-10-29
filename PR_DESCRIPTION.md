# PR Description (Draft Artifact)

**This is a draft documentation artifact — do NOT create an actual PR from this file.**

---

## Summary

SIGHUP hot-reload for worker network registration settings (Issue #188). Workers can now re-read router/relay URLs, API keys, and model filters from environment variables without restarting, while in-flight inference requests continue uninterrupted.

---

## What Changed

### Core Implementation (`pkg/worker/`)

| File | Changes |
|------|---------|
| `run.go` | Added `ValidateRunConfig`, `ReloadableConfig`, `Handle` fields (`regMu`, `regCancel`, `regConfig`, `rootCtx`, mTLS static params), `launchRegistration`, `startHTTPRegistration`, `launchMTLSRegistration`, `buildRegistrationInfo`, `ReloadConfig`, `effectiveReloadConfig`, `newReloadableConfig`, `extractHost`, `isLoopback` |
| `server.go` | Added `SetAPIKey`, `currentAPIKey`, per-request API key read in `wrapInferenceEndpoints`, `filtersMu` protection on `SetModelFilters`/`ApplyModelFilters` |
| `cmd/worker/main.go` | SIGHUP signal handler (goroutine loop; SIGINT/SIGTERM still shut down; SIGHUP logged on failure) |

### Test Coverage (`pkg/worker/run_test.go`)

9 new tests covering reload and validation:

| Test | Purpose |
|------|---------|
| `TestEffectiveReloadConfig` | Env var reading with empty-value preservation |
| `TestReloadConfig_ReloadsRouterBase` | Integration: registration loop switches to new router URL |
| `TestReloadConfig_SwapsRelayToHTTP` | Config swapping logic: relay ↔ HTTP path |
| `TestValidateRunConfig_CatchesMisconfigs` | Prod-mode relay rejection, non-loopback router rejection |
| `TestReloadConfig_AppliesModelFiltersToHeartbeats` | `INFERMESH_ALLOWED_MODELS`/`EXCLUDED_MODELS` propagate to heartbeats |
| `TestReloadConfig_PropagatesRelayAPIKeyToRegistration` | Relay API key included in WorkerInfo after reload |
| `TestReloadConfig_ConcurrentWithInflightInference` | Reload does not disrupt in-flight requests (via `SetModelFilters` concurrency) |
| `TestReloadConfig_RejectsInvalidConfig` | Failed reload preserves old `regConfig` |
| `TestReloadConfig_ChangesAPIKey` | `SetAPIKey` swaps inference auth; old key rejected, new key accepted |
| `TestReloadConfig_RestartsMTLSLoop` | **Real mTLS**: self-signed CA + stub router; verifies mTLS heartbeat continues after SIGHUP |

---

## What is NOT Hot-Reloadable (Requires Restart)

| Setting | Reason |
|---------|--------|
| `--port` / worker listen port | HTTP server bind is static |
| `--dev-mode` / `--prod-mode` | Mode determines security stack (mTLS vs plain) |
| `--mtls-cert`, `--mtls-key`, `--cert-dir` | Cert rotation out of scope (static params stored at startup) |
| `--backend` / `--backend-url` / `--model-path` | Backend adapter and model declaration are startup-only |
| `--model-refresh-interval` / `--health-check-interval` | Ticker intervals fixed at startup |
| `--threads` (llama-cpp) | Backend metric seeded once |

---

## Security Notes

- **Threat model:** LAN-trusted. The worker runs on operator-controlled machines; SIGHUP is a local signal (typically `kill -SIGHUP <pid>` or container orchestration).
- **Keys never logged:** `ReloadConfig` logs `api_key_set` (boolean) and key values are never printed.
- **Empty env var = preserve current:** Setting an env var to empty string (`INFERMESH_API_KEY=`) does **not** clear the key; it keeps the current effective value. To clear a key, restart with the flag unset.
- **Validation on reload:** `ValidateRunConfig` rejects relay URL in prod mode and non-loopback router in prod mode, preventing silent registration failures.
- **mTLS certs static:** SIGHUP reuses the cert/key/CA paths captured at startup. Cert rotation requires restart.

---

## Out-of-Scope Stray Change (Issue #190)

The reviewer flagged a change in `app/frontend/src/App.vue` that is unrelated to Issue #188:

```typescript
// In handleStartRouter / handleStopRouter / handleRestartRouter:
await routerAuth.fetch()  // Re-reads router auth state for sidebar indicator
```

This change (3 occurrences) updates the desktop app's router auth indicator after the supervisor starts/stops/restarts the router. It was bundled in the same commit/PR but belongs to **Issue #190** (router auth indicator sync).

**Recommendation:** Mention in the commit message or split into a separate PR:
> "Also includes a stray fix for Issue #190: routerAuth.fetch() after router lifecycle changes in App.vue (desktop app only)."

---

## Files Modified (Summary)

```
pkg/worker/run.go              # Core reload logic, validation, registration loop swap
pkg/worker/server.go           # Dynamic API key, model filters thread-safety
cmd/worker/main.go             # SIGHUP signal handler
pkg/worker/run_test.go         # 9 new tests (unit + integration + real mTLS)
README.md                      # SIGHUP documentation block (pre-existing, verified)
app/frontend/src/App.vue       # Stray: routerAuth.fetch() for Issue #190 (3 lines)
```

---

## Verification

- `make test` — all unit tests pass (including 9 new reload tests)
- `make test-integration` — integration tests pass
- `make test-e2e` — E2E tests pass
- `make lint` — golangci-lint clean
- Manual: `kill -SIGHUP <worker-pid>` triggers reload; logs show "registration config reloaded via SIGHUP"

---

## Related Issues

- **Primary:** #188 — SIGHUP hot-reload for worker registration settings
- **Stray:** #190 — Router auth indicator sync in desktop app (App.vue)
- **Validation reference:** #152 — Registry event-drop risk under high churn (documented, no code change)