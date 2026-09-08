# InferMesh Agent Guidance

This file provides developer guidelines for working on the InferMesh project, including the agent pipeline structure, development workflow, and key technical considerations.

## Pipeline Overview

### Dev Pipeline (application code)

1. **Team Lead** → assign work
2. **Architect** → design, review architecture
3. **Backend Engineer** → implement features, tests, integration
4. **Test Engineer** → write unit/integration/E2E tests, verify fixes
5. **Code Reviewer** → review changes, approve

### DevOps Pipeline (infrastructure & deployment)

1. **Team Lead** → assign work
2. **DevOps Architect** → infrastructure design
3. **DevOps Engineer** → cluster config, CI/CD, deployment
4. **DevOps Test Engineer** → integration testing, cluster verification
5. **DevOps Reviewer** → review infrastructure changes

### Intake Workflow

**Intake → Researcher → Pipeline Lead**

- **Researcher** handles raw/ambiguous tasks: finds specs, clarifies requirements
- **Pipeline Lead** routes to appropriate pipeline (dev/devops/mixed)
- **Mixed tasks** split simultaneously to both dev and devops pipeline leads

## Development Process

### Phase-based approach

| Phase | Issues | Focus |
|-------|--------|-------|
| Phase 0 | #11, #12 | Foundation: scaffolding, testing infrastructure |
| Phase 1 | #3, #4, #5, #6 | Discovery: mDNS, worker protocol, capabilities, registry |
| Phase 2 | #7, #8 | Routing: scheduling, OpenAI API |
| Phase 3 | #9, #10 | Hardening: security, platform support |
| Phase 4 | — | Validation: E2E, dynamic join/leave, failure detection |

### Rules for mixed tasks

When a task spans dev + ops:

1. **Split** the task into dev portion + ops portion
2. **Dispatch both** pipeline leads in parallel via single user request
3. **Wait** for both to complete
4. **Synthesize** a combined result (notes each change's domain)

## Project Constraints

### Technology choices

- **Go** for orchestration (fast to implement)
- **mDNS** for initial discovery (swappable to centralized registry)
- **Single router** (simpler; HA planned later)
- **Single GPU per worker** (most machines are single-GPU)
- **Manual model declaration** in MVP (auto-discovery future feature)
- **mTLS with self-signed certs** (production security without PKI complexity)
- **Dev mode** (no auth, localhost HTTP) for easy local development
- **Mono-repo** (simpler than multi-repo for MVP)

### Out of scope (MVP)

- Kubernetes/Docker
- Distributed model inference (model-parallel)
- Automatic model discovery/downloading
- Web UI
- Multi-router HA
- WAN/remote workers
- CPU-only workers (can add later)
- Windows support
- LiteLLM integration (can be used in front later)
- Enterprise authentication beyond mTLS
- Certificate authority management (use self-signed for MVP)

## Build & Test Commands

```bash
# Build binaries
make build

# Unit tests (method-level, table-driven)
make test

# Integration tests (multi-component interaction)
make test-integration

# End-to-end tests (full flow, real network)
make test-e2e

# Linting (golangci-lint)
make lint

# Go modules
tidy:
    go mod tidy
```

### Test Levels

| Level | Location | Purpose |
|-------|----------|---------|
| Unit | `pkg/*/` | Complex logic, method-level, table-driven tests |
| Integration | `tests/integration/` | Router↔worker interaction, mDNS simulation, heartbeat tests |
| E2E | `tests/e2e/` | Full flow: start router, workers, send request, streaming response |

**Coverage target**: 80% on `pkg/` packages.

## Key Technical Considerations

### Worker Architecture

- Workers expose HTTP endpoints for discovery, capabilities, metrics, and OpenAI API
- Backend abstraction: llama.cpp / ollama / LM Studio / vLLM
- Single GPU per worker, VRAM/quantization reporting
- Heartbeat mechanism for health tracking

### Router Architecture

- In-memory worker registry (no external DB in MVP)
- Deterministic scoring for worker selection
- OpenAI-compatible API (proxy or direct worker routing)
- mTLS + API key authentication in production

### Security Patterns

```
# Dev mode (no auth, localhost)
infermesh-router --dev-mode

# Production mode (mTLS, self-signed certs)
infermesh-router --prod-mode --mtls-cert ./certs/router.crt --mtls-key ./certs/router.key
```

- Two-mode approach: dev (simple, no auth) vs production (secure, mTLS)
- Self-signed certificates for MVP (production security without PKI complexity)
- Audit logging of requests and worker events
- Worker allow/deny policies in production

### Code Style & Conventions

- Use Go 1.22+
- Table-driven unit tests for complex logic
- Integration tests for component interactions
- E2E tests for full workflow validation
- Follow standard Go module structure
- Keep dependencies minimal (Go standard library preferred)

## Running Tests Locally

### Prerequisites

- Go 1.22+
- Docker (optional, for integration tests)
- Network with multiple machines for E2E tests

### Unit Tests

```bash
# Run all unit tests
cd infermesh
make test

# Run specific package tests
make test PKG=pkg/discovery
```

### Integration Tests

```bash
# Requires Docker daemon for mDNS simulation
make test-integration
```

### E2E Tests

```bash
# Requires multiple machines or localhost simulation
make test-e2e
```

## References

- [GitHub Issues](https://github.com/seppaleinen/infermesh/issues)
- [Original Spec](https://github.com/seppaleinen/infermesh/issues/1)
- [Technology Preferences](README.md#technology-stack)
- [Testing Strategy](README.md#testing-strategy)

## How to Get Help

1. **For technical questions**: Ask the dev/devops team lead
2. **For unclear requirements**: Researcher can clarify via intake workflow
3. **For build/test issues**: DevOps team lead
4. **For architecture questions**: Architect role in appropriate pipeline

## Project Notes

This project is intentionally small and focused:

- **Core problem**: Turn heterogeneous machines with spare GPU capacity into a dynamically discovered, automatically scheduled inference pool
- **Analogy**: "Airbnb for spare inference capacity on our network"
- **Not**: Kubernetes cluster, LiteLLM, distributed model inference

Focus on the problem, not the implementation details. If something is out of scope, it will be noted in the relevant issue.

---

*Last updated: 2026-09-07*