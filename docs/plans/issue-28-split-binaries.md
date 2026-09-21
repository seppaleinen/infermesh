# Issue #28 — Split Unified Binary into Separate Router/Worker Binaries

Status: **implemented** (see commit on `origin/main`). The plan below is kept
for historical reference; §10 records the one scope deviation from the original
plan (`--backend-url` was added during implementation).

---

## Constraints (from AGENTS.md)

- Go 1.27+, module `github.com/seppaleinen/infermesh`.
- Dependencies: stdlib + `github.com/hashicorp/mdns` + `gopkg.in/yaml.v3` + `golang.org/x/net/websocket` — **no new dependencies**.
- CI must not regress: Go 1.27 pin, `golangci/golangci-lint-action@v9` at `v2.13.2`, `go vet ./...`, `make build`, `go test ./... -v -cover -race`.
- Coverage target: **80% on `pkg/`** (`make coverage`, funccover).
- Build output goes to gitignored `bin/`; root-level binaries must never be committed.

---

## Current state (what we're splitting)

```
cmd/infermesh/
  main.go          # dispatch on os.Args[1] → router.go/worker.go
  router.go        # runRouter() — router + optional --worker (combined mode)
  worker.go        # runWorker() — worker
cmd/relay/main.go  # relay broker (already separate)
```

The `--worker` flag on the router subcommand runs an **in-process worker** registered against its own router (combined mode, dev-only). This was a convenience for one-machine dev with the unified binary.

---

## Decision: drop `--worker` combined mode

With separate binaries, the combined mode (`router --worker`) is redundant: users just run two binaries (`infermesh-router` + `infermesh-worker`). Keeping it on the router binary would reintroduce the coupling we're removing.

**Consequences:**
- `test.sh` and `tests/e2e/combined_test.go` must be updated (they test the combined mode today).
- The dev one-machine flow becomes two terminals (or `tmux split`), matching the relay flow already documented in README.
- No flags removed from the worker binary; `--relay-url` stays on both.

---

## 1. Code layout (after)

```
cmd/router/
  main.go          # former cmd/infermesh/router.go, minus --worker logic
cmd/worker/
  main.go          # former cmd/infermesh/worker.go
cmd/relay/
  main.go          # unchanged
cmd/infermesh/     # DELETED (no shim, no launcher)
```

### What moves / changes

| From | To | Notes |
|------|----|-------|
| `cmd/infermesh/router.go` | `cmd/router/main.go` | Remove `--worker` flag, `Worker` bool, `Port`/`Backend`/`ModelPath`/`EnableHealthChecks` passthrough fields; remove `runRouter`'s combined-mode validation, startup, shutdown. Keep `RelayURL`, `SetRelayURL`, `RunRelay`. |
| `cmd/infermesh/worker.go` | `cmd/worker/main.go` | Rename `runWorker` → `main`; `parseWorkerFlags` → `parseFlags`; keep all flags including `--capabilities`, `--relay-url`. |
| `cmd/infermesh/main.go` | **deleted** | No replacement. |

### Shared logic (no change)

- `pkg/router/server.go` — unchanged (hub, relay hub, server logic)
- `pkg/worker/run.go` — unchanged (`RunConfig`, `RunWorker`)
- `pkg/worker/server.go`, `pkg/worker/backend_integration.go`, `pkg/worker/register.go` — unchanged
- `pkg/protocol/ws.go`, `pkg/router/ws_hub.go`, `pkg/router/worker_client.go`, `pkg/worker/ws_client.go`, `pkg/wsutil` — unchanged

---

## 2. Build targets (Makefile)

### Before
```make
build:      go build -o bin/infermesh ./cmd/infermesh
relay:      go build -o bin/infermesh-relay ./cmd/relay
build-static: 2 cross files (infermesh-linux-amd64, infermesh-darwin-arm64)
```

### After
```make
build-router:   go build -o bin/infermesh-router ./cmd/router
build-worker:   go build -o bin/infermesh-worker ./cmd/worker
build:          build-router build-worker
relay:          go build -o bin/infermesh-relay ./cmd/relay

build-static:
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-router-linux-amd64 ./cmd/router
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-router-darwin-arm64 ./cmd/router
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-worker-linux-amd64 ./cmd/worker
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-worker-darwin-arm64 ./cmd/worker
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-relay-linux-amd64 ./cmd/relay
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-relay-darwin-arm64 ./cmd/relay
```

- `make build` → produces `bin/infermesh-router`, `bin/infermesh-worker` (NOT `bin/infermesh`)
- `make build-static` → 6 cross-platform files (router 2, worker 2, relay 2)
- `make relay` unchanged

---

## 3. Files to update (binary-name references)

| File | What to change |
|------|----------------|
| `README.md` | Quick Start commands; relay section (already uses `infermesh-router`/`infermesh-worker` ✓); any remaining `./bin/infermesh router` → `./bin/infermesh-router`, `./bin/infermesh worker` → `./bin/infermesh-worker` |
| `AGENTS.md` | Build commands (lines 15–16, 25–28); CLI flag table |
| `TEST_SUMMARY.md` | Manual smoke commands |
| `docs/contracts/discovery-v1.md` | Line 1023 build command |
| `test.sh` | Replace `./bin/infermesh router --dev-mode --worker ...` with two-process launch (router + worker in tmux panes) |
| `hack/test_platforms.sh` | Lines 109–110, 128–129: expect 6 cross files, check for `infermesh-router` and `infermesh-worker` |
| `tests/e2e/combined_test.go` | **Rewrite** as two-process test: start `infermesh-router` and `infermesh-worker` as separate processes, verify registration and endpoints. Keep fail-fast tests (port collision, prod-mode) as separate router-only tests. |
| `tests/e2e/discovery_test.go` | Line 49: `binPath := "../../bin/infermesh"` → use `infermesh-router` + `infermesh-worker` (two processes) |
| `tests/e2e/relay_test.go` | Lines 268–270: already expects `infermesh-router`/`infermesh-worker` ✓ — will pass once binaries exist |
| `packaging/macos/build_app.sh` | Lines 18, 24–25: already expects `infermesh-router`/`infermesh-worker` ✓ — will pass once binaries exist |
| `test-relay.sh` | Already uses `infermesh-router`/`infermesh-worker` ✓ — will pass once binaries exist |

---

## 4. Test plan

### Unit tests (existing, no change)
- `pkg/worker`, `pkg/router`, `pkg/discovery`, `pkg/registry`, `pkg/scheduler`, `pkg/security`, `pkg/protocol`, `pkg/wsutil`, `pkg/capabilities` — all pass.

### E2E tests (updated)
1. **`tests/e2e/router_worker_test.go`** (renamed from `combined_test.go`): start `infermesh-router` + `infermesh-worker` as separate processes; verify `/v1/workers` lists the worker, `/v1/models` responds, SIGTERM on both exits cleanly.
2. **`tests/e2e/router_test.go`** (new): router-only fail-fast tests (port collision logic moved from combined test; `--prod-mode --worker` gone so that test removed).
3. **`tests/e2e/relay_test.go`**: runs unskipped (binaries will exist).
4. **`tests/e2e/discovery_test.go`**: rewritten to use two processes.

### Manual smoke (must pass before done)
```bash
make build

# Terminal 1
./bin/infermesh-router --dev-mode --addr 127.0.0.1:8080

# Terminal 2
./bin/infermesh-worker --dev-mode --backend lmstudio --model-path /path/to/model --router http://127.0.0.1:8080

# Terminal 3 (client)
curl -s http://127.0.0.1:8080/v1/workers   # one worker listed
curl -s http://127.0.0.1:8080/v1/models    # model listed
curl -X POST http://127.0.0.1:8080/v1/chat/completions ...  # works if LM Studio running

# Relay smoke (from README)
./bin/infermesh-relay --listen :8090 --dev-mode
./bin/infermesh-router --dev-mode --addr :8080 --relay-url ws://127.0.0.1:8090
./bin/infermesh-worker --dev-mode --relay-url ws://127.0.0.1:8090 --backend lmstudio
```

---

## 5. Phased implementation steps

### Phase 1 — Extract router binary (behavior-neutral)
1. `mkdir -p cmd/router`; move `cmd/infermesh/router.go` → `cmd/router/main.go`; change `package main`; remove `--worker` combined mode logic (flags, validation, startup, shutdown); keep relay logic.
2. `mkdir -p cmd/worker`; move `cmd/infermesh/worker.go` → `cmd/worker/main.go`; change `package main`; rename `runWorker` → `main`, `parseWorkerFlags` → `parseFlags`, `registerWorkerFlags` → `registerFlags`.
3. Delete `cmd/infermesh/`.
4. Update Makefile per §2.
5. `go build ./cmd/router ./cmd/worker ./cmd/relay` — compiles.
6. `make build` → `bin/infermesh-router`, `bin/infermesh-worker`.
7. `go vet ./...`, `golangci-lint run`.

### Phase 2 — Update scripts & tests
8. Update `README.md`, `AGENTS.md`, `TEST_SUMMARY.md`, `docs/contracts/discovery-v1.md`.
9. Update `test.sh` → two-process tmux (router pane + worker pane).
10. Update `hack/test_platforms.sh` → 6 cross files, check for router/worker.
11. Rewrite `tests/e2e/combined_test.go` → `tests/e2e/router_worker_test.go` (two-process). Remove combined-mode fail-fast tests (port collision, prod-mode); add router-only port-collision test if router still has the logic (it doesn't — that was combined-mode only). Keep worker `--capabilities` test.
12. Rewrite `tests/e2e/discovery_test.go` → two-process (router + worker via mDNS).
13. `tests/e2e/relay_test.go` — no change needed (already expects separate binaries).

### Phase 3 — Verification
14. `make build && make relay` — all three binaries present.
15. `make test` — unit + e2e green.
16. `make lint` clean.
17. `make coverage` ≥ 80% on `pkg/`.
18. Run `test-relay.sh` — passes end-to-end.
19. Run manual smoke commands from §4.

---

## 6. Risks & edge cases

- **`--worker` flag removal**: was a dev convenience. Users who want one-machine dev run two terminals (or `test.sh` tmux). Document in README.
- **`test.sh` tmux layout**: currently starts router+worker in one pane, curl in another. After split: pane 0 = router, pane 1 = worker, pane 2 = curl (or keep 2 panes: router+worker in one, curl in other via `tmux split-window -h`). Keep it simple.
- **`combined_test.go` → `router_worker_test.go`**: the fail-fast cases (`--prod-mode --worker`, port collision) are combined-mode specific and can be dropped. The router no longer has a `--worker` flag. If we want a router port-collision test, it's a different scenario (two routers).
- **Coverage**: no new code, just moving; coverage should stay ≥ 80%.
- **Cross-platform builds**: 6 files instead of 4 (router 2, worker 2, relay 2). `hack/test_platforms.sh` validates all 6.
- **`pkg/worker/run.go`**: `RunConfig` has `RouterBase` and `RelayURL`. Both binaries pass the same struct; no change needed.

---

## 7. Definition of done

- `make build` → `bin/infermesh-router`, `bin/infermesh-worker` (no `bin/infermesh`)
- `make relay` → `bin/infermesh-relay`
- `make build-static` → 6 cross-platform files (router 2, worker 2, relay 2)
- `make test`, `make test-integration`, `make test-e2e` all pass (no skipped e2e due to missing binaries)
- `make lint` clean; `go vet ./...` clean
- `make coverage` ≥ 80% on `pkg/`
- `test-relay.sh` passes end-to-end
- No references to `./bin/infermesh router` or `./bin/infermesh worker` remain in the repo (except in `docs/plans/issue-23-combine-clis.md` as historical record)
- Manual smoke (two-process + relay three-process) works

---

## 8. Out of scope (explicit)

- Adding a unified launcher shim (`cmd/infermesh/main.go` dispatching to sub-binaries) — the issue says "prefer full removal".
- Changing any CLI flag names or behavior beyond removing `--worker` combined mode.
- Merging relay into router.
- Windows support.
- Production-mode combined mode (was never supported anyway).

---

## 9. Implementation deviation (post-plan)

During implementation, `tests/e2e/relay_test.go` and `test-relay.sh` were found
to reference a `--backend-url` worker flag that did not exist on the CLI
(`flag provided but not defined: -backend-url`). The flag is clearly intended
(the relay e2e test wires an in-process `httptest` mock backend through the
llama-cpp adapter, and `test-relay.sh` uses the same flag), so it was added
rather than left as a broken test:

- `pkg/worker/run.go`: `RunConfig.BackendURL` field; `NewBackendFromName` now
  takes a `backendURL` and uses it to override each adapter's default endpoint
  (llama-cpp `http://localhost:8080`, ollama `localhost:11434`, lmstudio
  `127.0.0.1:1234`, vllm `localhost:8000`, custom `localhost:8000`).
- `cmd/worker/main.go`: `--backend-url` flag bound to `WorkerFlags.BackendURL`.
- `pkg/worker/run_test.go`: `TestNewBackendFromName` extended with a
  `backendURL` column.
- `test-relay.sh`: worker args switched from `--router` to `--relay-url`
  (the relay scenario needs WebSocket registration, not HTTP).

This is a net-new flag, not a rename — it does not change the behavior of any
existing flag. The `--backend-url` flag is documented in `AGENTS.md`.