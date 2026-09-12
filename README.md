# InferMesh — Distributed Inference Pool

Build a lightweight, self-hosted, open-source system that turns a collection of heterogeneous machines with spare GPU/inference capacity into a dynamically discovered, automatically scheduled inference pool.

**Think: "Airbnb for spare inference capacity on our network."**

Not: Kubernetes, LiteLLM, or distributed model inference.

## Quick Start

```bash
# Build the router and worker binaries
make build

# Run router in dev mode (no auth, localhost)
./bin/infermesh-router --dev-mode

# Run worker (also dev mode, auto-discovers router via mDNS)
./bin/infermesh-worker --dev-mode \
  --model-path /path/to/model.gguf \
  --backend llama-cpp

# Use as an OpenAI-compatible endpoint
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=unused
```

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
│   ├── router/                    # Router binary entry point
│   └── worker/                    # Worker binary entry point
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
make build        # Build router and worker binaries
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

## macOS Firewall: no-admin workaround (SSH reverse tunnel)

On macOS Sequoia+ (26.6.1 confirmed), the Application Firewall silently blocks inbound LAN connections to unsigned or ad-hoc-signed binaries. There is **no** "Allow incoming connections?" prompt for apps that lack a Developer-ID certificate — the block is permanent and un-surfaced. Launching the app with `open` or running the bare binary does not surface a prompt.

### Recommended: SSH reverse tunnel (no admin required)

The tunnel carries **both directions** over SSH: the worker's callback listener is exposed on the router host's loopback (`-R`), and the worker's registration POST is forwarded to the router host's loopback (`-L`). The router therefore dials `127.0.0.1` and sees a loopback peer for registration — the firewall is never consulted, regardless of the router build.

```
router host (GPU node)          this machine (macOS, firewall-blocked)
  sshd listener 127.0.0.1:PORT     worker :PORT
  ssh -R PORT:127.0.0.1:PORT <────────────────
  router dials 127.0.0.1:PORT           |
  ssh -L ROUTER_PORT:127.0.0.1:ROUTER_PORT ────▶ registration POST via loopback
```

```bash
# Build first
make build

# Start worker with SSH tunnel (needs sshd + key access on router host)
./packaging/macos/worker_tunnel.sh \
    --router-host 192.168.1.216 \
    --backend lmstudio \
    --model-path /path/to/model
```

Requirements on the router host: sshd with `AllowTcpForwarding yes` (default on most distros) and SSH key access for your user.

See `packaging/macos/worker_tunnel.sh --help` for all flags.

### Alternative: admin path (socketfilterfw)

If you have admin / passwordless sudo, you can explicitly allow the app bundles through the firewall:

```bash
# Build the app bundles (optional — only useful with this path)
make app

# Allow through firewall (requires sudo)
bash packaging/macos/enable_incoming.sh
```

This uses `socketfilterfw` to add and unblock the inner Mach-O binary directly. No prompt is needed — it requires root. Re-run after each `make app` rebuild since the ad-hoc signature changes.

## Outbound-only Architecture

Workers connect **outbound** to the router via WebSocket — the router never dials workers directly. This eliminates the macOS Application Firewall and NAT traversal issues entirely.

### How it works

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
│ listens on :8080    │
│ (mTLS / API key)    │
└──────────┬──────────┘
            │  ▲
            │  │ worker dials ws://router:8080/v1/connect
            │  │ (outbound WebSocket, no inbound firewall)
            ▼  │
┌─────────────────────┐
│   Worker            │
│ connects outbound   │
└─────────────────────┘
```

- Workers dial the router over `wss://` (WebSocket over TLS) when mTLS is configured, or `ws://` in dev mode.
- The router serves the `/v1/connect` endpoint and accepts WebSocket upgrades.
- No firewall rules are needed on worker machines; workers initiate all connections.

### Configuration

```bash
# Router with mTLS and API key (production)
./bin/infermesh-router --prod-mode --mtls-cert cert.pem --mtls-key key.pem --api-key mysecret

# Worker connecting to router with API key
./bin/infermesh-worker --prod-mode --router wss://router-host:8080 --api-key mysecret \
  --model-path /path/to/model.gguf --backend llama-cpp
```

- `--api-key` on both router and worker enables authentication. The worker sends its API key in the `Authorization: Bearer <key>` header during registration.
- Workers always bind to `127.0.0.1` in production mode, exposing their HTTP API only locally.

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