# Architecture Decision Records

Architecture Decision Records (ADRs) document significant architectural decisions made during the development of InferMesh.

## Location

ADRs are stored in `docs/adr/` and follow the naming convention:

- `0001-decision-title.md`
- `0002-another-decision.md`

Sequential numbering ensures ordering and makes it easy to see all decisions at a glance.

## Format

Each ADR follows a standard template:

```markdown
# ADR-XXXX: Title

**Status:** Accepted | Rejected | Superseded | Deprecated

**Date:** YYYY-MM-DD

## Context

What is the issue that we're seeing that is motivating this decision or change?

## Decision

What is the change that we're proposing or have agreed to implement?

## Consequences

What becomes easier or more difficult to do because of this change?
```

## Status Values

- **Accepted** — The decision is approved and will be implemented.
- **Rejected** — The decision was considered but not accepted.
- **Superseded** — The decision has been replaced by a later ADR.
- **Deprecated** — The decision is no longer relevant.

## Related Files

- See `docs/contracts/` for API and protocol contracts
- See `docs/plans/` for implementation plans

---

*Last updated: 2026-10-13*
