# Research Brief: Remove mandatory --model-path requirement in prod mode (Issue #186)

## Problem Statement

`pkg/worker/run.go` returns `ErrMissingModelPath` when a worker starts in production mode without `--model-path` and the backend is `llama-cpp` (lines 149-152, 222-224). This blocks workers that:

- Use llama-cpp but load models dynamically (e.g. via a model catalogue the backend exposes)
- Start before the backend is ready; models should be discovered on the next refresh cycle
- Want to declare models via the router's SetModels API rather than a static file path

The check is duplicated: once before backend setup (line 149) and again inside the auto-discovery failure path (line 222).

## Current Code Locations

### Primary enforcement (pkg/worker/run.go)
- Line 21-23: `ErrMissingModelPath` error definition
- Line 149-152: First `requireModelPath` check before backend setup
- Line 222-224: Second `requireModelPath` check in auto-discovery failure path

### Test (pkg/worker/run_test.go)
- Line 213-222: `TestRunWorkerRequiresModelPathInProd` - expects the error

### CLI error handling (cmd/worker/main.go)
- Line 214-219: Special-case handling for `ErrMissingModelPath`

### Flag help text (cmd/worker/main.go)
- Line 85: `--model-path` flag description says "path to the model file" (implies required)

## Desired Solution

Remove the `ErrMissingModelPath` requirement entirely:

1. Delete `ErrMissingModelPath` from `pkg/worker/run.go`
2. Remove both `requireModelPath` checks (lines 149-152 and 222-224)
3. The worker should start without `--model-path`; models are discovered dynamically via `ListModels()` or declared later via `SetModels`
4. Update or remove `TestRunWorkerRequiresModelPathInProd` in `pkg/worker/run_test.go`
5. Update `cmd/worker/main.go` error handling for `ErrMissingModelPath` (lines 214-219) — the special-case branch becomes dead code
6. Update the `--model-path` flag help text in `cmd/worker/main.go` line 85 to reflect that it is optional in all modes

## Acceptance Criteria

- Worker starts in prod mode without `--model-path` for all backends (llama-cpp, ollama, lmstudio, vllm, custom)
- No `ErrMissingModelPath` error returned in any path
- Tests updated and green
- `make lint` passes

## Constraints

- No new Go dependencies
- Follow existing code conventions
- Coverage must not regress
- This is a pure code change (no infrastructure/devops work)

## Expected Output

Modified files:
- `pkg/worker/run.go` - remove ErrMissingModelPath and both requireModelPath checks
- `pkg/worker/run_test.go` - update/remove TestRunWorkerRequiresModelPathInProd
- `cmd/worker/main.go` - remove ErrMissingModelPath error handling, update flag help text
- All tests pass, `make lint` clean