package registry

import (
	"context"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// EventType for registry-side state transitions.
type EventType string

const (
	RegisterAdded       EventType = "added"
	RegistryUpdated     EventType = "updated"
	RegistryUnavailable EventType = "unavailable"
	RegistryRemoved     EventType = "removed"
)

// Event is emitted by the Registry when a worker's state changes.
type Event struct {
	Type   EventType
	Worker protocol.WorkerInfo
}

// Registry is the interface for the worker registry.
// The router depends on this interface only — not on the discovery
// implementation — so the registry can be fed by any Discovery backend.
type Registry interface {
	// Start begins the heartbeat sweep goroutine.
	Start(ctx context.Context) error

	// Stop halts the heartbeat sweep and closes all event channels.
	Stop() error

	// HandleEvent processes a discovery event and updates internal state.
	HandleEvent(event protocol.DiscoveryEvent) error

	// Get returns a worker by ID.
	Get(id string) (protocol.WorkerInfo, bool)

	// List returns all workers known to the registry (any status).
	List() []protocol.WorkerInfo

	// ListAvailable returns workers whose Status is available.
	ListAvailable() []protocol.WorkerInfo

	// Subscribe returns a channel of registry-level events.
	// Subscribers are notified of Added, Updated, Unavailable, Removed.
	Subscribe() <-chan Event
}