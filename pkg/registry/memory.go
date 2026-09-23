package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// workerState tracks a worker's state in the registry.
type workerState struct {
	info      protocol.WorkerInfo
	lastSeen  time.Time
	available bool
}

// memoryRegistry is the default in-memory Registry implementation.
// It is thread-safe via sync.RWMutex.
type memoryRegistry struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.RWMutex
	workers map[string]*workerState
	subMu   sync.Mutex
	subs    []chan Event
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a new in-memory Registry.
func New(cfg Config, log *slog.Logger) (Registry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("registry config: %w", err)
	}
	return &memoryRegistry{
		cfg:     cfg,
		log:     log,
		workers: make(map[string]*workerState),
	}, nil
}

// Start launches the heartbeat sweep goroutine.
func (r *memoryRegistry) Start(ctx context.Context) error {
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.wg.Add(1)
	go r.sweepLoop()
	return nil
}

// sweepLoop periodically checks for stale workers.
func (r *memoryRegistry) sweepLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.checkStale()
		}
	}
}

// checkStale marks unavailable/removes workers that have stopped reporting.
func (r *memoryRegistry) checkStale() {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, state := range r.workers {
		since := now.Sub(state.lastSeen)

		if since >= r.cfg.RemoveTTL {
			r.broadcast(Event{Type: RegistryRemoved, Worker: state.info})
			delete(r.workers, id)
			r.log.Info("worker removed from registry", "id", id, "stale_for", since)
		} else if since >= r.cfg.UnavailableTTL && state.available {
			state.available = false
			state.info.Status = protocol.StatusUnavailable
			r.broadcast(Event{Type: RegistryUnavailable, Worker: state.info})
			r.log.Info("worker marked unavailable", "id", id, "stale_for", since)
		}
	}
}

// HandleEvent processes a discovery event from any Discovery backend.
func (r *memoryRegistry) HandleEvent(event protocol.DiscoveryEvent) error {
	worker := event.Worker

	r.mu.Lock()
	defer r.mu.Unlock()

	state, exists := r.workers[worker.ID]
	now := time.Now()
	worker.LastSeen = now

	switch event.Type {
	case protocol.EventAdded, protocol.EventUpdated:
		if !exists {
			r.workers[worker.ID] = &workerState{
				info:      worker,
				lastSeen:  now,
				available: true,
			}
			r.broadcast(Event{Type: RegisterAdded, Worker: worker})
			r.log.Info("worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port)
		} else {
			// Update existing
			oldStatus := state.info.Status
			state.info = worker
			state.lastSeen = now
			state.available = true

			if oldStatus != protocol.StatusAvailable {
				r.broadcast(Event{Type: RegisterAdded, Worker: worker})
				r.log.Info("worker re-registered", "id", worker.ID)
			} else {
				r.broadcast(Event{Type: RegistryUpdated, Worker: worker})
			}
		}

	case protocol.EventRemoved:
		if exists {
			delete(r.workers, worker.ID)
			r.broadcast(Event{Type: RegistryRemoved, Worker: worker})
			r.log.Info("worker deregistered", "id", worker.ID)
		}

	case protocol.EventExpired:
		if exists {
			if state.available {
				state.available = false
				state.info.Status = protocol.StatusUnavailable
				r.broadcast(Event{Type: RegistryUnavailable, Worker: state.info})
				r.log.Info("worker marked unavailable", "id", worker.ID)
			}
		}
	}

	return nil
}

// Get retrieves a worker by ID.
func (r *memoryRegistry) Get(id string) (protocol.WorkerInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state, ok := r.workers[id]
	if !ok {
		return protocol.WorkerInfo{}, false
	}
	return state.info, true
}

// List returns all registered workers.
func (r *memoryRegistry) List() []protocol.WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]protocol.WorkerInfo, 0, len(r.workers))
	for _, state := range r.workers {
		result = append(result, state.info)
	}
	return result
}

// ListAvailable returns only available workers.
func (r *memoryRegistry) ListAvailable() []protocol.WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]protocol.WorkerInfo, 0)
	for _, state := range r.workers {
		if state.available {
			result = append(result, state.info)
		}
	}
	return result
}

// Subscribe returns a channel of registry events.
func (r *memoryRegistry) Subscribe() <-chan Event {
	ch := make(chan Event, 32)
	r.subMu.Lock()
	r.subs = append(r.subs, ch)
	r.subMu.Unlock()
	return ch
}

// broadcast sends an event to all subscribers.
func (r *memoryRegistry) broadcast(event Event) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- event:
		default:
			// Subscriber channel full — drop.
		}
	}
}

// Stop shuts down the registry.
// Subscriber channels obtained via Subscribe are NOT closed; they remain
// usable by the caller and will simply stop receiving events.
func (r *memoryRegistry) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}
