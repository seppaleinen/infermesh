# InferMesh — Distributed Inference Pool

## Overview

InferMesh is a lightweight, self-hosted, open-source system that turns a collection of heterogeneous machines with spare GPU/inference capacity into a dynamically discovered, automatically scheduled inference pool.

Think: **"Airbnb for spare inference capacity on our network."**

Not: Kubernetes, LiteLLM, or distributed model inference.

## Architecture

```
┌─────────────────┐
│ Inference Router│
├─────────────────┤
│ Discovery       │  ← mDNS
│ Registry        │  ← in-memory, heartbeats
│ Scheduler       │  ← deterministic scoring
│ Health          │  ← failure detection
│ OpenAI API      │  ← /v1/chat/completions, /v1/completions, /v1/models
└────────┬────────┘
         │
   ┌─────┼─────┐
   │     │     │
   ▼     ▼     ▼
Worker A Worker B Worker C
   │     │     │
   ▼     ▼     ▼
llama.cpp  vLLM  ollama
```

## Technology Stack

- **Language**: Go (orchestration layer)
- **Discovery**: mDNS (DNS-SD)
- **Communication**: HTTP/REST (initial), gRPC (future)
- **Security**: mTLS (self-signed), dev/prod modes
- **API**: OpenAI-compatible
- **Platforms**: Linux + macOS (Apple Silicon M1/M2/M4)

## Core Components

### Router
- Discovers workers via mDNS
- Maintains in-memory worker registry
- Tracks health via heartbeats
- Scores and schedules requests
- Exposes OpenAI-compatible API
- Routes to workers (direct or proxy mode)

### Worker
- Small standard HTTP interface:
  - `GET /health`
  - `GET /capabilities`
  - `GET /metrics`
  - `POST /v1/chat/completions`
  - `POST /v1/completions`
  - `GET /v1/models`
- Adapts underlying inference engine (llama.cpp, ollama, LM Studio) to standard interface
- Reports GPU/VRAM, model list with quantization, runtime metrics

## Directory Layout (Mono-Repo)

```
infermesh/
├── cmd/
│   ├── router/           # Router binary entry point
│   └── worker/           # Worker binary entry point
├── pkg/
│   ├── discovery/        # mDNS service discovery
│   ├── registry/         # Worker registry & heartbeats
│   ├── scheduler/        # Capability matching & scoring
│   ├── worker/           # Worker HTTP server
│   ├── router/           # Router HTTP server & API
│   ├── security/         # mTLS, auth, audit logging
│   └── platform/         # GPU detection, platform helpers
├── internal/             # Private packages
├── tests/
│   ├── integration/      # Multi-component tests
│   └── e2e/              # End-to-end tests
├── scripts/              # Helper scripts
├── Makefile
├── go.mod
├── go.sum
├── CONTEXT.md
├── README.md
└── LICENSE
```

## Testing Strategy

- **Unit tests**: Method-level, table-driven, for all complex logic in `pkg/`
- **Integration tests**: Router + worker interaction, discovery simulation, heartbeat behavior
- **E2E tests**: Full flow — start router, start workers, send request, receive streaming response
- Coverage target: 80% on `pkg/` packages

## Pipeline

| Phase | Issues | Description |
|-------|--------|-------------|
| Phase 0 — Foundation | #11, #12 | Project scaffolding, testing infrastructure |
| Phase 1 — Core Discovery & Visibility | #3, #5, #6 | mDNS discovery, capability reporting, worker registry |
| Phase 2 — Routing & API | #7, #8 | Scheduling, OpenAI-compatible API |
| Phase 3 — Hardening | #9, #10 | Security, platform support |
| Phase 4 — MVP Validation | — | End-to-end testing, dynamic join/leave, failure detection |

## Design Decisions

- **Go** over Rust: faster to implement, sufficient for MVP
- **mDNS** over centralized registry: zero-config, swappable later
- **Single router** initially: simpler, can add HA later
- **Single GPU per worker**: most machines are single-GPU
- **Manual model declaration** in MVP: auto-discovery is a future feature
- **mTLS with self-signed certs**: production security without PKI complexity
- **Dev mode (no auth, localhost HTTP)**: easy local development
- **Mono-repo**: simpler than multi-repo for MVP

## Out of MVP Scope

- Kubernetes/Docker
- Distributed model inference (model-parallel)
- Automatic model discovery/downloading
- Web UI
- Multi-router HA
- WAN/remote workers
- CPU-only workers (can add later)
- Windows support