# Contract: mDNS Discovery System (Issue #3)

> **Dev Team Lead** → **Architect** → **Contract**
>
> This contract defines the interfaces, data structures, and integration points for the pluggable mDNS service discovery system. It is consumed by `pkg/protocol`, `pkg/discovery`, and `pkg/registry` packages.

## 1. Overview

The discovery system enables zero-config worker discovery on a LAN. Workers advertise their presence via mDNS (DNS-SD); the router discovers them passively. The design is **pluggable**: mDNS is the first implementation, but a centralized registry can be swapped in later without changing core logic.

### Service Type
```
_infermesh-worker._tcp.local.
```

### Default Timing Parameters

| Parameter          | Default | Description                                              |
|--------------------|---------|----------------------------------------------------------|
| `ServiceType`      | `_infermesh-worker._tcp.local.` | mDNS service type string                |
| `Domain`           | `local.`                         | mDNS domain                                |
| `ProbeInterval`    | `5s`                             | Router polls for workers every N seconds    |
| `ProbeTimeout`     | `2s`                             | Per-query timeout                           |
| `HeartbeatTTL`     | `30s`                            | TTL of each worker's mDNS announcement       |
| `UnavailableTTL`   | `60s`                            | No heartbeat for this long → mark unavailable |
| `RemoveTTL`        | `120s`                           | No heartbeat for this long → remove from registry |
| `CheckInterval`    | `5s`                             | Registry heartbeat sweep interval           |

---

## 2. Protocol Package (`pkg/protocol/`)

Shared types consumed by both worker and router. Placed in `pkg/protocol/` so neither depends on the other.

### `pkg/protocol/worker.go`

```go
package protocol

import "time"

// WorkerStatus indicates the availability state of a worker.
type WorkerStatus string

const (
	StatusAvailable    WorkerStatus = "available"
	StatusBusy         WorkerStatus = "busy"
	StatusUnassigned   WorkerStatus = "unassigned"
	StatusUnavailable  WorkerStatus = "unavailable"
)

// GPUInfo describes the GPU hardware of a worker.
type GPUInfo struct {
	Vendor         string `json:"vendor"`         // "nvidia", "amd", "apple"
	Model          string `json:"model"`          // "RTX 4090", "M2 Ultra"
	ComputeCore    int    `json:"compute_cores"`  // CUDA cores / GPU cores
	TotalVRAM      int64  `json:"total_vram_mb"`  // total VRAM in megabytes
	FreeVRAM       int64  `json:"free_vram_mb"`   // currently free VRAM
}

// MemoryInfo describes system memory available to the worker.
type MemoryInfo struct {
	TotalMB int64 `json:"total_mb"`
	FreeMB  int64 `json:"free_mb"`
}

// ModelInfo describes a single model loaded or known to the worker.
type ModelInfo struct {
	Name          string   `json:"name"`            // e.g. "llama-3-8b"
	Size          int64    `json:"size_bytes"`      // model file size
	Quantization  string   `json:"quantization"`    // "Q4_K_M", "FP16", etc.
	MaxTokens     int      `json:"max_tokens"`      // context window
	Backend       string   `json:"backend"`         // "llama-cpp", "ollama", etc.
	Loaded        bool     `json:"loaded"`          // whether model is in VRAM
}

// Capabilities describes what a worker can do.
type Capabilities struct {
	GPU      GPUInfo     `json:"gpu"`
	Models   []ModelInfo `json:"models"`
	Engines  []string    `json:"engines"`   // supported backends ["llama-cpp","vllm",...]
	VRAM     MemoryInfo  `json:"vram"`
	System   MemoryInfo  `json:"system"`
}

// WorkerInfo is the canonical payload carried in mDNS TXT records.
// Both worker (announcing) and router (receiving) use this type.
type WorkerInfo struct {
	ID            string       `json:"id"`             // unique worker identifier
	Hostname      string       `json:"hostname"`       // machine hostname
	IP            string       `json:"ip"`             // resolved IPv4 address
	Port          int          `json:"port"`           // HTTP API port
	Capabilities  Capabilities `json:"capabilities"`   // GPU, models, etc.
	Status        WorkerStatus `json:"status"`         // availability
	Version       string       `json:"version"`        // InferMesh protocol version
	LastSeen      time.Time    `json:"-"`              // set by router; not serialized
}

// DiscoveryEventType categorizes a discovery event.
type DiscoveryEventType string

const (
	EventAdded     DiscoveryEventType = "added"     // new worker discovered
	EventUpdated   DiscoveryEventType = "updated"   // existing worker refreshed
	EventRemoved   DiscoveryEventType = "removed"   // worker TTL expired or explicitly gone
	EventExpired   DiscoveryEventType = "expired"   // worker marked unavailable by registry
)

// DiscoveryEvent is emitted by a Discovery implementation and consumed by the Registry.
type DiscoveryEvent struct {
	Type   DiscoveryEventType `json:"type"`
	Worker WorkerInfo          `json:"worker"`
	Time   time.Time          `json:"time"`
}
```

### `pkg/protocol/serialize.go`

```go
package protocol

import "encoding/json"

// SerializeWorkerInfo converts WorkerInfo to a compact JSON string for mDNS TXT records.
func SerializeWorkerInfo(info WorkerInfo) (string, error) {
	data, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DeserializeWorkerInfo parses a WorkerInfo from an mDNS TXT record.
func DeserializeWorkerInfo(raw string) (WorkerInfo, error) {
	var info WorkerInfo
	err := json.Unmarshal([]byte(raw), &info)
	return info, err
}
```

---

## 3. Discovery Package (`pkg/discovery/`)

### 3.1 Interfaces

`pkg/discovery/discovery.go`

```go
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
```

### 3.2 Configuration

`pkg/discovery/config.go`

```go
package discovery

import "time"

// Config controls mDNS discovery behavior.
// Values are validated by `Validate()`.
type Config struct {
	ServiceType    string        `json:"service_type"`     // "_infermesh-worker._tcp.local."
	Domain         string        `json:"domain"`           // "local."
	ProbeInterval  time.Duration `json:"probe_interval"`   // router queries every N
	ProbeTimeout   time.Duration `json:"probe_timeout"`    // per-query timeout
	HeartbeatTTL   time.Duration `json:"heartbeat_ttl"`    // mDNS announcement TTL
	Interface      string        `json:"interface"`        // network interface name ""=all
	LocalOnly      bool          `json:"local_only"`       // bind to loopback only (dev mode)
	InstanceName   string        `json:"instance_name"`    // unique name for this node
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
```

### 3.3 mDNS Implementation (Worker Side — Announcer)

`pkg/discovery/mdns.go`

```go
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/miekg/mdns"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// mdnsAnnouncer implements Announcer using mDNS.
type mdnsAnnouncer struct {
	config Config
	log    *slog.Logger
	server *mdns.Server
	info   protocol.WorkerInfo
}

// NewAnnouncer creates a new mDNS-based Announcer.
func NewAnnouncer(cfg Config, log *slog.Logger) (Announcer, error) {
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
	txt, err := protocol.SerializeWorkerInfo(info)
	if err != nil {
		return fmt.Errorf("serialize worker info: %w", err)
	}

	service := &mdns.Service{
		Service: a.config.ServiceType,
		Port:    info.Port,
		Info:    txt,
		Domain:  a.config.Domain,
		// Instance name = worker ID for deterministic DNS-SD
		Instance: info.ID,
	}

	// If no instance name set, generate one from hostname
	if info.ID != "" {
		service.Instance = info.ID
	}

	cfg := &mdns.Config{
		Local:    a.config.LocalOnly,
		IPv6:     false, // MVP: IPv4 only
	}
	if a.config.Interface != "" {
		cfg.IfName = a.config.Interface
	}

	server, err := mdns.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("create mDNS server: %w", err)
	}

	// Register the service
	if err := server.Register(service); err != nil {
		server.Shutdown()
		return fmt.Errorf("register mDNS service: %w", err)
	}

	a.server = server
	a.info = info
	a.log.Info(" mDNS service announced",
		"worker_id", info.ID,
		"port", info.Port,
		"ttl", a.config.HeartbeatTTL,
		"service", a.config.ServiceType,
	)
	return nil
}

// Start is a no-op for the announcer; Announce already started serving.
func (a *mdnsAnnouncer) Start(ctx context.Context) error {
	// mDNS server is already serving after Announce
	return nil
}

// Stop shuts down the mDNS server.
func (a *mdnsAnnouncer) Stop() error {
	if a.server != nil {
		a.log.Info(" mDNS service withdrawn", "worker_id", a.info.ID)
		return a.server.Shutdown()
	}
	return nil
}

// Events is unused for the announcer; returns a nil channel.
func (a *mdnsAnnouncer) Events() <-chan protocol.DiscoveryEvent {
	return nil
}
```

### 3.4 mDNS Implementation (Router Side — Listener)

`pkg/discovery/mdns_listener.go`

```go
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/miekg/mdns"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// mdnsListener implements Listener using mDNS.
// It periodically queries for _infermesh-worker._tcp.local. services
// and emits discovery events.
type mdnsListener struct {
	config  Config
	log     *slog.Logger
	events  chan protocol.DiscoveryEvent
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewListener creates a new mDNS-based Listener.
func NewListener(cfg Config, log *slog.Logger) (Listener, error) {
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

	l.log.Info(" mDNS listener started",
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
			l.log.Info(" mDNS listener stopped")
			close(l.done)
			return
		case <-ticker.C:
			l.doProbe(ctx)
		}
	}
}

// doProbe performs a single mDNS query and emits events for results.
func (l *mdnsListener) doProbe(ctx context.Context) {
	entries := make(chan *mdns.ServiceEntry, 32)
	go func() {
		err := mdns.Query(&mdns.QueryParams{
			Service:   l.config.ServiceType,
			Domain:    l.config.Domain,
			Entries:   entries,
			Timeout:   l.config.ProbeTimeout,
			Local:     l.config.LocalOnly,
		})
		if err != nil {
			l.log.Warn(" mDNS query error", "error", err)
		}
		close(entries)
	}()

	for entry := range entries {
		info, err := protocol.DeserializeWorkerInfo(entry.Info)
		if err != nil {
			l.log.Warn(" invalid worker info in mDNS entry",
				"error", err,
				"host", entry.Host,
			)
			continue
		}

		// Fill in IP and timestamp from mDNS layer
		if entry.AddrV4 != nil {
			info.IP = entry.AddrV4.String()
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
```

### 3.5 Factory Function

`pkg/discovery/factory.go`

```go
package discovery

import (
	"fmt"
	"log/slog"

	"github.com/seppaleinen/infermesh/pkg/protocol"
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
		return NewAnnouncer(cfg, log)
	default:
		return nil, fmt.Errorf("unknown announcer backend: %s", backend)
	}
}

// NewListener creates a Listener for the specified backend.
// Currently only BackendMDNS is implemented.
func NewListener(backend BackendType, cfg Config, log *slog.Logger) (Listener, error) {
	switch backend {
	case BackendMDNS:
		return NewListener(cfg, log)
	default:
		return nil, fmt.Errorf("unknown listener backend: %s", backend)
	}
}
```

---

## 4. Registry Package (`pkg/registry/`)

### 4.1 Interface

`pkg/registry/registry.go`

```go
package registry

import (
	"context"
	"sync"
	"time"

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

	// ListAvailable returns workers whose Status is available/bsn.
	ListAvailable() []protocol.WorkerInfo

	// Subscribe returns a channel of registry-level events.
	// Subscribers are notified of Added, Updated, Unavailable, Removed.
	Subscribe() <-chan Event
}
```

### 4.2 Configuration

`pkg/registry/config.go`

```go
package registry

import "time"

// Config controls registry behavior.
type Config struct {
	UnavailableTTL time.Duration // mark unavailable after this much silence
	RemoveTTL      time.Duration // remove from map after this much silence
	CheckInterval  time.Duration // how often to sweep for stale workers
}

// Defaults returns sensible defaults.
func Defaults() Config {
	return Config{
		UnavailableTTL: 60 * time.Second,
		RemoveTTL:      120 * time.Second,
		CheckInterval:  5 * time.Second,
	}
}

func (c Config) Validate() error {
	if c.UnavailableTTL < 1*time.Second {
		return fmt.Errorf("unavailable_ttl must be >= 1s")
	}
	if c.RemoveTTL < c.UnavailableTTL {
		return fmt.Errorf("remove_ttl must be >= unavailable_ttl")
	}
	if c.CheckInterval < 500*time.Millisecond {
		return fmt.Errorf("check_interval must be >= 500ms")
	}
	return nil
}
```

### 4.3 In-Memory Implementation

`pkg/registry/memory.go`

```go
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
	cfg        Config
	log        *slog.Logger
	mu         sync.RWMutex
	workers    map[string]*workerState
	events     chan Event
	subMu      sync.Mutex
	subs       []chan Event
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
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
		events:  make(chan Event, 128),
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
			r.emit(Event{Type: RegistryRemoved, Worker: state.info})
			r.broadcast(Event{Type: RegistryRemoved, Worker: state.info})
			delete(r.workers, id)
			r.log.Info(" worker removed from registry", "id", id, "stale_for", since)
		} else if since >= r.cfg.UnavailableTTL && state.available {
			state.available = false
			state.info.Status = protocol.StatusUnavailable
			r.emit(Event{Type: RegistryUnavailable, Worker: state.info})
			r.broadcast(Event{Type: RegistryUnavailable, Worker: state.info})
			r.log.Info(" worker marked unavailable", "id", id, "stale_for", since)
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
			r.emit(Event{Type: RegisterAdded, Worker: worker})
			r.broadcast(Event{Type: RegisterAdded, Worker: worker})
			r.log.Info(" worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port)
		} else {
			// Update existing
			oldStatus := state.info.Status
			state.info = worker
			state.lastSeen = now
			state.available = true

			if oldStatus != protocol.StatusAvailable {
				r.emit(Event{Type: RegisterAdded, Worker: worker})
				r.broadcast(Event{Type: RegisterAdded, Worker: worker})
				r.log.Info(" worker re-registered", "id", worker.ID)
			} else {
				r.emit(Event{Type: RegistryUpdated, Worker: worker})
				r.broadcast(Event{Type: RegistryUpdated, Worker: worker})
			}
		}

	case protocol.EventRemoved:
		if exists {
			delete(r.workers, worker.ID)
			r.emit(Event{Type: RegistryRemoved, Worker: worker})
			r.broadcast(Event{Type: RegistryRemoved, Worker: worker})
			r.log.Info(" worker deregistered", "id", worker.ID)
		}

	case protocol.EventExpired:
		if exists {
			if state.available {
				state.available = false
				state.info.Status = protocol.StatusUnavailable
				r.emit(Event{Type: RegistryUnavailable, Worker: state.info})
				r.broadcast(Event{Type: RegistryUnavailable, Worker: state.info})
				r.log.Info(" worker marked unavailable", "id", worker.ID)
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

// emit sends an event to the buffered internal channel.
func (r *memoryRegistry) emit(event Event) {
	select {
	case r.events <- event:
	default:
		// Buffer full — drop to avoid blocking.
	}
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
func (r *memoryRegistry) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	close(r.events)
	return nil
}
```

---

## 5. Integration: Discovery → Registry → Router

### Dependency Chain (Router Side)

```
cmd/infermesh/router.go           (router subcommand of the unified binary)
  → pkg/router/server.go          (HTTP server, OpenAI API)
    → pkg/registry.New()          (in-memory worker registry)
    → pkg/discovery.NewListener() (mDNS listener)
    → registry.HandleEvent()      (called for each discovery event)
    → registry.Subscribe()        (router reacts to worker add/remove)
```

### Dependency Chain (Worker Side)

```
cmd/infermesh/worker.go           (worker subcommand of the unified binary)
  → pkg/worker/server.go          (HTTP server, /health, /capabilities, etc.)
    → pkg/discovery.NewAnnouncer() (mDNS announcer)
    → announcer.Announce()         (advertise worker info)
    → announcer.Stop()             (withdraw on shutdown)
```

### Key Integration Points

1. **Registry consumes discovery events.** The router has a bridge goroutine:
   ```go
   go func() {
       for event := range listener.Events() {
           registry.HandleEvent(event)
       }
   }()
   ```
   Because the mDNS listener always emits `EventUpdated`, the registry internally tracks first-seen vs. re-seen to distinguish Added vs. Updated.

2. **Registry emits state-change events.** The router subscribes and reacts:
   ```go
   sub := registry.Subscribe()
   go func() {
       for event := range sub {
           switch event.Type {
           case registry.RegisterAdded:
               log.Printf("worker online: %s", event.Worker.ID)
           case registry.RegistryUnavailable:
               log.Printf("worker offline: %s", event.Worker.ID)
           case registry.RegistryRemoved:
               log.Printf("worker removed: %s", event.Worker.ID)
           }
       }
   }()
   ```

3. **Worker lifecycle.** The worker's main goroutine:
   - Starts its HTTP server
   - Creates an mDNS Announcer and calls `Announce()`
   - On SIGINT/SIGTERM: calls `announcer.Stop()`
   - The mDNS server sends a goodbye packet and withdraws the service

---

## 6. Project Structure

```
infermesh/
├── cmd/
│   ├── router/
│   │   └── main.go                 # Entry point: wires registry + discovery + HTTP API
│   └── worker/
│       └── main.go                 # Entry point: wires announcer + HTTP server
├── pkg/
│   ├── protocol/
│   │   ├── worker.go               # WorkerInfo, Capabilities, DiscoveryEvent
│   │   └── serialize.go            # JSON serialization helpers
│   ├── discovery/
│   │   ├── discovery.go            # Discovery, Announcer, Listener interfaces
│   │   ├── config.go               # Config struct + Defaults() + Validate()
│   │   ├── factory.go              # NewAnnouncer/NewListener factory
│   │   ├── mdns_announcer.go       # mDNS worker-side implementation
│   │   ├── mdns_listener.go        # mDNS router-side implementation
│   │   └── discovery_test.go       # Unit tests
│   ├── registry/
│   │   ├── registry.go             # Registry interface + Event types
│   │   ├── config.go               # Config struct + Defaults() + Validate()
│   │   ├── memory.go               # In-memory implementation
│   │   └── registry_test.go        # Unit tests
│   ├── router/
│   │   └── server.go               # HTTP server (stubs for Phase 2)
│   ├── worker/
│   │   └── server.go               # HTTP server (stubs for /health, /capabilities)
│   ├── security/
│   │   └── security.go             # mTLS / dev-mode (stubs for Phase 3)
│   └── platform/
│       └── platform.go             # GPU detection (stubs for Phase 1)
├── tests/
│   ├── integration/
│   │   └── discovery_test.go       # mDNS join/leave simulation
│   └── e2e/
│       └── discovery_test.go       # Real network discovery E2E
├── Makefile
├── go.mod
├── go.sum
├── CONTEXT.md
├── AGENTS.md
├── README.md
└── LICENSE
```

---

## 7. go.mod & Makefile

### `go.mod`

```
module github.com/seppaleinen/infermesh

go 1.22.0

require (
	github.com/miekg/mdns v1.1.28
)
```

### `Makefile`

```makefile
.PHONY: build test test-integration test-e2e lint tidy

build:
  go build -o bin/infermesh-router ./cmd/router
  go build -o bin/infermesh-worker ./cmd/worker

test:
	go test -v -race -cover ./pkg/...

test-integration:
	go test -v -race ./tests/integration/...

test-e2e:
	go test -v -race ./tests/e2e/...

lint:
	golangci-lint run

tidy:
	go mod tidy
```

---

## 8. Testing Strategy

### Unit Tests (`pkg/discovery/discovery_test.go`)

Table-driven tests for:

| Test Case                              | Input                                    | Expected                                 |
|----------------------------------------|------------------------------------------|------------------------------------------|
| Serialize/Deserialize WorkerInfo round-trip | Valid WorkerInfo with Capabilities          | Equal to original                        |
| Serialize WorkerInfo with empty fields   | WorkerInfo with zero-values               | JSON string, not error                   |
| Deserialize invalid JSON                 | malformed JSON string                     | Error returned                           |
| Config Defaults                        | N/A                                       | ServiceType == `_infermesh-worker._tcp.local.` |
| Config DevDefaults                     | N/A                                       | LocalOnly == true                        |
| Config Validate — missing ServiceType  | Config{ServiceType: ""}                  | Error                                    |
| Config Validate — short HeartbeatTTL   | Config{HeartbeatTTL: 100ms}             | Error                                    |
| Config Validate — RemoveTTL < UnavailableTTL | Config{RemoveTTL: 10s, UnavailableTTL: 60s} | Error                             |
| Registry HandleEvent — Added           | EventAdded + valid WorkerInfo            | Worker in ListAvailable                  |
| Registry HandleEvent — Updated         | EventUpdated + same ID, new Port         | Port updated in registry                 |
| Registry HandleEvent — Removed         | EventRemoved + existing ID               | Worker removed from registry             |
| Registry HandleEvent — Expired         | EventExpired + existing ID               | Worker marked unavailable                |
| Registry sweep — marks unavailable     | Worker with old lastSeen (65s)            | RegistryUnavailable event emitted        |
| Registry sweep — removes               | Worker with old lastSeen (125s)           | RegistryRemoved event emitted, gone from map |
| Registry Subscribe                     | Subscriber + HandleEvent Added            | Subscriber receives event                |

### Integration Tests (`tests/integration/discovery_test.go`)

Simulated worker join/leave with mDNS:

1. **Worker join test:**
   - Start a mock mDNS announcer (or use the real implementation in a goroutine)
   - Start a listener
   - Assert: listener receives discovery event within timeout
   - Assert: registry has the worker

2. **Worker leave test:**
   - Register a worker
   - Stop the announcer (withdraw)
   - Wait for next probe cycle
   - Assert: registry emits Unavailable event
   - Assert: after RemoveTTL, registry removes the worker

3. **Worker refresh test:**
   - Register a worker
   - Re-announce after delay
   - Assert: registry updates LastSeen, worker remains available

4. **Registry TTL test:**
   - Register a worker
   - Stop sending heartbeats
   - Assert: registry marks unavailable after UnavailableTTL
   - Assert: registry removes after RemoveTTL

### E2E Tests (`tests/e2e/discovery_test.go`)

Real network dynamic join/leave:

1. Start `infermesh-router` on machine A
2. Start `infermesh-worker` on machine B (same LAN)
3. Assert: router logs show worker registration
4. Kill the worker process on machine B
5. Assert: router logs show worker marked unavailable
6. Restart the worker on machine B
7. Assert: router logs show worker re-registered

Note: E2E tests require real network. For CI, use a single machine with loopback mDNS.

---

## 9. Assumptions & Constraints

1. **mDNS library**: `github.com/miekg/mdns` is used (as suggested in issue). This is a well-maintained, pure-Go implementation of mDNS/DNS-SD that works on Linux and macOS.

2. **IPv4 only (MVP)**: IPv6 is explicitly out of scope for this initial implementation. The mDNS config sets `IPv6: false`.

3. **mDNS query vs. passive listen**: The router uses active mDNS queries (polling) rather than passive multicast listening. This is simpler to implement and test. The `mdns.Query` function sends a multicast DNS query and waits for responses. Future phases can switch to passive listening via `mdns.Server` if needed.

4. **WorkerInfo in TXT record**: Worker metadata is encoded as JSON in the mDNS TXT record (`entry.Info`). This is a pragmatic choice — DNS-SD TXT records have a 255-byte limit per string, but mDNS libraries concatenate all TXT strings, so larger payloads work. For future, if WorkerInfo grows too large, a hybrid approach (TXID + HTTP fetch of full capabilities) can be used.

5. **No auth in dev mode**: In dev mode (`--dev-mode`), no authentication is used (per AGENTS.md). The discovery system works the same way in both modes.

6. **mTLS**: The discovery/mDNS layer is independent of mTLS (Phase 3). The router connects to workers over HTTP in dev mode; mTLS is layered on top in production mode.

7. **Registry TTL values are separate from mDNS TTL**: The mDNS TTL (30s) controls how long the OS mDNS responder caches entries. The registry's UnavailableTTL (60s) and RemoveTTL (120s) control the router's internal state. These are intentionally different to provide a grace period beyond mDNS TTL expiry.

8. **Single router**: The design assumes a single router. Multiple routers would need conflict detection (future work).

---

## 10. Swapping mDNS for Centralized Registry

To swap in a centralized registry:

1. Implement `discovery.Announcer` and `discovery.Listener` against your backend (HTTP API, Redis pub/sub, etc.)
2. Use the existing `registry.Registry` interface — no changes needed
3. The router's bridge goroutine (`listener.Events() → registry.HandleEvent()`) stays the same

The centralized registry implementation would:
- `Announce`: POST worker info to a central API
- `Listen`: Subscribe to a message queue or poll the central API
- `HandleEvent`: Same as mDNS — the registry doesn't care where events come from

This is achieved by the clean separation between `Discovery` (transport) and `Registry` (business state).

---

## 11. Sequence Diagrams

### Worker Discovery Flow

```
Worker                      mDNS                        Router
  │                           │                           │
  │── mDNS.Announce() ───────►│                           │
  │   (register service)      │                           │
  │◄── mdns.Server start ─────│                           │
  │                           │── probe() at 5s intervals │
  │                           │   Query(SRV/TXT) ────────►│
  │                           │◄── Response with WorkerInfo
  │                           │                           │
  │                           │   Emit DiscoveryEvent     │
  │                           │   (Updated)               │
  │                           │                           │
  │                           │                           │── HandleEvent()
  │                           │                           │   → registry.Add
  │                           │                           │   → emit RegistryEvent(Added)
  │                           │                           │── log: "worker registered"
```

### Worker Loss / Timeout Flow

```
Router Registry (sweep every 5s)
  │
  │  t=0s:   Worker heartbeat stops (crash / network loss)
  │
  │  t=5s:   sweep — lastSeen = 5s ago  → still available
  │
  │  t=35s:  sweep — lastSeen = 35s ago → mDNS entry expired, no probe results
  │
  │  t=60s:  sweep — lastSeen = 60s ago → UnavailableTTL reached
  │          → mark unavailable
  │          → emit RegistryEvent(Unavailable)
  │
  │  t=120s: sweep — lastSeen = 120s ago → RemoveTTL reached
  │          → delete from map
  │          → emit RegistryEvent(Removed)
```

### Worker Rejoin Flow

```
Router Registry
  │
  │  t=0s:   Worker process restarts, calls Announce()
  │          mDNS server re-registers service
  │
  │  t=5s:   Next probe picks up re-announcement
  │          → emit DiscoveryEvent(Updated)
  │          → registry: worker ID already exists but was unavailable
  │          → mark available again
  │          → emit RegistryEvent(Added) ("re-registered")
```
