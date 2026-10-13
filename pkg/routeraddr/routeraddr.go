// Package routeraddr provides a single helper for normalising router
// listen addresses into base URLs. Four sites previously maintained their
// own near-identical logic (issue #103 R2); this is the shared implementation.
package routeraddr

import "strings"

// defaultRouterAddr is the compile-time fallback when no address is supplied.
const defaultRouterAddr = ":8080"

// Normalize converts a router listen address into an HTTP base URL.
//
// Accepts:
//   - "" or ":8080" → "http://127.0.0.1:8080"
//   - "host:port"   → "http://host:port"
//   - ":port"        → "http://127.0.0.1:port"
//   - "http://..."   → returned as-is (trailing slash trimmed)
//
// The scheme is always http: the supervisor and worker only talk to the
// router over plain HTTP in dev mode, and over mTLS (which the RouterClient
// upgrades to https lazily) in prod mode.
func Normalize(addr string) string {
	a := strings.TrimSpace(addr)
	if a == "" {
		a = defaultRouterAddr
	}
	if strings.Contains(a, "://") {
		return strings.TrimRight(a, "/")
	}
	if strings.HasPrefix(a, ":") {
		a = "127.0.0.1" + a
	}
	return "http://" + strings.TrimRight(a, "/")
}

// Mode is the dev/prod tri-state for an InferMesh process. Four sites
// previously each computed this independently from their own flag pairs
// (issue #103 R2); resolve it once per process and pass the Mode around.
type Mode int

const (
	ModeDev  Mode = iota // default when neither flag is set
	ModeProd             // --prod-mode overrides --dev-mode
)

// ResolveMode resolves the dev/prod tri-state from a flag pair.
// prod-mode wins over dev-mode when both are set (matches the historical
// per-site behaviour in cmd/worker/main.go and cmd/router/main.go).
func ResolveMode(devMode, prodMode bool) Mode {
	if prodMode {
		return ModeProd
	}
	return ModeDev
}

// IsDev reports whether m is ModeDev.
func (m Mode) IsDev() bool { return m == ModeDev }

// IsProd reports whether m is ModeProd.
func (m Mode) IsProd() bool { return m == ModeProd }

// String returns "dev" or "prod" for logging and serialization.
func (m Mode) String() string {
	if m == ModeProd {
		return "prod"
	}
	return "dev"
}