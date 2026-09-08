package discovery

import (
	"fmt"
	"time"
)

// Config controls mDNS discovery behavior.
// Values are validated by `Validate()`.
type Config struct {
	ServiceType   string        `json:"service_type"`    // "_infermesh-worker._tcp.local."
	Domain        string        `json:"domain"`           // "local."
	ProbeInterval time.Duration `json:"probe_interval"`   // router queries every N
	ProbeTimeout  time.Duration `json:"probe_timeout"`    // per-query timeout
	HeartbeatTTL  time.Duration `json:"heartbeat_ttl"`    // mDNS announcement TTL
	Interface     string        `json:"interface"`        // network interface name ""=all
	LocalOnly     bool          `json:"local_only"`       // bind to loopback only (dev mode)
	InstanceName  string        `json:"instance_name"`    // unique name for this node
}

// Defaults returns a Config populated with production-safe defaults.
func Defaults() Config {
	return Config{
		ServiceType:   "_infermesh-worker._tcp.local.",
		Domain:        "local.",
		ProbeInterval: 5 * time.Second,
		ProbeTimeout:  2 * time.Second,
		HeartbeatTTL:  30 * time.Second,
		Interface:     "",
		LocalOnly:     false,
	}
}

// DevDefaults returns a Config tuned for local development.
func DevDefaults() Config {
	c := Defaults()
	c.LocalOnly = true
	return c
}

// Validate checks the config for required fields and sensible values.
func (c Config) Validate() error {
	if c.ServiceType == "" {
		return fmt.Errorf("service_type is required")
	}
	if c.Domain == "" {
		return fmt.Errorf("domain is required")
	}
	if c.ProbeInterval < 1*time.Second {
		return fmt.Errorf("probe_interval must be >= 1s")
	}
	if c.ProbeTimeout < 100*time.Millisecond {
		return fmt.Errorf("probe_timeout must be >= 100ms")
	}
	if c.HeartbeatTTL < 1*time.Second {
		return fmt.Errorf("heartbeat_ttl must be >= 1s")
	}
	return nil
}