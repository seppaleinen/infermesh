# InferMesh Agent Guidance

Go 1.27+ mono-repo: headless `router`, `worker`, and `relay` binaries turn heterogeneous machines with spare GPU capacity into a dynamically discovered, OpenAI-compatible inference pool ("Airbnb for spare inference capacity"), with the visual Wails desktop app (`app/`) as the new MVP north star — the pool plus a native manager window/tray. Not Kubernetes, not LiteLLM, not distributed inference.

## Repo gotchas (read before building)

- **`go.mod`/`go.sum` are tracked since `a720766`.** If you hit a missing module definition, restore `go.mod`: `module github.com/seppaleinen/infermesh`, `go 1.27`, deps only `github.com/hashicorp/mdns` + `gopkg.in/yaml.v3`, then `go mod tidy`.
- **Stale ~11 MB binaries `router` and `worker` were removed from tracking in `a720766`.** Real build output is `bin/` (gitignored). Don't commit root-level binaries.
- **`app/` is a separate Go module** with `replace github.com/seppaleinen/infermesh => ../` in `app/go.mod`. Root CI/lint/test do NOT descend into it; run `cd app && go test ./...` separately. Wails is pinned to `v3.0.0-beta.24` (`wails3` CLI must be installed at that exact version).
- **CI pins Go 1.27 and golangci-lint v2** (`golangci-lint-action@v9`, `version: v2.13.2`) — keep these on current stable, don't regress to old pins.
- **mDNS is flaky on some networks/VMs.** The worker's `--router http://127.0.0.1:8080` flag (dev mode only) registers over plain HTTP and skips mDNS entirely — this is the reliable way to run the full loop on one machine.

## Build & run

```bash
make build            # -> bin/infermesh-router + bin/infermesh-worker (from ./cmd/router, ./cmd/worker)
make build-app        # -> bin/app/infermesh-app (host-OS native desktop app; requires `wails3` on PATH + Node for the frontend bundle; CGO required; NOT part of `make build-static`)
# app/ note: frontend/dist is gitignored, but the tracked app/frontend/dist/.gitkeep placeholder keeps `go test ./...` (and `//go:embed all:frontend/dist`) compiling on a fresh clone; the real wails3 build regenerates dist before compiling, and `build:frontend` in app/build/Taskfile.yml restores the .gitkeep after vite's emptyOutDir wipes it.
make build-static     # cross-platform: 15 files (5 OS/arch x 3 binaries), CGO_ENABLED=0
make lint             # golangci-lint run
make tidy             # go mod tidy
make clean            # removes bin/, cover/
```

Run (defaults to **dev mode** if neither `--dev-mode` nor `--prod-mode` is passed):

```bash
./bin/infermesh-router --dev-mode                    # listens on :8080 (default --addr, hardcoded in router)
./bin/infermesh-worker --dev-mode --router http://127.0.0.1:8080 \
  --backend lmstudio --model-path /path/to/model     # default port 8081
./test.sh                                             # tmux one-machine harness: router + LM Studio worker (gpt-oss-20b) + curl
```

CLI flags (verified in `cmd/router/`, `cmd/worker/`):
- **Router**: `--dev-mode`, `--prod-mode`, `--mtls-cert`, `--mtls-key`, `--cert-dir`, `--api-key`, `--addr` (default `:8080`), `--relay-url` (relay connectivity, dev mode only).
- **Worker**: `--port` (default 8081), `--backend` (llama-cpp | ollama | lmstudio | vllm | custom), `--model-path`, `--router` (HTTP registration, dev-mode only), `--relay-url` (relay connectivity, dev mode only), `--capabilities` (print and exit), `--enable-health-checks` (default on), plus the mTLS flags. Prod mode **requires** `--model-path`.
- Backend endpoints are hardcoded in `pkg/worker/backend_integration.go`: llama-cpp `localhost:8080` (**collides with the router port!**), ollama `localhost:11434`, lmstudio `127.0.0.1:1234`, vllm `localhost:8000`.

## Testing

```bash
make test             # unit: go test ./pkg/...
make test-integration # go test ./tests/integration/...
make test-e2e         # go test ./tests/e2e/...
make test-security    # go test ./pkg/security/...
make coverage         # go tool funccover (requires Go 1.27+)
go test ./pkg/discovery/...   # single package (Makefile has NO PKG= variable)
./hack/test_platforms.sh [--build|--cross|--all]
```

- Unit tests live **inside `pkg/*`** (table-driven); `tests/unit/` and `tests/integration/` are currently empty dirs.
- What actually exists today: `tests/e2e/discovery_test.go`, `tests/e2e/combined_test.go`, `tests/integration_security/security_test.go`.
- Coverage target: **80% on `pkg/`**.
- mDNS timing contract: service type `_infermesh-worker._tcp.local.`, heartbeat TTL 30s, unavailable at 60s, removed at 120s (see `docs/contracts/discovery-v1.md`).

## Layout (trust `git ls-files`, not the README/CONTEXT trees)

The README and CONTEXT.md directory trees are stale (`internal/`, `scripts/`, `pkg/contracts` don't exist). Actual layout:

```
cmd/{router,worker,relay}     # binary entry points: router, worker, relay
pkg/protocol                  # shared types (WorkerInfo, Capabilities, events)
pkg/discovery                 # mDNS listener (router) / announcer (worker), pluggable backends
pkg/registry                  # in-memory worker registry, heartbeats
pkg/scheduler                 # capability matching & deterministic scoring
pkg/router                    # router HTTP server + OpenAI-compatible API
pkg/worker                    # worker HTTP server + backend adapters
pkg/security                  # mTLS, cert generation, API-key middleware
pkg/capabilities              # GPU/VRAM detection, model parsing
pkg/platform                  # platform helpers (macOS/Linux)
pkg/wsutil                    # WebSocket transport helpers (router hub / worker client)
app/                          # Wails v3 desktop app (separate Go module, replace => ../)
  main.go                     # Wails application + SystemTray (Open/Quit); embeds assets/trayTemplate.png
  main_test.go                # GUI-free tests (embedded tray icon)
  assets/                     # tray template icon (trayTemplate.png, regenerated by tools/gen-trayicon)
  tools/gen-trayicon/         # stdlib-only Go generator for assets/trayTemplate.png
  go.mod / go.sum             # pinned wails v3.0.0-beta.24
  Taskfile.yml                # Wails task pipeline (embedded go-task; no standalone task needed)
  build/                      # platform assets (darwin/linux/windows/…)
  frontend/                   # Vue 3 + Vite + TS; bundled into binary at build time
tests/{integration,integration_security,e2e}
hack/test_platforms.sh
docs/contracts/               # design contracts (per-issue)
```

Dependencies are intentionally minimal: stdlib + `hashicorp/mdns` + `yaml.v3`. Keep it that way.

## Design constraints

- mDNS discovery first, pluggable for a centralized registry later
- Single router (no HA), in-memory registry (no DB), deterministic scoring
- One GPU per worker; manual model declaration in MVP
- Dev mode = no auth, localhost; prod mode = mTLS with self-signed certs + API keys
- Platforms: headless binaries ship Linux/macOS/Windows (`make build-static` includes windows-amd64); the desktop app builds natively on macOS (CGO/cocoa, primary verification) and Linux (webkit2gtk build deps — docs/CI note only, no Linux build attempted on this mac); Windows desktop build deferred (webview2 note only).

**Out of scope (MVP)**: Kubernetes/Docker, distributed (model-parallel) inference, automatic model discovery/downloading, multi-router HA, WAN workers, CPU-only workers, LiteLLM, enterprise auth beyond mTLS.

## Phase tracking (GitHub issues)

| Phase | Issues | Focus |
|-------|--------|-------|
| 0 — Foundation | #11, #12 | scaffolding, test infrastructure |
| 1 — Discovery | #3, #4, #5, #6 | mDNS, worker protocol, capabilities, registry |
| 2 — Routing | #7, #8 | scheduling, OpenAI API |
| 3 — Hardening | #9, #10 | security (mTLS), platform support |
| 4 — Validation | — | E2E, dynamic join/leave, failure detection |

## References

- `README.md` — project overview, MVP definition (directory tree is stale)
- `CONTEXT.md` — architecture summary
- `docs/contracts/discovery-v1.md` — mDNS discovery contract
- `TEST_SUMMARY.md`, `DYNAMIC_MODEL_MANAGEMENT_PLAN.md` — work notes
- [GitHub Issues](https://github.com/seppaleinen/infermesh/issues)

## Release Workflow

Releases are automated via GitHub Actions and triggered upon successful completion of the `ci.yml` workflow on the `main` branch.

The workflow performs the following:
1. Determines the next semantic version by incrementing the patch version of the latest git tag.
2. Creates and pushes an annotated tag.
3. Runs `make build-static` to generate static binaries for 5 platforms (Linux amd64/arm64, Darwin amd64/arm64, Windows amd64).
4. Packages binaries into `.tar.gz` archives.
5. Generates `SHA256SUMS.txt` for verification.
6. Creates a GitHub Release with the generated tarballs and checksum file.

### New endpoint: `/meta/models/popular` (Issue #31)

Returns a sorted list of models available across the pool with per-model
worker count and call count over a rolling 10-minute window.

- Endpoint: `GET /meta/models/popular`
- Response: `PopularModelsResponse{Models: []PopularModelInfo{Model, WorkerCount, CallCount}}`
- Sorted by `worker_count` descending
- Worker count: number of workers advertising the model (regardless of `Loaded` state)
- Call count: inferences dispatched in last 10 minutes
- Counter implementation: `pkg/router/call_counter.go` (in-memory, stdlib only)

---

*Last updated: 2026-09-21*
