package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// mdnsAnnouncer implements Announcer using mDNS.
type mdnsAnnouncer struct {
	config Config
	log    *slog.Logger
	server *mdns.Server
	info   protocol.WorkerInfo
}

// newMDNSAnnouncer creates a new mDNS-based Announcer.
func newMDNSAnnouncer(cfg Config, log *slog.Logger) (*mdnsAnnouncer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("discovery config: %w", err)
	}
	return &mdnsAnnouncer{config: cfg, log: log}, nil
}

// Announce registers the worker's service entry with mDNS.
// The mdns.Server automatically sends periodic announcements at
// HeartbeatTTL intervals to refresh the registration.
func (a *mdnsAnnouncer) Announce(ctx context.Context, info protocol.WorkerInfo) error {
	info.LastSeen = time.Now()
	disc := protocol.DiscoveryInfo{
		ID:       info.ID,
		Hostname: info.Hostname,
		IP:       info.IP,
		Port:     info.Port,
		Version:  info.Version,
		Status:   info.Status,
	}
	txt, err := protocol.SerializeDiscoveryInfo(disc)
	if err != nil {
		return fmt.Errorf("serialize discovery info: %w", err)
	}

	// Prepare service type without trailing .local.
	service := strings.TrimSuffix(a.config.ServiceType, ".local.")

	// Build instance name: "workerID@hostname" for uniqueness
	instance := fmt.Sprintf("%s@%s", info.ID, info.Hostname)

	// Ensure hostname is FQDN (ends with .)
	hostName := info.Hostname
	if !strings.HasSuffix(hostName, ".") {
		hostName = hostName + "."
	}

	// Use explicit IPs to avoid net.LookupIP failures in container/VM environments
	ips := []net.IP{net.ParseIP(info.IP)}
	if info.IP == "" || ips[0] == nil {
		ips = []net.IP{net.IPv4(127, 0, 0, 1)}
	}

	svc, err := mdns.NewMDNSService(instance, service, "", hostName, info.Port, ips, []string{txt})
	if err != nil {
		return fmt.Errorf("create mdns service: %w", err)
	}

	server, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		return fmt.Errorf("create mDNS server: %w", err)
	}

	a.server = server
	a.info = info
	a.log.Info("mDNS service announced",
		"worker_id", info.ID,
		"port", info.Port,
		"service", service,
		"instance", instance,
	)
	return nil
}

// Start is a no-op for the announcer; Announce already started serving.
func (a *mdnsAnnouncer) Start(ctx context.Context) error {
	return nil
}

// Stop shuts down the mDNS server.
func (a *mdnsAnnouncer) Stop() error {
	if a.server != nil {
		a.log.Info("mDNS service withdrawn", "worker_id", a.info.ID)
		return a.server.Shutdown()
	}
	return nil
}

// Events is unused for the announcer; returns a nil channel.
func (a *mdnsAnnouncer) Events() <-chan protocol.DiscoveryEvent {
	return nil
}