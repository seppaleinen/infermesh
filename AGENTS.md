# InferMesh Agent Guidance

Go 1.25+ mono-repo: an inference **router** + **worker** binaries that turn heterogeneous machines with spare GPU capacity into a dynamically discovered, OpenAI-compatible inference pool ("Airbnb for spare inference capacity"). Not Kubernetes, not LiteLLM, not distributed inference.

## Repo gotchas (read before building)

- **`go.mod`/`go.sum` are gitignored and NOT tracked.** A fresh clone has no module definition and `go build` fails. If you hit this, restore `go.mod`: `module github.com/seppaleinen/infermesh`, `go 1.25.0`, deps only `github.com/hashicorp/mdns` + `gopkg.in/yaml.v3`.
- **Stale ~11 MB binaries `router` and `worker` are tracked at the repo root** (leftovers of local builds). Real build output is `bin/` (gitignored). Don't commit root-level binaries.
- **CI pins Go 1.23 but `go.mod` requires 1.25.0** — `.github/workflows/ci.yml` is inconsistent (its `go build ./../...` path is also broken). Local toolchain is newer (1.27+).
- **mDNS is flaky on some networks/VMs.** The worker's `--router http://127.0.0.1:8080` flag (dev mode only) registers over plain HTTP and skips mDNS entirely — this is the reliable way to run the full loop on one machine.

## Build & run

```bash
make build            # -> bin/infermesh-router, bin/infermesh-worker (from ./cmd/router, ./cmd/worker)
make build-static     # cross-platform: linux-amd64 + darwin-arm64, CGO_ENABLED=0
make lint             # golangci-lint run
make tidy             # go mod tidy
make clean            # removes bin/, cover/
```

Run (defaults to **dev mode** if neither `--dev-mode` nor `--prod-mode` is passed):

```bash
./bin/infermesh-router --dev-mode                    # listens on :8080 (hardcoded, no port flag)
./bin/infermesh-worker --dev-mode --router http://127.0.0.1:8080 \
  --backend lmstudio --model-path /path/to/model     # default port 8081
./test.sh                                             # tmux one-machine harness: router + LM Studio worker (gpt-oss-20b) + curl
```

CLI flags (verified in `cmd/*/main.go`):
- **Router**: `--dev-mode`, `--prod-mode`, `--mtls-cert`, `--mtls-key`, `--cert-dir`, `--api-key`
- **Worker**: `--port` (default 8081), `--backend` (llama-cpp | ollama | lmstudio | vllm | custom), `--model-path`, `--router` (HTTP registration, dev-mode only), `--capabilities` (print and exit), `--enable-health-checks` (default on), plus the mTLS flags. Prod mode **requires** `--model-path`.
- Backend endpoints are hardcoded in `cmd/worker/main.go`: llama-cpp `localhost:8080` (**collides with the router port!**), ollama `localhost:11434`, lmstudio `127.0.0.1:1234`, vllm `localhost:8000`.

## Testing

```bash
make test             # unit: go test ./pkg/...
make test-integration # go test ./tests/integration/...
make test-e2e         # go test ./tests/e2e/...
make test-security    # go test ./pkg/security/...
make coverage         # go tool funccover (requires Go 1.25+)
go test ./pkg/discovery/...   # single package (Makefile has NO PKG= variable)
./hack/test_platforms.sh [--build|--cross|--all]
```

- Unit tests live **inside `pkg/*`** (table-driven); `tests/unit/` and `tests/integration/` are currently empty dirs.
- What actually exists today: `tests/e2e/discovery_test.go`, `tests/integration_security/security_test.go`.
- Coverage target: **80% on `pkg/`**.
- mDNS timing contract: service type `_infermesh-worker._tcp.local.`, heartbeat TTL 30s, unavailable at 60s, removed at 120s (see `docs/contracts/discovery-v1.md`).

## Layout (trust `git ls-files`, not the README/CONTEXT trees)

The README and CONTEXT.md directory trees are stale (`internal/`, `scripts/`, `pkg/contracts` don't exist). Actual layout:

```
cmd/router, cmd/worker        # binary entry points
pkg/protocol                  # shared types (WorkerInfo, Capabilities, events)
pkg/discovery                 # mDNS listener (router) / announcer (worker), pluggable backends
pkg/registry                  # in-memory worker registry, heartbeats
pkg/scheduler                 # capability matching & deterministic scoring
pkg/router                    # router HTTP server + OpenAI-compatible API
pkg/worker                    # worker HTTP server + backend adapters
pkg/security                  # mTLS, cert generation, API-key middleware
pkg/capabilities              # GPU/VRAM detection, model parsing
pkg/platform                  # platform helpers (macOS/Linux)
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
- Platforms: Linux + macOS (Apple Silicon); Windows out of scope

**Out of scope (MVP)**: Kubernetes/Docker, distributed (model-parallel) inference, automatic model discovery/downloading, Web UI, multi-router HA, WAN workers, CPU-only workers, LiteLLM, enterprise auth beyond mTLS.

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

---

*Last updated: 2026-09-09*
