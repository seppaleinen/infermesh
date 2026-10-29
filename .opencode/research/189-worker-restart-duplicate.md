# Research Brief: Worker restart creates duplicate registry entry (Issue #189)

## Problem Statement

When a worker restarts, the old entry remains in the registry for up to 120s (RemoveTTL) before cleanup, resulting in duplicate entries. This happens because:

1. Worker ID is randomized on each start: `pkg/worker/run.go` lines 153-157 generate `worker-%d` where `%d = rand.Intn(10000)`
2. mDNS listener only emits `EventUpdated` — no `EventRemoved` when a worker stops
3. Registry relies on TTL-based cleanup (60s → unavailable, 120s → removed)

The registry sees a restarted worker as a completely new worker, not a re-registration of the same worker.

## Current Code Locations

### Worker ID generation (pkg/worker/run.go)
- Lines 153-157:
```go
if dcfg.InstanceName == "" {
    instanceID := rand.Intn(10000)
    dcfg.InstanceName = fmt.Sprintf("worker-%d", instanceID)
}
```

### Registry cleanup (internal/registry/memory.go)
- `checkStale()` function: marks unavailable at 60s, removes at 120s
- No immediate cleanup on re-registration with different ID

### mDNS listener (pkg/discovery/mdns_listener.go)
- `doProbe()` only emits `EventUpdated`
- No mechanism to detect when a worker has stopped advertising

### Router registration (pkg/router/handlers_meta.go)
- Lines 568-580: collision check rejects duplicate ID from different peer IP
- Same peer IP: allows re-registration (logs "worker re-registered from same peer")
- But this only helps if the worker ID is the SAME

## Desired Solution

Make the worker ID **stable across restarts** so re-registration overwrites the old entry immediately:

1. **Replace random ID with stable identifier:**
   - Use hostname + fixed suffix (e.g., `worker-<hostname>`)
   - Or: use MAC address hash
   - Or: store ID in a file on first run, reuse on subsequent starts

2. **Keep the existing collision check** — it already handles same-peer-IP re-registration correctly

3. **Optionally:** Add explicit deregistration on graceful shutdown (calls router's deregister endpoint)

## Acceptance Criteria

- Worker restart with same hostname → same worker ID, immediate re-registration (no duplicate)
- Multiple workers on same host → still unique IDs (hostname + instance suffix if needed)
- Existing collision check still prevents cross-host ID spoofing
- `make lint` passes, tests green

## Constraints

- No new Go dependencies
- Follow existing conventions
- Must work in container/VM environments (no assumptions about persistent storage)
- Hostname-based approach is simplest and works everywhere

## Expected Output

Modified files:
- `pkg/worker/run.go` — replace random ID generation with stable hostname-based ID
- Possibly `pkg/discovery/config.go` — document InstanceName contract
- Tests updated if needed
- All tests pass, lint clean

## Alternative approaches considered

| Approach | Pros | Cons |
|----------|------|------|
| Reduce TTLs | Faster cleanup | More false positives from transient network issues |
| Explicit deregistration | Clean shutdown | Doesn't help on crash/kill -9; requires router connectivity |
| Stable worker ID (chosen) | Immediate re-registration, no TTL change needed | Requires hostname uniqueness (already assumed) |
