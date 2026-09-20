# InferMesh — Distributed Inference Pool

Build a lightweight, self-hosted, open-source system that turns a collection of heterogeneous machines with spare GPU/inference capacity into a dynamically discovered, automatically scheduled inference pool.

**Think: "Airbnb for spare inference capacity on our network."**

Not: Kubernetes, LiteLLM, or distributed model inference.

## Quick Start

```bash
# Build the unified infermesh binary
make build

# Run router in dev mode (no auth, localhost)
./bin/infermesh router --dev-mode

# Run a worker (also dev mode, auto-discovers router via mDNS)
./bin/infermesh worker --dev-mode \
  --model-path /path/to/model.gguf \
  --backend llama-cpp

# Or run the router and an in-process worker in one command (dev mode only)
./bin/infermesh router --dev-mode --worker \
  --model-path /path/to/model.gguf \
  --backend llama-cpp

# Use as an OpenAI-compatible endpoint
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=unused
```

## Firewall / Outbound-Only Relay

If your worker machine sits behind a firewall that blocks inbound connections (common on macOS Sequoia+, corporate LANs, or restricted VMs), use the **outbound-only relay** architecture. Both the router and every worker dial OUTBOUND to a relay broker on a permissive host — no machine with an unsigned binary ever accepts an inbound connection.

Build the relay binary:

```bash
make relay
```

### Three-machine setup

**On the GPU / permissive host** (e.g. `192.168.1.216`):

```bash
# Terminal 1 — relay broker (listens on :8090)
./bin/infermesh-relay --listen :8090 --dev-mode

# Terminal 2 — router (connects outbound to the relay)
./bin/infermesh router --dev-mode --addr :8080 \
  --relay-url ws://192.168.1.216:8090
```

**On the worker machine** (firewalled, can only dial outbound):

```bash
# Terminal 3 — worker (connects outbound to the relay)
./bin/infermesh worker --dev-mode \
  --relay-url ws://192.168.1.216:8090 \
  --backend lmstudio
```

After all three are up, test inference:

```bash
curl -X POST http://192.168.1.216:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-oss-20b",
    "messages": [{"role": "user", "content": "Hello via relay"}],
    "max_tokens": 16
  }'
```

The relay forwards worker registration messages to the router and proxies inference requests from the router back to the worker — all over outbound WebSocket connections.

## Architecture

```
┌─────────────────────┐
│   AI Client         │
│ OpenAI API client   │
│ Agent / IDE         │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│   Inference Router  │
│ Discovery (mDNS)    │
│ Registry (in-memory)│
│ Scheduler (scoring) │
│ Health Monitoring   │
│ OpenAI API          │
└──────────┬──────────┘
           │
  ┌────────┼────────┐
  │        │        │
  ▼        ▼        ▼
Worker A  Worker B  Worker C
 llama.cpp ollama   LM Studio
```

### Key Principles

1. **Zero-config discovery** — Workers auto-discover the router via mDNS. No manual configuration.
2. **Heterogeneous hardware** — Mix GPUs, OS, and inference engines in the same pool.
3. **Intelligent routing** — Router scores eligible workers and picks the best one.
4. **Direct worker communication** — Router routes; client talks directly to worker when possible.
5. **Minimal dependencies** — Single static binaries, no Kubernetes/Docker required.

## Technology Stack

| Layer | Technology |
|-------|-----------|
| Language | Go (>= 1.22) |
| Discovery | mDNS / DNS-SD |
| Communication | HTTP/REST (gRPC future) |
| Security | mTLS with self-signed certs |
| Authentication | Dev: none / Prod: mTLS + API keys |
| API | OpenAI-compatible |
| Platforms | Linux (Ubuntu/Debian), macOS (Apple Silicon M1/M2/M4) |

## Worker Protocol

Workers expose a small, standardized HTTP interface:

```http
GET  /health                    # Worker health status
GET  /capabilities              # GPU, VRAM, engines, models, quantization
GET  /metrics                  # Runtime: GPU util, VRAM used, queue depth
POST /v1/chat/completions      # OpenAI-compatible chat (streaming)
POST /v1/completions           # OpenAI-compatible completions (streaming)
GET  /v1/models                # Available model list
```

### Supported Backends

| Backend | Status |
|---------|--------|
| llama.cpp / llama-server | MVP |
| Ollama | MVP |
| LM Studio | MVP |
| vLLM | Future |
| TensorRT-LLM | Future |

## Directory Structure

```
infermesh/
├── cmd/
│   └── infermesh/                 # Unified binary entry point (router + worker subcommands)
├── pkg/
│   ├── discovery/                 # mDNS service discovery
│   │   └── discovery_test.go      # Unit tests
│   ├── registry/                  # Worker registry & heartbeats
│   ├── scheduler/                 # Capability matching & scoring
│   ├── protocol/                  # Shared types (HTTP request/response)
│   ├── capabilities/              # GPU/VRAM detection, model parsing
│   ├── router/                    # Router HTTP server & OpenAI API
│   ├── worker/                    # Worker HTTP server & backend adapters
│   ├── security/                  # mTLS, TLS cert generation, auth
│   └── platform/                  # GPU detection, platform helpers (macOS/Linux)
├── internal/                      # Private implementation details
├── tests/
│   ├── integration/               # Multi-component integration tests
│   └── e2e/                       # End-to-end flow tests
├── scripts/                       # Helper scripts (certs, test runners)
├── Makefile
├── go.mod
├── go.sum
├── CONTEXT.md                     # Full project context
├── AGENTS.md                      # Developer guidelines
├── README.md
└── LICENSE
```

## Development Workflow

The project uses an **agent pipeline** for development:

1. **dev-team-lead** → assigns work to architect → engineer → test → reviewer
2. **devops-team-lead** → handles infrastructure, cluster, and deployment pipelines
3. Each issue is tracked on [GitHub Issues](https://github.com/seppaleinen/infermesh/issues)

### Build Commands

```bash
make build        # Build the unified infermesh binary
make test         # Run unit tests
make test-integration  # Run integration tests
make test-e2e     # Run end-to-end tests
make lint         # Run linter (golangci-lint)
make tidy         # go mod tidy
```

### Testing Strategy

| Level | Location | Description |
|-------|----------|-------------|
| Unit | `pkg/*/` | Method-level, table-driven, complex logic |
| Integration | `tests/integration/` | Router↔worker interaction, mDNS discovery, heartbeat behavior |
| E2E | `tests/e2e/` | Full flow: start router, start workers, send request, streaming response |

**Coverage target**: 80% on `pkg/` packages.

## Current Issues

| Phase | Issues | Description |
|-------|--------|-------------|
| Phase 0 — Foundation | [#11](https://github.com/seppaleinen/infermesh/issues/11), [#12](https://github.com/seppaleinen/infermesh/issues/12) | Project scaffolding, testing infrastructure |
| Phase 1 — Core Discovery & Visibility | [#3](https://github.com/seppaleinen/infermesh/issues/3), [#4](https://github.com/seppaleinen/infermesh/issues/4), [#5](https://github.com/seppaleinen/infermesh/issues/5), [#6](https://github.com/seppaleinen/infermesh/issues/6) | mDNS discovery, worker protocol, capability reporting, worker registry |
| Phase 2 — Routing & API | [#7](https://github.com/seppaleinen/infermesh/issues/7), [#8](https://github.com/seppaleinen/infermesh/issues/8) | Scheduling, OpenAI-compatible API |
| Phase 3 — Hardening | [#9](https://github.com/seppaleinen/infermesh/issues/9), [#10](https://github.com/seppaleinen/infermesh/issues/10) | Security (mTLS), platform support |
| Phase 4 — MVP Validation | — | End-to-end testing, dynamic join/leave, failure detection |

## MVP Definition

The first working version should be able to:

1. Start a router.
2. Start workers on 2–3 different machines.
3. Workers automatically discover the router/peers via mDNS.
4. Workers report GPU, VRAM, model and load information.
5. Router maintains a live worker registry.
6. Client sends an OpenAI-compatible request.
7. Router determines which workers are compatible.
8. Router selects the best available worker (deterministic scoring).
9. Request is sent to that worker.
10. Streaming response is returned to the client.
11. If a worker becomes unavailable, it is automatically removed from scheduling.
12. When it returns, it automatically rejoins.

## Security

- **Dev mode**: No authentication, plain HTTP on localhost. Works out of the box.
- **Production mode**: mTLS with self-signed certificates. Worker identity verification, audit logging, request-level access control.
- **Key requirements**: Authentication, worker identity, TLS/mTLS, authorization, request-level access control, worker allow/deny policies, audit logging.

## macOS App Bundles (Firewall / Local Network)

On macOS Sequoia+ with the Application Firewall enabled, unsigned CLI binaries get silently blocked for inbound LAN connections. Bundling the binaries as `.app` bundles gives them a real identity and triggers the one-time "Allow incoming connections?" GUI prompt — no admin required.

Build both bundles:

```bash
make app
```

Outputs `dist/InferMesh Router.app` and `dist/InferMesh Worker.app`.

Run the bundled worker (same flags as the bare binary):

```bash
./dist/InferMesh\ Worker.app/Contents/MacOS/infermesh-worker --dev-mode --backend custom --router http://127.0.0.1:8080
```

Enable firewall / local network access:

```bash
bash packaging/macos/enable_incoming.sh
```

This uses `socketfilterfw` when passwordless sudo is available; otherwise it prints GUI instructions (launch each bundle once, click Allow, then check System Settings → Privacy & Security → Local Network).

The bundles can be copied to `~/Applications` or `/Applications` and run from there.

**Rebuild caveat**: the ad-hoc signature changes on every `make app` build, so the firewall prompt may re-fire after a rebuild. A self-signed Keychain certificate with `codesign --sign "<cert-name>"` is the stable alternative (documented only — see `packaging/macos/` for the scripts and plist template).

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run checks: `make lint && make test` (plus `make test-integration` / `make test-e2e` where applicable)
5. Install the push gate once: `make install-hooks` — runs lint before every push
6. Submit a pull request

See [AGENTS.md](AGENTS.md) for developer guidelines and the agent pipeline documentation.

## License

MIT — see [LICENSE](LICENSE).