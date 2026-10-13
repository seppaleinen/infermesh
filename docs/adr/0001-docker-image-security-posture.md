# ADR-0001: Docker Image Security Posture

**Status:** Accepted

**Date:** 2026-10-13

## Context

InferMesh provides Docker images for the router and worker components via GHCR. The images in `docker/Dockerfile` currently default to `ENV ROUTER_ARGS="--dev-mode"` and `ENV WORKER_ARGS="--dev-mode"`, exposing ports 8080/8081 with no authentication.

Two options were considered for improving the default security posture:

**Option 1 (chosen):** Keep dev-mode default + add prominent documentation warning
**Option 2 (rejected):** Generate placeholder certs at container start (entrypoint cert generation)

The images already support production mode via runtime environment variables:
- Router: `-e ROUTER_ARGS="--prod-mode --mtls-cert=/certs/cert.pem --mtls-key=/certs/key.pem --cert-dir=/certs"`
- Worker: `-e WORKER_ARGS="--prod-mode --router=https://router:8080 --mtls-cert=/certs/cert.pem --mtls-key=/certs/key.pem"`

## Decision

**Keep the dev-mode default and reject placeholder certificate generation as the default image behavior.**

The decision preserves the current behavior where images default to dev-mode for ease of use, while requiring explicit documentation warnings about the security implications.

### Rationale

1. **Complexity vs. benefit:** Placeholder certs at container start would require entrypoint cert generation logic, adding complexity with no real security benefit for the default image.

2. **Use case alignment:** Default images are intended as developer/homelab tools, not production deployments. Production users will supply real mTLS certs anyway, which the images already support.

3. **Explicit is better than implicit:** Defaulting to dev-mode makes the security posture explicit. Users must consciously opt into production mode.

4. **No lock-in:** The current design allows flexible deployment — dev-mode for local testing, explicit prod-mode flags for production.

### Documentation requirements

- The README Docker Images section must prominently warn that dev-mode has no authentication
- The warning must be visible before any usage examples
- The README must show the explicit override to prod-mode
- This ADR must document the foot-gun for future maintainers

## Consequences

### Positive

- Images remain simple and easy to use for development
- No added complexity from cert generation at container start
- Clear separation between dev (default) and prod (opt-in) deployment paths
- Production users retain flexibility to use real certs via environment variables

### Negative

- Users who expose port 8080 from a dev-mode container to the network have no authentication (foot-gun)
- Risk of accidental production deployment with dev-mode defaults
- Requires diligent documentation maintenance to keep warnings visible

### Future considerations

The issue mentioned potentially adding a log message on container start to warn when running in dev-mode with ports exposed. This would be a code change to the entrypoint and is **out of scope** for this ADR. It is recorded here as a potential future hardening step, but not implemented now to maintain the no-code-changes constraint on this documentation phase.

## Related files

- `docker/Dockerfile` — Router and worker images default to dev-mode
- `README.md` — Docker Images section (updated with warnings)
- `docker-compose.yml` — Also defaults to dev-mode

---

*Last updated: 2026-10-13*
