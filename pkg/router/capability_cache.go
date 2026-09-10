package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
)

// CapabilityCache wraps a registry with capability caching.
// It fetches /capabilities from workers on registry events and serves
// cached snapshots to router endpoints.
type CapabilityCache struct {
	reg    registry.Registry
	log   *slog.Logger
	mu    sync.RWMutex
	cache map[string]protocol.WorkerInfo
}

// NewCapabilityCache creates a new capability cache.
func NewCapabilityCache(reg registry.Registry, log *slog.Logger) *CapabilityCache {
	return &CapabilityCache{
		reg:   reg,
		log:   log,
		cache: make(map[string]protocol.WorkerInfo),
	}
}

// Get returns the cached capabilities for a worker.
func (c *CapabilityCache) Get(id string) (protocol.WorkerInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	w, ok := c.cache[id]
	return w, ok
}

// List returns all cached workers.
func (c *CapabilityCache) List() []protocol.WorkerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	workers := make([]protocol.WorkerInfo, 0, len(c.cache))
	for _, w := range c.cache {
		workers = append(workers, w)
	}
	return workers
}

// Update fetches capabilities from a worker's /capabilities endpoint.
func (c *CapabilityCache) Update(worker protocol.WorkerInfo) error {
	// Always seed the cache with the discovery info so the worker is
	// visible even if capability fetch fails.
	c.mu.Lock()
	c.cache[worker.ID] = worker
	c.mu.Unlock()

	caps, err := c.fetchCapabilities(worker)
	if err != nil {
		c.log.Warn("failed to fetch capabilities from worker",
			"worker", worker.ID, "error", err)
		return err
	}

	worker.Capabilities = caps
	c.mu.Lock()
	c.cache[worker.ID] = worker
	c.mu.Unlock()
	return nil
}

// fetchCapabilities fetches capabilities from a worker's HTTP endpoint.
func (c *CapabilityCache) fetchCapabilities(worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	url := fmt.Sprintf("http://%s:%d/capabilities", worker.IP, worker.Port)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return protocol.Capabilities{}, fmt.Errorf("fetching capabilities: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return protocol.Capabilities{}, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var workerResp protocol.WorkerInfo
	if err := json.NewDecoder(resp.Body).Decode(&workerResp); err != nil {
		return protocol.Capabilities{}, fmt.Errorf("decoding capabilities: %w", err)
	}

	return workerResp.Capabilities, nil
}

// Start starts the capability cache and listens for registry events.
func (c *CapabilityCache) Start(ctx context.Context) {
	sub := c.reg.Subscribe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-sub:
				if !ok {
					return
				}
				switch event.Type {
				case registry.RegisterAdded, registry.RegistryUpdated:
					if err := c.Update(event.Worker); err != nil {
						c.log.Warn("capability cache update failed", "worker", event.Worker.ID, "error", err)
					}
				case registry.RegistryRemoved, registry.RegistryUnavailable:
					c.mu.Lock()
					delete(c.cache, event.Worker.ID)
					c.mu.Unlock()
				}
			}
		}
	}()

	c.log.Info("capability cache started")
}

// Stop stops the capability cache.
func (c *CapabilityCache) Stop() {
	c.log.Info("capability cache stopped")
}