# Issue #23 — Combine Both CLIs: Technical Plan

Status: planning only (no code in this document).
Scope: one `infermesh` binary with `router` and `worker` subcommands, plus a `--worker` flag on the router subcommand that runs an in-process worker registered against its own router.

## Constraints (from AGENTS.md)

- Go 1.27+, module `github.com/seppaleinen/infermesh`.
- Dependencies: stdlib + `github.com/hashicorp/mdns` + `gopkg.in/yaml.v3` — **no new dependencies**.
- CI must not regress: Go 1.27 pin, `golangci/golangci-lint-action@v9` at `v2.13.2`, `go vet ./...`, `make build`, `go test ./... -v -cover -race`.
- Coverage target: **80% on `pkg/`** (`make coverage`, funccover).
- Build output goes to gitignored `bin/`; root-level binaries must never be committed (stale untracked `router`/`worker` files remain on disk from before `a720766` — leave them untracked).

---

## 1. CLI shape

Single binary, two subcommands, dispatch on `os.Args[1]`. Each subcommand gets its own stdlib `flag.NewFlagSet` with `ExitOnError`. No global flags, no new parsing library.

```
infermesh <subcommand> [flags]

  router  [--dev-mode|--prod-mode] [router flags] [--worker] [worker passthrough flags]
  worker  [worker flags] [--capabilities]
```

Flag inventory (kept 1:1 with today's binaries so existing flag names keep working):

- `infermesh router` — `--dev-mode`, `--prod-mode`, `--mtls-cert`, `--mtls-key`, `--cert-dir`, `--api-key`, `--addr` (default `:8080`), plus the worker passthrough flags from section 2.
- `infermesh worker` — `--capabilities`, `--dev-mode`, `--prod-mode`, `--mtls-cert`, `--mtls-key`, `--port` (default 8081), `--cert-dir`, `--model-path`, `--backend` (llama-cpp | ollama | lmstudio | vllm | custom), `--router`, `--enable-health-checks` (default on).
- Bare `infermesh` (no subcommand) or unknown subcommand → usage text, exit 2.
- `--capabilities` remains a worker-only print-and-exit path.

Decision: per-subcommand FlagSets, not one combined FlagSet (avoids ambiguous flag conflicts, keeps parsing unit-testable per subcommand).

## 2. The new `--worker` flag on the router subcommand

`infermesh router ... --worker` starts a worker **in the same process** and registers it against the same router.

- **In-process, no fork.** Rejected alternative: forked subprocess. Signal handling, reaping, and crash isolation add complexity with no benefit — the actual inference backend is always an external HTTP process (LM Studio, Ollama, llama.cpp server, vLLM) regardless.
- **Dev mode only.** `router --prod-mode --worker` is a hard error (exit 2). Rationale: (a) dev mode is the only mode where registration is plain HTTP over loopback (`/v1/dev/register`), which is what makes one-process self-registration trivial; (b) the mTLS client cert is a single self-signed cert (SANs `localhost` + primary IPv4, CN "InferMesh CA") — sharing one cert across two roles in one process is a security-config quagmire not worth solving in MVP.
- **Worker passthrough flags on the router subcommand:** `--port` (default 8081), `--backend`, `--model-path`, `--enable-health-checks` (default on). No `--mtls-*` / `--cert-dir` in combined mode.
- **Registration URL derived from `--addr`** via `RouterBaseFromListenAddr(addr) (string, error)`:
  - `:8080`, `0.0.0.0:8080`, `localhost:8080` → `http://127.0.0.1:8080`
  - `127.0.0.1:9000` → `http://127.0.0.1:9000`
  - explicit non-loopback host preserved: `myhost:8080` → `http://myhost:8080`
  - scheme always `http` (dev-only mode).
  - Rejected alternative: a new `--worker-router` flag — duplicates data already in `--addr` and creates a disagreement failure mode.
- **API key:** `--api-key` is currently stored in `security.Config` but not enforced by any router middleware, so self-registration needs no auth wiring. Document this; do not add auth in this issue.
- **Startup order:**
  1. Parse flags; run fail-fast validation (below).
  2. Build and serve the router (dev mode: plain HTTP; routes `/v1/dev/register`, `/v1/workers`, `/v1/models`).
  3. Poll `/v1/workers` until it responds (≤ ~5s) — sanity check only; the register loop self-heals anyway (10s retry).
  4. Build the worker handle (backend + worker server + register loop, existing `RegisterLoopWithRefresh`).
- **Shutdown:** one SIGINT/SIGTERM handler; cancel a shared context → stop worker handle → close router listener → stop registry; exit 0. Stop the worker **before** closing the router so de-registration reaches the router; suppress `http.ErrServerClosed` log noise.
- **Logging:** two `slog` loggers with a `role` attribute (`router` / `worker`) so interleaved output stays readable.

Fail-fast validation (exit 2, clear message):
1. `--prod-mode --worker` (dev-only, above).
2. Worker port == router port.
3. Worker port == the backend's hardcoded port (llama-cpp 8080, ollama 11434, lmstudio 1234, vllm 8000, custom 8000).
4. Existing worker checks carried over: `--backend custom` behavior and `--model-path` requirements unchanged from today's worker.

## 3. Code layout

- `cmd/infermesh/` (new, single package):
  - `main.go` — dispatch: `os.Args[1]` → per-subcommand FlagSet (`ExitOnError`) → `runRouter` / `runWorker`; usage/help.
  - `router.go` — `runRouter(...)`: today's `cmd/router/main.go` behavior (registry, discovery listener, router server, signal handling) plus optional in-process worker start when `--worker` is set.
  - `worker.go` — `runWorker(...)`: today's `cmd/worker/main.go` behavior, with the backend factory and capabilities print delegated to `pkg/worker`.
- `cmd/router/` and `cmd/worker/` — **deleted outright, no shims.** (Rejected: thin wrapper kept for one release — two entry points to lint/test, and the old `infermesh-router`/`infermesh-worker` names keep circulating in docs.)
- Shared logic moves into `pkg/worker/`:
  - `run.go` (new): `Handle` (worker runtime: server + register loop + cancel), `RunWorker(ctx, cfg)`, `NewBackendFromName(backend, modelPath)` (from `createBackend` in `cmd/worker/main.go`), `RouterBaseFromListenAddr(addr string) (string, error)`, `LocalIP()`, `Hostname()` (moved from cmd/worker helpers).
  - capabilities print: `PrintCapabilities(io.Writer)` — from `capabilitiesCmd()` in `cmd/worker/capabilities.go`.
- Unchanged: `pkg/router/server.go`, `pkg/registry`, `pkg/discovery`, `pkg/scheduler`, `pkg/security`, `pkg/protocol`, `pkg/capabilities`, `pkg/platform`; `pkg/worker/server.go` and `pkg/worker/register.go` keep their signatures; `pkg/worker/backend_integration.go` keeps its constructors.
- Test placement: unit tests live inside `pkg/worker` and `cmd/infermesh` (per repo convention: unit tests inside the package, table-driven).

## 4. Build and release

- Makefile:
  - `build`: `go build -o bin/infermesh ./cmd/infermesh`.
  - `build-static`: `CGO_ENABLED=0` cross builds → `bin/infermesh-linux-amd64` + `bin/infermesh-darwin-arm64` (2 files, not 4).
  - `clean`, `lint`, `tidy`, `test*`, `coverage` unchanged.
- CI (`.github/workflows/ci.yml`): **no changes required** — it runs `make build`, `go vet ./...`, `go test ./... -v -cover -race`, golangci-lint v2.13.2 on Go 1.27. Nothing to pin or regress.
- Docs/scripts to update in the rename commit: `README.md`, `AGENTS.md` (CLI flags + build sections), `test.sh`, `hack/test_platforms.sh`, `TEST_SUMMARY.md` references.
- No new module files, no DB, no new dependencies.
- Root-level `router`/`worker` binaries: already untracked since `a720766`; `make build` will no longer produce them. Ensure they are never committed.

## 5. Test plan

Unit — `pkg/worker`:
- Table-driven `RouterBaseFromListenAddr`: `:8080`, `0.0.0.0:8080`, `localhost:8080`, `127.0.0.1:9000`, `myhost:8080` → expected `http://...` values; invalid inputs (`"noport"`, malformed brackets) → error.
- Combined-mode collision validation (worker port vs router port; worker port vs backend port) via a `validateCombinedFlags` helper.
- `NewBackendFromName` dispatch table (each name → constructor; unknown name → error).
- `PrintCapabilities` writes to a buffer without blocking.

Unit — `cmd/infermesh`:
- `parseRouterFlags(args []string)` / `parseWorkerFlags(args []string)` against the exact flag inventory: known flags accepted, unknown rejected, defaults correct.

E2E — `tests/e2e/combined_test.go` (modeled on `tests/e2e/discovery_test.go`; skip if binary absent, same pattern):
1. Build `bin/infermesh` once.
2. Start `bin/infermesh router --dev-mode --addr 127.0.0.1:8082 --worker --port 8085 --backend lmstudio --model-path /tmp/fake-model` (no real backend needed for registration).
3. Wait ≤ ~10s for `/v1/workers` to list one registered worker on port 8085.
4. Assert `/v1/models` and `/v1/workers` respond (router has no `/health` endpoint; do not add one in this issue).
5. Send SIGTERM → exit 0 within timeout.
6. Negative: `router --dev-mode --worker --addr 127.0.0.1:8082 --port 8082` → non-zero exit, message mentions the port collision.
7. Negative: `router --prod-mode --worker` → non-zero exit.

Coverage: ≥ 80% on `pkg/` verified with `make coverage`. A chat-completion round-trip against a fake backend (`httptest` + `--backend custom`) is documented as a manual smoke, not CI, to keep e2e hermetic.

Manual smoke (must pass before done):
```
make build
./bin/infermesh router --dev-mode --worker --backend lmstudio --model-path /path/to/model
# second terminal:
curl -s localhost:8080/v1/workers      # one worker, port 8081
curl -s localhost:8080/v1/models      # model listed
curl -s localhost:8080/v1/chat/completions -d '{"model":"...","messages":[...]}'  # works if LM Studio is running
Ctrl-C                                # clean exit 0, no dangling processes
```

## 6. Risks and edge cases

- **llama-cpp backend hardcodes `http://localhost:8080`** — the same as the router default `:8080`. In combined mode this collides only if the user sets `--port 8080`; the fail-fast check covers it. Not fixed with a `--backend-url` flag (scope creep for MVP).
- **One worker per process = one GPU.** Known MVP limitation; users who need two GPUs run `infermesh worker` as a separate process. Document it.
- **mDNS vs HTTP registration:** in combined mode the worker registers via HTTP `/v1/dev/register` (derived from `--addr`). The router's mDNS listener keeps running and simply sees no mDNS announcements for the embedded worker — identical to today's `test.sh` dev setup, so behavior is unchanged.
- **Readiness race:** register loop retries every 10s, so the worker self-heals if it starts before the router is fully up; the `/v1/workers` poll is UX polish, not a correctness requirement.
- **Shutdown ordering:** stop worker before router so de-registration lands before the listener closes; suppress `http.ErrServerClosed` noise; stop the registry after the listener closes.
- **mTLS combined mode:** intentionally unsupported (dev-only). Future prod combined mode would need a separate client cert per role — explicitly out of scope.
- **`--capabilities` on the router subcommand:** not a flag there; users run `infermesh worker --capabilities`.
- **Windows:** out of scope per MVP; `build-static` only ships linux-amd64 + darwin-arm64.
- **golangci-lint v2.13.2:** new files in `cmd/infermesh` and `pkg/worker` must pass lint (context usage, error wrapping) — part of the definition of done.
- **Root binaries:** never commit `bin/*` or root-level `router`/`worker`; `bin/` stays gitignored.

## 7. Phased implementation steps

**Phase 1 — extraction (behavior-neutral, tests green):**
1. Create `pkg/worker/run.go`: `Handle`, `RunWorker`, `NewBackendFromName`, `RouterBaseFromListenAddr`, `LocalIP`, `Hostname`; `PrintCapabilities`; move logic from `cmd/worker/main.go` + `cmd/worker/capabilities.go`.
2. Table-driven unit tests for the above in `pkg/worker`.
3. Reimplement `cmd/worker/main.go` as a thin wrapper over `pkg/worker` (identical behavior). Verify `go test ./... -race` green, coverage unchanged or better.

**Phase 2 — unified binary + rename:**
4. Add `cmd/infermesh/{main.go,router.go,worker.go}`; dispatch on `os.Args[1]` with per-subcommand FlagSets; `parseRouterFlags`/`parseWorkerFlags` extractable and unit-tested.
5. Add `--worker` + `--port/--backend/--model-path/--enable-health-checks` to the router subcommand; implement combined startup (router up → poll → worker up) and fail-fast validation.
6. Delete `cmd/router/` and `cmd/worker/`.
7. Update Makefile (`build`, `build-static` → `bin/infermesh` + 2 cross files), `test.sh`, `hack/test_platforms.sh`, `README.md`, `AGENTS.md`, `TEST_SUMMARY.md`.

**Phase 3 — tests + verification:**
8. Add `tests/e2e/combined_test.go` (positive registration, SIGTERM exit 0, port-collision negative, prod+worker negative).
9. Run `make lint`, `go vet ./...`, `go test ./... -v -cover -race`; verify `pkg/` coverage ≥ 80% via `make coverage`.
10. Run the manual smoke commands from §5 on a real machine with LM Studio.

**Phase 4 — polish:**
11. Suppress `http.ErrServerClosed` log noise; add `role` log attribute; final lint pass; check `docs/contracts/` for binary-name wording updates.

---

## Definition of done

- `make build` → only `bin/infermesh`; `make build-static` → `bin/infermesh-linux-amd64`, `bin/infermesh-darwin-arm64`.
- `go vet ./...`, golangci-lint v2.13.2, and `go test ./... -race` all green; `pkg/` coverage ≥ 80%.
- `infermesh router --dev-mode --worker` registers a worker against itself (verified via `/v1/workers`).
- Fail-fast cases exit non-zero with a clear message (`--prod-mode --worker`, port collisions).
- SIGTERM → exit 0, no dangling worker or backend processes.
- No new dependencies; CI config unchanged; `bin/` gitignored; no root-level binaries committed.
