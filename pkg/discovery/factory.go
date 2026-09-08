package discovery

import (
	"fmt"
	"log/slog"
)

// BackendType identifies the discovery mechanism.
type BackendType string

const (
	BackendMDNS       BackendType = "mdns"
	BackendCentralized BackendType = "centralized"
)

// NewAnnouncer creates an Announcer for the specified backend.
// Currently only BackendMDNS is implemented.
func NewAnnouncer(backend BackendType, cfg Config, log *slog.Logger) (Announcer, error) {
	switch backend {
	case BackendMDNS:
		return newMDNSAnnouncer(cfg, log)
	default:
		return nil, fmt.Errorf("unknown announcer backend: %s", backend)
	}
}

// NewListener creates a Listener for the specified backend.
// Currently only BackendMDNS is implemented.
func NewListener(backend BackendType, cfg Config, log *slog.Logger) (Listener, error) {
	switch backend {
	case BackendMDNS:
		return newMDNSListener(cfg, log)
	default:
		return nil, fmt.Errorf("unknown listener backend: %s", backend)
	}
}