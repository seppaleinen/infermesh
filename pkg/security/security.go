package security

import (
	"net"
)

// Security provides mTLS / dev-mode configuration.
type Config struct {
	DevMode            bool   `json:"dev_mode"`
	MTLSCert           string `json:"mtls_cert"`
	MTLSKey            string `json:"mtls_key"`
	CertDir            string `json:"cert_dir"`
	APIKey             string `json:"api_key"`
	TrustedCNs         string `json:"trusted_cns"`          // comma-separated CNs allowed to present client certs (empty = any)
	EnableHealthChecks bool   `json:"enable_health_checks"` // Controls periodic backend health check loop
}

func Defaults() Config {
	return Config{
		DevMode:            true,
		EnableHealthChecks: true,
	}
}

func IsDevMode(cfg Config) bool {
	return cfg.DevMode
}

// IsLoopbackAddr reports whether the given listen address binds to a
// loopback-accessible interface. Returns true for "127.0.0.1:port",
// "[::1]:port", ":port" (binds all, loopback-accessible). Returns false
// for explicit non-loopback addresses like "0.0.0.0:port" or "192.168.1.1:port".
func IsLoopbackAddr(addr string) bool {
	return isLoopbackAddr(addr)
}

// isLoopbackAddr reports whether the given listen address binds to a
// loopback-accessible interface. Returns true for "127.0.0.1:port",
// "[::1]:port", ":port" (binds all, loopback-accessible). Returns false
// for explicit non-loopback addresses like "0.0.0.0:port" or "192.168.1.1:port".
func isLoopbackAddr(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// If SplitHostPort fails, addr might be just a host without port
		// or malformed. Treat as non-loopback to be safe.
		return false
	}
	if host == "" || host == "::" {
		// Empty host or "::" means bind all interfaces (loopback accessible)
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname (e.g., "localhost") - resolve and check
		ips, err := net.LookupIP(host)
		if err != nil {
			return false
		}
		for _, ip := range ips {
			if !ip.IsLoopback() {
				return false
			}
		}
		return true
	}
	return ip.IsLoopback()
}
