# ADR-0003: Kubernetes/Helm Charts Promotion

**Status:** Accepted

**Date:** 2026-10-13

## Context

InferMesh includes a Helm chart at `charts/infermesh/` and a Docker Compose file at `docker-compose.yml`, both committed in Phase 0 (commit 3d2f8f1). These distribution artifacts consume published GHCR images — they do not build anything and have no code coupling to the main repository.

Two positions were considered:

**Option 1 (chosen):** Kubernetes/Helm is an ASPIRATIONAL distribution channel, not a supported one
**Option 2 (rejected):** Promote Helm chart to first-class supported deployment with SLA

## Decision

**The Helm chart is provided as a community convenience and marked best-effort. Docker Compose is the primary homelab deployment path.**

### Rationale

1. **No Kubernetes CI:** There is no Kubernetes cluster in the test matrix. The chart is not validated by automated CI.

2. **Maintainer context:** The maintainers do not operate Kubernetes day-to-day. There is no operational expertise to support production Kubernetes deployments.

3. **Community value:** The chart provides value to homelab users who already operate Kubernetes clusters and want to deploy InferMesh there.

4. **Honest positioning:** Marking the chart as experimental/best-effort sets appropriate expectations. Users get a working starting point without an implied SLA.

5. **Docker Compose as primary:** Docker Compose is the supported, tested, and documented homelab deployment path. It works out of the box and is actively maintained.

### Documentation requirements

- README must state that Helm chart is experimental/best-effort with no SLA
- README must position Docker Compose as the primary homelab deployment path
- This ADR must document the aspirational nature for future maintainers

## Consequences

### Positive

- Helm chart remains available for Kubernetes users
- No implied support burden on maintainers
- Clear expectations for users
- Docker Compose receives primary focus and testing

### Negative

- Kubernetes users may be disappointed by lack of official support
- Chart may drift from best practices without active maintenance
- No guarantee of compatibility with future InferMesh releases

### Future considerations

If community demand for Kubernetes deployments grows, or if maintainers adopt Kubernetes operationally, this decision can be revisited. A future ADR could promote the chart to supported status with:
- Kubernetes CI integration
- Cluster-based E2E tests
- Dedicated maintainer(s) with K8s expertise
- SLA and support commitments

## Related files

- `charts/infermesh/` — Helm chart (experimental/best-effort)
- `docker-compose.yml` — Docker Compose (primary homelab deployment)
- `README.md` — Helm Chart section (updated with experimental notice)

---

*Last updated: 2026-10-13*
