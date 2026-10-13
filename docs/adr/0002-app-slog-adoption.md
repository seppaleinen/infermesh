# ADR-0002: App Slog Adoption

**Status:** Accepted

**Date:** 2026-10-13

## Context

InferMesh has two distinct logging conventions across the codebase:

- **`pkg/` (headless router/worker/relay):** Uses `slog` with injected `*slog.Logger` throughout
- **`app/` (Wails desktop app):** Uses stdlib `log` with 12 call sites in `main.go`/`activation.go`

Two options were considered:

**Option 1 (chosen):** Keep separate logging conventions — app uses `log`, pkg uses `slog`
**Option 2 (rejected):** Migrate `app/` to `slog` for consistency

## Decision

**Keep separate logging conventions. The desktop app will continue using stdlib `log`; the headless components will continue using `slog`.**

The divergence is intentional and pragmatic, not accidental.

### Rationale

1. **Different audiences:** The desktop app logs lifecycle events, single-instance guards, and activation listeners — all human-readable one-liners surfaced to a desktop user. Structured logging buys nothing for this use case.

2. **Console UX:** Desktop users expect simple, readable output. Operators of headless router/worker/relay benefit from structured logs for filtering, aggregation, and operational debugging.

3. **Complexity cost:** Migrating the app to `slog` would require passing logger instances through Wails bindings, adding complexity with minimal benefit. The app is already a thin wrapper around the pkg components.

4. **Pragmatic divergence:** Different parts of the system have different needs. Forcing uniformity would be premature abstraction.

### Current state

- `app/main.go` and `app/activation.go` use stdlib `log` (12 call sites)
- `pkg/` packages use `slog` with dependency injection
- No logging bridge between app and pkg is required for the MVP

## Consequences

### Positive

- Desktop app retains simple, readable logging for users
- Headless components benefit from structured logging for operators
- No refactoring required for the app layer
- Clear separation of concerns: UI layer vs. service layer

### Negative

- Two logging conventions to document and maintain
- Developers must be aware of the context-appropriate logging style
- Inconsistent log output format across components

### Future considerations

If the app grows to support centralized logging or log export features, re-evaluation of a unified logging approach may be warranted. For the current MVP scope with desktop-local usage, the divergence is acceptable.

## Related files

- `app/main.go` — Uses stdlib `log` for app lifecycle
- `app/activation.go` — Uses stdlib `log` for activation listener
- `pkg/*` — Uses `slog` with injected logger

---

*Last updated: 2026-10-13*
