# Plan: Issue #84 — Auto-enroll GUI as worker (reuse binary)

Source issue: https://github.com/seppaleinen/infermesh/issues/84
Status: [PLANNING]
REWORK_COUNT: 0

## Clarification (locked)
- GUI = lifecycle manager / supervisor hook only.
- Spawns `bin/infermesh-worker` via `supervisor.go`; binary handles proxy to external engine (`--backend` ollama/lmstudio/etc.).
- No stub, no proxy-through-GUI.

## Approach (incremental phases, low blast radius)
1. **Supervisor hook** — add `StartWorker()` to `app/supervisor.go` (existing `ProcessStatus.Worker` / `Registered` tracking).
2. **Config/connection trigger** — `app/config_service.go` / `router_client.go`: call supervisor on successful router-connect / settings-save.
3. **Frontend status** — `App.vue`: show enrollment / `Registered == true` via supervisor service.
4. **Auth pass-through** — use existing `keyring.go` / env vars (`secretRefMTLSCert`, `envWorkerCustomAuth`) so child inherits dev/prod mode correctly.
5. **Auto-start on connect** — wire settings-save → supervisor start (optional toggle; default on).

## Files (app/ only; binary reused)
- `app/supervisor.go` — StartWorker, ProcessState
- `app/config_service.go` — trigger
- `app/router_client.go` — success event hook
- `app/frontend/src/App.vue` — status display
- `app/autostart.go` — optional extend
- `docs/plans/issue-84-plan.md` (this file)

## Trade-offs rejected
- Embedding `pkg/worker/` — duplicate, rejected by user.
- Changing router protocol — unnecessary.
- Full auto-start without toggle — deferred to phase 5.

## Acceptance Criteria (from #84)
- [ ] GUI connects to router → worker subprocess starts automatically (dev/prod respected)
- [ ] `/v1/workers` shows new node; supervisor `Registered == true`
- [ ] UI reflects state; no duplicate source in `app/`

## Risks / open
- `bin/infermesh-worker` gitignored; build/check before spawn.
- mDNS announcer conflict if external inference worker also runs locally (`--instance-name` / `--local-only`).
- `pkg/scheduler` sees real capabilities from backend; no stub handling needed.
- Prod auth: supervisor must pass cert/key env/args correctly to child process.

## Next step
Await user approval of phases; then dispatch `dev-team-lead` with brief + plan (pipeline: team-lead → dev-team-lead → dev-architect → backend-engineer → test-engineer → code-reviewer; post-verification → devops-cleanup + post-mortem-analyst).
