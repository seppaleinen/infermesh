# ADR-0005: Configurable Worker Backend Health Check Interval

**Status:** Accepted

**Date:** 2026-10-29

## Context

The worker's backend health check loop ran on a hardcoded 30-second cadence
(`DefaultHealthCheckInterval = 30 * time.Second`) with no operator control.
Operators running busy pools or slow backends had no way to tune how often the
worker probed backend health, and operators wanting faster failure detection
could not shorten the interval without editing source.

The existing `--model-refresh-interval` flag already demonstrated the desired
pattern: a `time.Duration` CLI flag defaulting to 0, resolved through an
`effective*` helper that falls back to a package-level default constant.

## Decision

**Thread a `--health-check-interval` flag through the worker CLI into
`RunConfig`, mirroring `--model-refresh-interval`.**

- `cmd/worker/main.go`: new `HealthCheckInterval time.Duration` field on
  `WorkerFlags`, registered as
  `--health-check-interval` (default 0, meaning "use the default").
- `pkg/worker/run.go`: `RunConfig.HealthCheckInterval` field; the health check
  loop call site passes `effectiveHealthCheckInterval(cfg.HealthCheckInterval)`.
- `pkg/worker/server.go`: new `effectiveHealthCheckInterval` helper, identical
  shape to `effectiveModelRefreshInterval` — 0 or negative resolves to
  `DefaultHealthCheckInterval` (30s).

The default is unchanged at 30s. Explicit values (e.g. `--health-check-interval 60s`)
override it.

### Rationale

1. **Consistency:** Mirrors the existing `--model-refresh-interval` flag, so the
   CLI surface for tuning periodic worker loops is uniform.
2. **Zero behavior change:** Default 0 preserves the current 30s cadence for
   every existing deployment.
3. **Small, reversible:** Flag threading only — no new goroutines, no new
   state, no protocol change.

## Consequences

### Positive

- Operators can slow health checks on idle pools (less noise) or speed them up
  for faster failure detection.
- The `effective*` helper keeps the zero-default convention consistent with the
  model refresh path.

### Negative

- One more CLI flag to document; negligible maintenance cost.

### Future considerations

- If health checks gain severity levels or per-backend intervals, this flag is
  the hook point; no redesign needed.

## Related files

- `cmd/worker/main.go` — `--health-check-interval` flag and `RunConfig` wiring
- `pkg/worker/run.go` — `RunConfig.HealthCheckInterval`, call site
- `pkg/worker/server.go` — `effectiveHealthCheckInterval`, `DefaultHealthCheckInterval`
- `pkg/worker/model_refresh.go` — the mirrored `effectiveModelRefreshInterval` pattern

---

*Last updated: 2026-10-29*