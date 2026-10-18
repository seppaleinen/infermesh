package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
)

// CapabilityCache wraps a registry with capability caching.
// It fetches /capabilities from workers on registry events and serves
// cached snapshots to router endpoints.
type CapabilityCache struct {
	reg   registry.Registry
	log   *slog.Logger
	mu    sync.RWMutex
	cache map[string]protocol.WorkerInfo

	// inferenceStats holds the per-worker per-model throughput snapshot
	// advertised by workers in their Capabilities payload (issue #134).
	// It is separate from the cache map so reads of the worker map never
	// contend with writes to this map.
	inferenceStatsMu sync.RWMutex
	inferenceStats   map[string]map[string]WorkerInferenceStats
}

// WorkerInferenceStats is the router-side projection of a worker's
// per-model inference throughput snapshot, as advertised in the worker's
// Capabilities payload.
type WorkerInferenceStats struct {
	WorkerID             string  `json:"worker_id"`
	Model                string  `json:"model"`
	TokensPerSecond      float64 `json:"tokens_per_second"`
	AvgTTFTMs            float64 `json:"avg_ttft_ms"`
	AvgPromptTokens      float64 `json:"avg_prompt_tokens"`
	AvgCompletionTokens  float64 `json:"avg_completion_tokens"`
	Samples              int     `json:"samples"`
}

// NewCapabilityCache creates a new capability cache.
func NewCapabilityCache(reg registry.Registry, log *slog.Logger) *CapabilityCache {
	return &CapabilityCache{
		reg:           reg,
		log:           log,
		cache:         make(map[string]protocol.WorkerInfo),
		inferenceStats: make(map[string]map[string]WorkerInferenceStats),
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

	// Hydrate the router-side inference-stats map from the worker's
	// advertised snapshot (issue #134). A worker that reports no stats
	// clears its prior entries so stale throughput data never survives a
	// refresh.
	c.setInferenceStats(worker.ID, caps.InferenceStats)
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

// setInferenceStats replaces the router-side throughput snapshot for one
// worker with the advertised list. An empty list clears the worker's prior
// entries so stale throughput data never survives a refresh. Caller must
// not hold inferenceStatsMu.
func (c *CapabilityCache) setInferenceStats(workerID string, snaps []protocol.InferenceStatsSnapshot) {
	c.inferenceStatsMu.Lock()
	defer c.inferenceStatsMu.Unlock()

	if len(snaps) == 0 {
		delete(c.inferenceStats, workerID)
		return
	}
	byModel := make(map[string]WorkerInferenceStats, len(snaps))
	for _, s := range snaps {
		byModel[s.Model] = workerInferenceStats(workerID, s)
	}
	c.inferenceStats[workerID] = byModel
}

// GetInferenceStats returns the router-side throughput snapshot for one
// worker/model. The ok flag is false when the worker has not advertised any
// stats for that model (or the worker is unknown).
func (c *CapabilityCache) GetInferenceStats(workerID, model string) (WorkerInferenceStats, bool) {
	c.inferenceStatsMu.RLock()
	defer c.inferenceStatsMu.RUnlock()

	byModel, ok := c.inferenceStats[workerID]
	if !ok {
		return WorkerInferenceStats{}, false
	}
	s, ok := byModel[model]
	return s, ok
}

// ListInferenceStats returns every worker-model throughput snapshot the
// router has seen, sorted by worker ID then model name for deterministic
// /v1/inference-stats payloads.
func (c *CapabilityCache) ListInferenceStats() []WorkerInferenceStats {
	c.inferenceStatsMu.RLock()
	defer c.inferenceStatsMu.RUnlock()

	out := make([]WorkerInferenceStats, 0)
	for _, byModel := range c.inferenceStats {
		for _, s := range byModel {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkerID != out[j].WorkerID {
			return out[i].WorkerID < out[j].WorkerID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// deleteWorkerInferenceStats drops the router-side throughput snapshot for
// one worker. It is called from the registry event loop on
// RegistryRemoved/RegistryUnavailable so a departed worker's stale
// throughput data cannot survive in the cache.
func (c *CapabilityCache) deleteWorkerInferenceStats(workerID string) {
	c.inferenceStatsMu.Lock()
	defer c.inferenceStatsMu.Unlock()
	delete(c.inferenceStats, workerID)
}

// workerInferenceStats projects a worker's advertised
// InferenceStatsSnapshot into the router-side DTO.
func workerInferenceStats(workerID string, s protocol.InferenceStatsSnapshot) WorkerInferenceStats {
	return WorkerInferenceStats{
		WorkerID:            workerID,
		Model:               s.Model,
		TokensPerSecond:     s.TokensPerSecond,
		AvgTTFTMs:           s.AvgTTFTMs,
		AvgPromptTokens:     s.AvgPromptTokens,
		AvgCompletionTokens: s.AvgCompletionTokens,
		Samples:             s.Samples,
	}
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
					// Drop the departed worker's throughput snapshot so
					// stale tokens-per-second data cannot survive in the
					// cache (issue #134).
					c.deleteWorkerInferenceStats(event.Worker.ID)
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
