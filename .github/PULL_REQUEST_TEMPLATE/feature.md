## Summary

Worker opt-out of serving specific models via `--allowed-models` (whitelist) and
`--excluded-models` (blacklist) CLI flags, enforced by the scheduler before
scoring and in all router fallback paths.

## Changes

- **`pkg/protocol/worker.go`** — add `AllowedModels` / `ExcludedModels` fields to
  `Capabilities` (both `omitempty`, backward compatible).
- **`pkg/worker/server.go`** — `Server.SetModelFilters` / `ApplyModelFilters`;
  `/capabilities` handler emits the filter lists.
- **`pkg/worker/run.go`** — `RunConfig.AllowedModels` / `ExcludedModels`;
  `splitModelList` parser; `ApplyModelFilters` invoked in all three refresh
  closures (mDNS, dev HTTP, prod mTLS, relay WS).
- **`pkg/scheduler/scheduler.go`** — `FilterByAllowedModels` implementing the
  whitelist-over-blacklist precedence rule; wired into `SelectWorker` after
  `FilterUnavailable` (filters before scoring).
- **`pkg/router/server.go`** — `toSchedulerWorker` copies both flags for every
  registration path; `selectWorkerExcluding` fallback applies
  `FilterByAllowedModels` in `advertising-model` and `any-available-worker`
  fallbacks.
- **`cmd/worker/main.go`** — `--allowed-models` and `--excluded-models`
  comma-separated CLI flags (spaces trimmed).
- **`pkg/scheduler/scheduler_test.go`** — `TestFilterByAllowedModels`.
- **`pkg/worker/server_test.go`** — `TestCapabilitiesHandler_IncludesModelFilters`,
  `TestCapabilitiesHandler_NoFiltersWhenUnset`.
- **`pkg/worker/run_test.go`** — `TestRunWorker_AllowedExcludedModels`.
- **`tests/e2e/worker_optout_test.go`** — 13 end-to-end subtests covering
  whitelist-only, blacklist-only, combined, fallback-path filtering, and
  backward-compatible (unset) behavior.
- **`docs/contracts/worker-optout-v1.md`** — ADR documenting the design.

## Testing

```bash
# Unit
make test                                       # pkg/* coverage incl. scheduler + worker tests
go test ./pkg/scheduler/... -run TestFilterByAllowedModels -v
go test ./pkg/worker/... -run "TestCapabilitiesHandler|TestRunWorker_AllowedExcludedModels" -v

# E2E (full pool with opt-out assertions)
make test-e2e
go test ./tests/e2e/... -run TestWorkerOptOut -v -count=1

# One-machine smoke (dev mode, no mDNS):
./bin/infermesh-router --dev-mode
./bin/infermesh-worker --dev-mode --router http://127.0.0.1:8080 \
  --backend lmstudio --model-path /path/to/model \
  --allowed-models "gpt-oss-20b"
curl http://127.0.0.1:8080/v1/models
```

Acceptance: `make test`, `make test-e2e`, and `make lint` all pass on Go 1.27.

## Breaking Changes

None. `AllowedModels` / `ExcludedModels` use `omitempty` and default to empty
(no restriction), so existing workers and routers behave identically without
flags. The new fields are additive to `Capabilities`.

## Issue

Closes #37
