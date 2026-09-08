package discovery

import (
	"context"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// Discovery is the top-level pluggable interface for all discovery backends.
// Both Announcer (worker side) and Listener (router side) embed this.
type Discovery interface {
	// Start begins the discovery process (announcing or listening).
	Start(ctx context.Context) error

	// Stop halts the discovery process and releases resources.
	Stop() error

	// Events returns a channel of discovery events for the router to consume.
	// Closed when Stop is called.
	Events() <-chan protocol.DiscoveryEvent
}

// Announcer advertises a worker's presence on the network.
// Implemented by workers.
type Announcer interface {
	Discovery

	// Announce advertises the given WorkerInfo on the local network.
	// The mDNS server handles periodic re-announcement / TTL refresh
	// until Stop is called.
	Announce(ctx context.Context, info protocol.WorkerInfo) error
}

// Listener discovers workers advertising on the network.
// Implemented by routers.
type Listener interface {
	Discovery
}