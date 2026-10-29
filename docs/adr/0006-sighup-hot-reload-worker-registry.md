# ADR-0006: SIGHUP Hot-Reload for Worker Network Registration Settings

**Status:** Accepted

**Date:** 2026-10-29

## Context

Workers register with the router via three paths (mDNS, HTTP, mTLS) and relay (WebSocket). Changing registration settings (router URL, relay URL, API keys, model filters) required a full worker restart, which:

- Disrupts in-flight inference requests
- Breaks the worker's pool presence (registration gap until restart completes)
- Is operationally painful for containerized/homelab deployments where only network config changes

The worker already supports dynamic model discovery and periodic capability refresh. The gap was a mechanism to re-read network-level registration config without restarting the HTTP server or backend.

## Decision

**Add SIGHUP hot-reload for worker network registration settings.**

- Worker listens for `SIGHUP` (via `os/signal` in `cmd/worker/main.go`)
- On signal, `Handle.ReloadConfig()` re-reads six environment variables:
  - `INFERMESH_ROUTER_URL`
  - `INFERMESH_RELAY_URL`
  - `INFERMESH_RELAY_API_KEY`
  - `INFERMESH_API_KEY`
  - `INFERMESH_ALLOWED_MODELS`
  - `INFERMESH_EXCLUDED_MODELS`
- Empty env var = keep current effective value (preserves backward compatibility)
- Only the registration heartbeat loop is cancelled and restarted; in-flight inference requests are NOT disrupted
- Startup validation (`ValidateRunConfig`) catches common misconfigurations on reload:
  - Relay URL in prod mode (relay is dev-only)
  - Non-loopback router base in prod mode (router's prod loopback check would reject it)
- Inference API key is dynamic: `Server.SetAPIKey` + per-request read in `wrapInferenceEndpoints`, so `INFERMESH_API_KEY` changes apply without restart
- mTLS workers: SIGHUP re-launches the mTLS registration loop with stored static cert params (cert rotation is out of scope)
- Relay→HTTP fallback is preserved on reload (relay reachability re-probed; unreachable relay + RouterBase set → HTTP)

## Consequences

### Positive

- Operators can change router/relay endpoints, API keys, and model filters without worker downtime
- In-flight inference requests continue uninterrupted
- Startup validation prevents silent registration failures after reload
- mTLS workers maintain registration across reloads (previously the mTLS loop was killed and not restarted)
- Empty env var semantics make it safe to reload a subset of settings

### Negative

- Added complexity in `Handle` struct: `regMu`, `regCancel`, `regConfig`, `rootCtx`, mTLS static params
- Lock ordering constraint: `filtersMu` → `apiKeyMu` → `regMu` (documented in `ReloadConfig`)
- Cert rotation still requires restart (out of scope for MVP)
- Port, dev/prod mode, backend type, model path, health check intervals still require restart

### Future considerations

- Cert rotation could be added by extending `ReloadableConfig` with cert paths and reloading TLS config
- A periodic reconciliation sweep (beyond the 120s registry TTL) could mitigate event-drop risk under extreme churn (Issue #152)

## Related files

- `pkg/worker/run.go` — `ValidateRunConfig`, `ReloadableConfig`, `Handle`, `launchRegistration`, `startHTTPRegistration`, `launchMTLSRegistration`, `buildRegistrationInfo`, `ReloadConfig`, `effectiveReloadConfig`, `newReloadableConfig`, `extractHost`, `isLoopback`
- `pkg/worker/server.go` — `SetAPIKey`, `currentAPIKey`, `wrapInferenceEndpoints`, `SetModelFilters`, `ApplyModelFilters`, `filtersMu`, `apiKeyMu`
- `cmd/worker/main.go` — SIGHUP signal handler
- `pkg/worker/run_test.go` — 9 reload/validation tests incl. `TestReloadConfig_RestartsMTLSLoop`, `TestReloadConfig_ChangesAPIKey`
- `README.md` — SIGHUP documentation block

---

*Last updated: 2026-10-29*