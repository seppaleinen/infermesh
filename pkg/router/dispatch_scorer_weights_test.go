package router

import (
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestBuildCandidatesWeightsEnabled verifies that when scorer weights are
// enabled (loaded-model bonus > 0 or cold-load penalty > 0), unloaded
// advertisers are included in the candidate pool alongside loaded workers.
func TestBuildCandidatesWeightsEnabled(t *testing.T) {
	workers := []protocol.WorkerInfo{
		{
			ID:    "worker-loaded",
			IP:    "127.0.0.1",
			Port:  8081,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: true},
				},
			},
		},
		{
			ID:    "worker-unloaded",
			IP:    "127.0.0.1",
			Port:  8082,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: false},
				},
			},
		},
	}
	tr := testRegistry(t, workers)
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)

	// Populate cache
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	// Enable scorer weights
	srv.SetScorerWeights(0.4, 0.25, 0.15, 0.1, 0.1, 0.15, 0.1, 0)

	candidates := srv.buildCandidates("model-a")
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates when weights enabled, got %d", len(candidates))
	}

	// Verify both loaded and unloaded workers are in candidates
	foundLoaded := false
	foundUnloaded := false
	for _, c := range candidates {
		if c.ID == "worker-loaded" {
			foundLoaded = true
		}
		if c.ID == "worker-unloaded" {
			foundUnloaded = true
		}
	}
	if !foundLoaded || !foundUnloaded {
		t.Errorf("expected both workers in candidates: loaded=%v, unloaded=%v", foundLoaded, foundUnloaded)
	}
}

// TestBuildCandidatesWeightsDisabled verifies backward compatible behavior when
// both loaded-model bonus and cold-load penalty are zero. Unloaded advertisers
// should be excluded from the candidate pool.
func TestBuildCandidatesWeightsDisabled(t *testing.T) {
	workers := []protocol.WorkerInfo{
		{
			ID:    "worker-loaded",
			IP:    "127.0.0.1",
			Port:  8081,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: true},
				},
			},
		},
		{
			ID:    "worker-unloaded",
			IP:    "127.0.0.1",
			Port:  8082,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: false},
				},
			},
		},
	}
	tr := testRegistry(t, workers)
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)

	// Populate cache
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	// Ensure weights are disabled (both 0)
	srv.SetScorerWeights(0.4, 0.25, 0.15, 0.1, 0.1, 0, 0, 0)

	candidates := srv.buildCandidates("model-a")
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate when weights disabled, got %d", len(candidates))
	}

	// Verify only loaded worker is in candidates
	if candidates[0].ID != "worker-loaded" {
		t.Errorf("expected loaded worker, got %s", candidates[0].ID)
	}
}

// TestBuildCandidatesAutoLoadPreserved verifies that the auto-load fallback
// path is preserved when no loaded workers exist, even when scorer weights are
// enabled.
func TestBuildCandidatesAutoLoadPreserved(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:    "worker-advertiser",
		IP:    "127.0.0.1",
		Port:  8081,
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "model-a", Loaded: false},
			},
		},
	}
	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)

	srv.cache.mu.Lock()
	srv.cache.cache[worker.ID] = worker
	srv.cache.mu.Unlock()

	// Enable scorer weights
	srv.SetScorerWeights(0.4, 0.25, 0.15, 0.1, 0.1, 0.15, 0.1, 0)

	// With weights enabled and no loaded workers, the unloaded advertiser
	// should be included as a candidate
	candidates := srv.buildCandidates("model-a")
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate when weights enabled and no loaded workers, got %d", len(candidates))
	}

	if candidates[0].ID != "worker-advertiser" {
		t.Errorf("expected worker-advertiser in candidates, got %s", candidates[0].ID)
	}
}
