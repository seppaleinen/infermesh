# Docker & Compose Guide

InferMesh ships with a single Docker Compose file that supports both
pre-built images and build-from-source in one file.

## Pre-built images (default)

Pull published images from `ghcr.io/seppaleinen/infermesh` and start with a
single command:

```bash
docker compose up -d
```

This starts:
- **router** on `:8080` (dev mode, no auth, loopback-only)
- **worker** on `:8081` (dev mode, registers to router over plain HTTP)

Override flags at runtime via environment variables:

```bash
docker compose up -d \
  -e ROUTER_ARGS="--dev-mode --api-key mykey" \
  -e WORKER_ARGS="--dev-mode --router http://router:8080 --backend ollama --model-path /models/qwen3-35b"
```

## Build from source

Build and run directly from a local clone — no GitHub access or pre-built
image needed. The `build:` stanzas on each service mean `--build` pulls from
the local source tree instead of the registry:

```bash
docker compose --build up -d
```

This uses `docker/Dockerfile` (multi-stage, distroless) to compile the
router, worker, and relay binaries from the local source tree.

## GPU workers

Add `--profile gpu` to start the worker with the NVIDIA container runtime.
Requires the NVIDIA driver and container runtime on the host.

```bash
docker compose --profile gpu up -d
```

### Backend inside vs. outside the container

The worker container needs an inference backend reachable from inside it.
Two approaches:

**Backend inside the container** (recommended for ollama):

```yaml
services:
  ollama:
    image: ollama/ollama:latest
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
    restart: unless-stopped

  worker:
    environment:
      WORKER_ARGS: "--dev-mode --router http://router:8080 --backend ollama --model-path qwen3-35b"
```

The worker's default `--backend-url` for ollama is `http://localhost:11434`,
which resolves to the ollama container when they share the Compose network.

**Backend on the host** (for llama-cpp, lmstudio, vLLM):

```yaml
services:
  worker:
    environment:
      WORKER_ARGS: "--dev-mode --router http://router:8080 --backend lmstudio --backend-url http://host.docker.internal:1234 --model-path /models/qwen3-35b"
```

`host.docker.internal` resolves to the Docker host from inside the container.

## Relay (outbound-only workers)

For hosts that cannot accept inbound connections (firewalls, cloud instances
without public IPs), use the relay broker. Workers and the router both connect
**outbound** to the relay — no inbound ports needed.

Start the relay:

```bash
docker compose --profile relay up -d
```

Workers on outbound-only hosts register via the relay instead of plain HTTP:

```bash
docker run -d \
  -e WORKER_ARGS="--dev-mode --relay-url ws://192.168.1.216:8090/v1/connect --backend ollama --model-path qwen3-35b" \
  ghcr.io/seppaleinen/infermesh/worker:latest
```

The router also needs a `--relay-url` flag pointing at the same relay so it
can reach outbound-only workers:

```bash
docker run -d \
  -e ROUTER_ARGS="--dev-mode --relay-url ws://relay:8090/v1/connect" \
  ghcr.io/seppaleinen/infermesh/router:latest
```

See `docs/contracts/meta-connections-count-v1.md` for the relay protocol.

## Image reference

| Image | Base | Use |
|-------|------|-----|
| `ghcr.io/seppaleinen/infermesh/router:latest` | distroless/static-debian12 | Router API server |
| `ghcr.io/seppaleinen/infermesh/worker:latest` | distroless/static-debian12 | Worker (inference backend proxy) |
| `ghcr.io/seppaleinen/infermesh/relay:latest` | distroless/static-debian12 | WebSocket relay broker |

All images are fully static (CGO_ENABLED=0), multi-arch (amd64 + arm64),
and default to `--dev-mode`. Override at runtime via `ROUTER_ARGS`,
`WORKER_ARGS`, or `RELAY_ARGS`.

## Development

Build and iterate locally without Docker:

```bash
make build
./bin/infermesh-router --dev-mode
./bin/infermesh-worker --dev-mode --router http://127.0.0.1:8080 --backend lmstudio --model-path /path/to/model
```

See `AGENTS.md` for the full build & run reference.