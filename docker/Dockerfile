# InferMesh Router & Worker — Multi-stage Dockerfile
#
# Builds both router and worker binaries in a single builder stage,
# then produces two minimal distroless images.
#
# Usage:
#   # Build locally (load into local daemon, single arch):
#   docker buildx build --platform linux/amd64 \
#     -t ghcr.io/seppaleinen/infermesh/router:latest \
#     -t ghcr.io/seppaleinen/infermesh/worker:latest \
#     -f docker/Dockerfile .
#
#   # Build + push multi-arch (CI):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --push \
#     -t ghcr.io/seppaleinen/infermesh/router:v0.1.16 \
#     -t ghcr.io/seppaleinen/infermesh/router:latest \
#     -t ghcr.io/seppaleinen/infermesh/worker:v0.1.16 \
#     -t ghcr.io/seppaleinen/infermesh/worker:latest \
#     -f docker/Dockerfile .
#
# Both images default to --dev-mode. Override at runtime:
#   docker run -e ROUTER_ARGS="--prod-mode --mtls-cert=/certs/cert.pem --mtls-key=/certs/key.pem --cert-dir=/certs" ...
#   docker run -e WORKER_ARGS="--prod-mode --router=https://router:8080 --mtls-cert=/certs/cert.pem --mtls-key=/certs/key.pem" ...

# ---------- Builder stage ----------
FROM golang:1.27-alpine AS builder

# Install build dependencies (git + make for potential future use)
RUN apk add --no-cache git

WORKDIR /src

# Copy only go.mod/go.sum first for dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Remove local replace directive (app/go.mod has it for local dev)
RUN sed -i '/^replace/d' go.mod || true

# Download all module dependencies (including app module)
RUN go mod tidy

# Build all binaries: CGO_ENABLED=0 for fully static, distroless-friendly binaries
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /router ./cmd/router
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /worker ./cmd/worker
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /relay ./cmd/relay

# ---------- Router runtime image ----------
FROM gcr.io/distroless/static-debian12 AS router

# Copy router binary
COPY --from=builder /router /router

# Expose default router port
EXPOSE 8080

# Default to dev-mode (no auth, loopback only).
# ENV with a LITERAL value (not ${VAR:-default}): any shell expansion form
# bakes the default into the image layer at build time, and Docker merges
# runtime -e over image ENV by value — so a baked "--dev-mode" wins over a
# runtime -e ROUTER_ARGS="--prod-mode ...". A literal ENV is overridable.
# Override at runtime with: -e ROUTER_ARGS="--prod-mode ..."
ENV ROUTER_ARGS="--dev-mode"

# Run router. ENTRYPOINT-ONLY (no CMD): Docker concatenates ENTRYPOINT + CMD,
# so a shell CMD would yield "/router /bin/sh -c /router $ROUTER_ARGS" and the
# binary would choke on "/bin/sh" as an unknown flag. With no CMD, runtime
# args (docker run ..., docker compose command:, or the Compose default) are
# passed straight to the binary.
ENTRYPOINT ["/router"]

# ---------- Worker runtime image ----------
FROM gcr.io/distroless/static-debian12 AS worker

# Copy worker binary
COPY --from=builder /worker /worker

# Expose default worker port
EXPOSE 8081

# Default to dev-mode.
# Override at runtime with: -e WORKER_ARGS="--prod-mode --router=https://router:8080 ..."
ENV WORKER_ARGS="--dev-mode"

# Run worker (ENTRYPOINT-ONLY, see router note above)
ENTRYPOINT ["/worker"]

# ---------- Relay runtime image ----------
FROM gcr.io/distroless/static-debian12 AS relay

# Copy relay binary
COPY --from=builder /relay /relay

# Expose default relay port
EXPOSE 8090

# Default to dev-mode.
# Override at runtime with: -e RELAY_ARGS="--prod-mode ..."
ENV RELAY_ARGS="--dev-mode"

# Run relay (ENTRYPOINT-ONLY, see router note above)
ENTRYPOINT ["/relay"]