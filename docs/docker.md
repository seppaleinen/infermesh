# Docker & Compose Guide

InferMesh ships two Docker Compose files:

| File | Use |
|------|-----|
| `docker-compose.yml` | **Pre-built images** — pulls from `ghcr.io/seppaleinen/infermesh` (with `--build` stanzas as a fallback). |
| `docker-compose.build.yml` | **Build from source** — compiles from the local source tree, no registry or GitHub access needed. |

Both files support the same opt-in profiles:

| Profile | Service | Purpose |
|---------|---------|---------|
| (none) | `router`, `worker` | Default pool (dev mode). |
| `--profile gpu` | `worker-gpu` | Worker with NVIDIA GPU access. |
| `--profile relay` | `relay` | WebSocket broker for outbound-only workers (dev mode only). |

> **⚠️ Security (ADR-0001):** Both compose files default to `--dev-mode`,
> which means **no authentication**. Anyone who can reach ports 8080/8081 can
> use the router and inject workers. Do **not** expose these ports to a
> network you do not control. For production, switch to the mTLS setup in
> [Production (mTLS)](#production-mtls).

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

### `--dev-mode` is mandatory on the worker

The worker binary has **no dev-mode default**: with no mode flag it starts in
prod mode and exits with `--model-path is required in production mode`. Both
compose files pass `--dev-mode` explicitly; if you override at runtime, keep
`--dev-mode` in the value.

### Router URL resolution

The worker resolves its router URL from three sources, in priority order:

1. `--router` flag
2. `INFERMESH_ROUTER_URL` env var
3. `http://127.0.0.1:8080` (loopback default)

The router exposes its URL on the Compose service name via
`INFERMESH_ROUTER_URL`, so the worker always registers to the router's actual
listen port — changing `--addr` on the router no longer requires a matching
`--router` edit on the worker.

The router's outbound relay URL resolves the same way: `--relay-url` flag, then
`INFERMESH_RELAY_URL`, then no relay. The relay service sets it to
`ws://relay:8090/v1/connect`.

### How args are passed to the binaries

The router, worker, and relay images are **ENTRYPOINT-ONLY** — they have no
`CMD`. That is deliberate: Docker concatenates `ENTRYPOINT` and `CMD`, so a
shell `CMD ["/bin/sh", "-c", "/router $ROUTER_ARGS"]` would actually run
`/router /bin/sh -c /router $ROUTER_ARGS`, and the binary would choke on
`/bin/sh` as an unknown flag. With no `CMD`, runtime args pass straight
through.

In the compose files the flags live in `command:` (a list, so shell quoting
cannot split them). The `*_ARGS` env vars are kept for `docker run` users who
pass them on the command line:

```bash
docker run -e ROUTER_ARGS="--dev-mode --addr :8080" \
  ghcr.io/seppaleinen/infermesh/router:latest
```

The image `ENV ROUTER_ARGS="--dev-mode"` is a **literal** value — never
`ENV ROUTER_ARGS=${ROUTER_ARGS:-"--dev-mode"}`. The shell form expands at
*build* time and bakes the default into the image layer; Docker merges runtime
`-e` over image `ENV` by *value*, so a baked `"--dev-mode"` silently wins over
a runtime `-e ROUTER_ARGS="--prod-mode ..."`.

## Build from source

Build and run directly from a local clone — no GitHub access, no registry
token, no pre-built image needed. Use the dedicated build-from-source compose
file:

```bash
docker compose -f docker-compose.build.yml build
docker compose -f docker-compose.build.yml up -d
```

This uses `docker/Dockerfile` (multi-stage, distroless) to compile the router
and worker binaries from the local source tree. The relay is built from
`docker/Dockerfile.relay` when the `relay` profile is active.

You can also build from source with the main compose file by passing
`--build`:

```bash
docker compose --build up -d
```

Both paths produce identical images; the dedicated file is just the
explicit, no-registry flow.

## GPU workers

Add `--profile gpu` to start a worker with the NVIDIA container runtime.
Requires the NVIDIA driver and the NVIDIA Container Toolkit on the host.

```bash
docker compose --profile gpu up -d
docker compose -f docker-compose.build.yml --profile gpu up -d
```

The `worker-gpu` service uses the modern Compose GPU reservation:

```yaml
deploy:
  resources:
    reservations:
      devices:
        - driver: nvidia
          count: all
          capabilities: [gpu]
```

### Alternative: `runtime: nvidia`

On hosts that predate the `deploy.resources.reservations.devices` syntax (or
when you prefer the legacy runtime), replace the `deploy:` block with:

```yaml
services:
  worker-gpu:
    runtime: nvidia
    environment:
      NVIDIA_VISIBLE_DEVICES: all   # or a specific index, e.g. "0"
```

Either approach gives the container access to all NVIDIA GPUs (or a specific
set via `NVIDIA_VISIBLE_DEVICES` / `count: <n>`).

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

`host.docker.internal` resolves to the Docker host from inside the container
(available on Linux 20.10+ and macOS/Windows by default).

> **Note:** For llama-cpp the default backend endpoint is `localhost:8080`
> INSIDE the container, which collides with the router port on the host.
> When running llama-cpp on the host, always point `--backend-url` at the
> host-side llama-cpp server explicitly.

## Relay (outbound-only workers)

For hosts that cannot accept inbound connections (firewalls, cloud instances
without public IPs), use the relay broker. Workers and the router both connect
**outbound** to the relay — no inbound ports needed on the worker host.

> **Dev-mode only.** Per the relay investigation in #141, the relay is a
> separate binary that stays dev-mode only (no auth). It is profile-gated so
> it does **not** start by default.

Start the relay:

```bash
docker compose --profile relay up -d
docker compose -f docker-compose.build.yml --profile relay up -d
```

Workers on outbound-only hosts register via the relay instead of plain HTTP:

```bash
docker run -d \
  -e WORKER_ARGS="--dev-mode --relay-url ws://relay:8090/v1/connect --backend ollama --model-path qwen3-35b" \
  ghcr.io/seppaleinen/infermesh/worker:latest
```

The router also needs a `--relay-url` flag pointing at the same relay so it
can reach outbound-only workers:

```bash
docker run -d \
  -e ROUTER_ARGS="--dev-mode --relay-url ws://relay:8090/v1/connect" \
  ghcr.io/seppaleinen/infermesh/router:latest
```

From inside a Compose network, the relay is reachable at `ws://relay:8090/v1/connect`.
From a separate host, use the relay's published port `8090` on that host's IP.

See `docs/contracts/meta-connections-count-v1.md` for the relay protocol.

## Production (mTLS)

The compose files default to dev-mode for ease of use. For a production
deployment, switch both services to prod-mode with real mTLS certificates.
The commented production example at the bottom of `docker-compose.yml` shows
the shape. Un-comment and adapt it:

```yaml
services:
  router:
    image: ghcr.io/seppaleinen/infermesh/router:latest
    ports:
      - "8080:8080"
    environment:
      ROUTER_ARGS: "--prod-mode --mtls-cert=/certs/tls.crt --mtls-key=/certs/tls.key --cert-dir=/certs"
    volumes:
      - "${CERT_DIR:-./certs}:/certs:ro"
    restart: unless-stopped

  worker:
    image: ghcr.io/seppaleinen/infermesh/worker:latest
    ports:
      - "8081:8081"
    environment:
      WORKER_ARGS: "--prod-mode --prod-router=https://router:8080 --mtls-cert=/certs/tls.crt --mtls-key=/certs/tls.key --backend lmstudio --model-path ${WORKER_MODEL_PATH:-}"
    volumes:
      - "${CERT_DIR:-./certs}:/certs:ro"
    restart: unless-stopped
    depends_on:
      - router
```

Generate the mTLS certificates with the router's cert generator (see
`pkg/security`), then mount the `./certs` directory read-only into both
containers.

## Image reference

| Image | Base | Build source | Use |
|-------|------|--------------|-----|
| `ghcr.io/seppaleinen/infermesh/router:latest` | distroless/static-debian12 | `docker/Dockerfile` (target: router) | Router API server |
| `ghcr.io/seppaleinen/infermesh/worker:latest` | distroless/static-debian12 | `docker/Dockerfile` (target: worker) | Worker (inference backend proxy) |
| `ghcr.io/seppaleinen/infermesh/relay:latest` | distroless/static-debian12 | `docker/Dockerfile.relay` | WebSocket relay broker |

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

Validate the compose files (no containers started):

```bash
make pkg-compose
```

See `AGENTS.md` for the full build & run reference.
