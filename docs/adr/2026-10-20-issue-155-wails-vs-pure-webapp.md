# ADR: Wails vs pure webapp (issue #155)

Date: 2026-10-20
Status: Accepted (keep Wails; migration deferred)

## Context

Issue #155 asks whether the Wails desktop app should be replaced with a
pure webapp. The stated rationale: Wails adds complexity, "doesn't seem
to support testing", and a simple web server showing the same information
"would work probably just as well" while making integration tests easier.

## Decision

**Keep Wails. Do not migrate to a pure webapp at this stage.**

## Consequences

### Positive

- Zero-friction first experience: user downloads a binary, opens it, and
  lands in the dashboard. No browser, no bookmark, no port to remember.
  This matches the "Airbnb for spare inference capacity" product定位.
- System tray is native and already implemented (`app/main.go`,
  `assets/trayTemplate.png`, `tools/gen-trayicon`). A pure webapp has no
  equivalent without a separate menubar app.
- Auto-start, activation dialogs, config persistence, and the process
  supervisor are already implemented across macOS/Linux/Windows
  (`app/autostart_*.go`, `app/activation_dialog_*.go`,
  `app/config_service.go`, `app/supervisor_process.go`).
- Testability claim is stale: `cd app && go test ./...` passes,
  `app/main_test.go` is GUI-free, and `app/smoke_test.go` runs under a
  `smoke` build tag in CI.

### Negative

- Wails is pinned to `v3.0.0-beta.24`, so release risk is real.
- Build complexity: requires `wails3` + Node + CGO. macOS verified; Linux
  needs webkit2gtk build deps; Windows desktop is deferred.
- Larger binary and slower iteration than a static webapp.

## Migration size estimate

A pure-webapp swap would **increase** net work, not reduce it. Wails
encapsulates the following that a pure webapp would need to reimplement:

| Capability | Where it lives today | Reimplementation cost |
|---|---|---|
| Launch arg parsing | `app/args.go` | low |
| Auto-start (3 platforms) | `app/autostart_backend.go` | medium |
| Activation dialogs | `app/activation_dialog_*.go` | medium |
| Config persistence | `app/config_service.go` | low |
| Process supervisor | `app/supervisor_process.go` | medium |
| System tray | `app/main.go` + assets | high (no web equivalent) |
| TS bindings | `wails3 generate bindings` | n/a (removed) |

The pipeline tests (`tests/integration/`, `tests/e2e/`) are orthogonal to
the UI layer — they already exercise the full router + worker flow without
any GUI. Replacing Wails would not make those tests easier; they already
exist and pass.

## Alternatives considered

### 1. Pure webapp (the proposal)

Rejected for MVP. Loses tray UX, requires reimplementation of
auto-start/dialogs/config, and gains only a simpler build chain — a trade
that does not pay for itself at MVP scope.

### 2. Hybrid: standalone HTTP dashboard + thin Wails launcher

A smaller, lower-risk alternative. The dashboard becomes a plain HTTP
service served by the router (or a separate process), reused by both the
Wails app (embedded window) and any future web/integration-test consumer.
Wails then degrades to a launcher + tray. This would decouple UI testing
from the desktop shell.

**Deferred.** Not needed for MVP. Revisit if web embedding or external
dashboard access becomes a requirement.

## When to revisit

- If the target user base is ops teams that prefer browser access over a
  downloaded binary.
- If the dashboard needs to be embeddable in Grafana or another control
  panel.
- If Wails v3 leaves beta and the build chain stabilises enough that the
  release risk is no longer a concern.