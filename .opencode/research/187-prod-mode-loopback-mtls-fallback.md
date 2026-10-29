# Research Brief: Prod mode loopback mTLS fallback (Issue #187)

## Problem Statement

The GUI app fails to start router + worker in production mode when no mTLS secrets are provisioned:

```
time=2026-10-28T14:25:39.881+01:00 level=ERROR msg="worker server error" role=worker error="invalid TLS configuration: mtls-cert path is required in production mode"
```

Root cause chain:
1. `app/args.go` `buildWorkerArgs` (prod): passes `--prod-router` but **never `--cert-dir`**, even when mTLS secrets exist
2. `app/args.go` `buildRouterArgs` (prod): omits mTLS flags entirely when no secrets exist
3. `pkg/router/server.go` Start(): prod mode **unconditionally** calls `ValidateTLSConfig` requiring MTLSCert, MTLSKey, CertDir/ca.crt
4. `pkg/worker/server.go` Start(): same unconditional mTLS requirement in prod mode

No fallback to plain HTTP exists for local loopback prod deployments.

## Current Code Locations

### App argument building (app/args.go)
- `buildWorkerArgs` lines 122-167: prod branch passes `--prod-router`, emits `--mtls-cert`/`--mtls-key` if secrets present, **never `--cert-dir`**
- `buildRouterArgs` lines 48-119: prod branch emits mTLS flags + `--cert-dir` only when secrets present

### Worker registration (pkg/worker/run.go)
- Line 316-322: prod-mode mTLS registration via `ProdRouter`, requires `cfg.CertDir` for `ca.crt`
- Line 287-315: dev-mode HTTP registration via `RouterBase`, relay fallback

### Server TLS validation (pkg/security/certgen.go)
- `ValidateTLSConfig` lines 192-233: returns error if any of certFile, keyFile, caCertFile empty

### Server Start() - Router (pkg/router/server.go)
- Lines 509-516: prod mode calls `ValidateTLSConfig`, then sets up mTLS middleware, then `ListenAndServeTLS`
- Lines 597-598: prod mode `ListenAndServeTLS` only; no plain HTTP fallback

### Server Start() - Worker (pkg/worker/server.go)
- Lines 226-250: prod mode `ValidateTLSConfig`, mTLS middleware, `ListenAndServeTLS`
- No plain HTTP fallback

## Desired Solution

### 1. app/args.go - buildWorkerArgs (prod branch)
```go
// If mTLS secrets present: pass --mtls-cert, --mtls-key, AND --cert-dir
// If NO secrets: fall back to --router (plain HTTP) instead of --prod-router
```

### 2. pkg/router/server.go - Start()
```go
// In prod mode: if MTLSCert=="" && CertDir=="" AND listen address is loopback
// -> skip mTLS middleware, use plain HTTP ListenAndServe
// Else -> existing mTLS path (unchanged)
```

### 3. pkg/worker/server.go - Start()
```go
// Same prod-mode loopback fallback logic as router
```

### 4. pkg/security/security.go - new helper
```go
func isLoopbackAddr(addr string) bool
// Returns true for "127.0.0.1:8080", "[::1]:8080", ":8080" (binds all but loopback-accessible)
// False for "0.0.0.0:8080" if we want strict, or true if we consider 0.0.0.0 loopback-accessible
// For GUI: router defaults to :8080 (all interfaces), worker to :8081
```

## Acceptance Criteria

- GUI prod mode with no mTLS secrets: router + worker start, register, and serve over loopback HTTP with no error
- GUI prod mode with mTLS secrets: identical behavior to today (mTLS enforced)
- Prod mode with no mTLS config on a non-loopback address (e.g., 0.0.0.0:8080 explicitly for remote): fails with clear error
- Existing mTLS tests still green; `make lint` passes

## Constraints

- No new Go dependencies
- Follow existing code conventions
- Coverage must not regress
- Pure dev pipeline work (no devops changes)

## Expected Output

Modified files:
- `app/args.go` - buildWorkerArgs prod branch fallback logic + cert-dir
- `pkg/security/security.go` - isLoopbackAddr helper
- `pkg/router/server.go` - Start() prod-mode loopback fallback
- `pkg/worker/server.go` - Start() prod-mode loopback fallback
- All tests green, `make lint` clean