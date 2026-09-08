package discovery

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// mdnsListener implements Listener using mDNS.
// It periodically queries for _infermesh-worker._tcp.local. services
// and emits discovery events.
type mdnsListener struct {
	config Config
	log    *slog.Logger
	events chan protocol.DiscoveryEvent
	cancel context.CancelFunc
	done   chan struct{}
}

// newMDNSListener creates a new mDNS-based Listener.
func newMDNSListener(cfg Config, log *slog.Logger) (*mdnsListener, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("discovery config: %w", err)
	}
	return &mdnsListener{
		config: cfg,
		log:    log,
		events: make(chan protocol.DiscoveryEvent, 128),
		done:   make(chan struct{}),
	}, nil
}

// Start begins probing for mDNS services at ProbeInterval.
func (l *mdnsListener) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel

	go l.probeLoop(ctx)

	l.log.Info("mDNS listener started",
		"service_type", l.config.ServiceType,
		"probe_interval", l.config.ProbeInterval,
	)
	return nil
}

// probeLoop runs the mDNS query loop until ctx is cancelled.
func (l *mdnsListener) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(l.config.ProbeInterval)
	defer ticker.Stop()

	// Initial probe immediately
	l.doProbe(ctx)

	for {
		select {
		case <-ctx.Done():
			l.log.Info("mDNS listener stopped")
			close(l.done)
			return
		case <-ticker.C:
			l.doProbe(ctx)
		}
	}
}

// doProbe performs a single mDNS query and emits events for results.
func (l *mdnsListener) doProbe(ctx context.Context) {
	// Trim .local. suffix for mdns library query
	service := l.config.ServiceType
	if strings.HasSuffix(service, ".local.") {
		service = strings.TrimSuffix(service, ".local.")
	}

	entries := make(chan *mdns.ServiceEntry, 32)
	go func() {
		// Suppress hashicorp/mdns library's log.Printf noise from IPv6 bind failures.
		// The library's unexported newClient() tries both udp4 and udp6; IPv6 multicast
		// is often unavailable on macOS. Redirecting the standard logger temporarily
		// silences these non-fatal errors while keeping IPv4 discovery working.
		origLogger := log.Default()
		log.SetOutput(io.Discard)
		err := mdns.Lookup(service, entries)
		log.SetOutput(origLogger.Writer())
		if err != nil {
			l.log.Warn("mDNS query error", "error", err)
		}
		close(entries)
	}()

	// Derive the service prefix (e.g. "_infermesh-worker._tcp") used to filter
	// mDNS entries to only those that belong to InferMesh workers. Non-InferMesh
	// devices on the network (PS4, smart TVs, etc.) announce other service types.
	servicePrefix := strings.TrimSuffix(service, ".local.")

	for entry := range entries {
		// Skip mDNS entries that are not our service type. entry.Name contains
		// the full service name (e.g. "_infermesh-worker._tcp.local.").
		if !strings.Contains(entry.Name, servicePrefix) {
			continue // Skip non-InferMesh mDNS entries
		}

		disc, err := protocol.DeserializeDiscoveryInfo(entry.Info)
		if err != nil {
			l.log.Debug("invalid worker info in mDNS entry",
				"error", err,
				"host", entry.Host,
			)
			continue
		}
		info := disc.ToWorkerInfo()

		// Fill in IP and port from mDNS layer
		if entry.AddrV4 != nil {
			info.IP = entry.AddrV4.String()
		} else if entry.AddrV6 != nil {
			info.IP = entry.AddrV6.String()
		}
		if entry.Port > 0 {
			info.Port = entry.Port
		}
		info.LastSeen = time.Now()

		// Determine event type — this listener always emits "updated"
		// since the registry tracks Added/Removed internally.
		event := protocol.DiscoveryEvent{
			Type:   protocol.EventUpdated,
			Worker: info,
			Time:   time.Now(),
		}

		select {
		case l.events <- event:
		case <-ctx.Done():
			return
		}
	}
}

// Events returns the channel of discovery events for the router to consume.
func (l *mdnsListener) Events() <-chan protocol.DiscoveryEvent {
	return l.events
}

// Stop cancels the probe loop and waits for it to finish.
func (l *mdnsListener) Stop() error {
	if l.cancel != nil {
		l.cancel()
	}
	select {
	case <-l.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}
