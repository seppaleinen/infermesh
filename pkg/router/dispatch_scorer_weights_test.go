package router

import (
	"context"
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
			ID:       "worker-loaded",
			IP:       "127.0.0.1",
			Port:     8081,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: true},
				},
			},
		},
		{
			ID:       "worker-unloaded",
			IP:       "127.0.0.1",
			Port:     8082,
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
			ID:       "worker-loaded",
			IP:       "127.0.0.1",
			Port:     8081,
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Loaded: true},
				},
			},
		},
		{
			ID:       "worker-unloaded",
			IP:       "127.0.0.1",
			Port:     8082,
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
		ID:       "worker-advertiser",
		IP:       "127.0.0.1",
		Port:     8081,
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

// TestDispatchUnloadedRelayModeFailsFast verifies SPEC-P1-07/SPEC-AC-02:
// unloaded model in relay mode fails fast (<1s, 503), doesn't hang 60s.
// This is the acceptance-critical test for the cold load timeout fix.
func TestDispatchUnloadedRelayModeFailsFast(t *testing.T) {
	// Worker advertising model with Loaded=false (relay mode)
	worker := protocol.WorkerInfo{
		ID:        "relay-unloaded",
		Hostname:  "relay-host",
		IP:        "127.0.0.1",
		Port:      8081,
		Status:    protocol.StatusAvailable,
		Version:   "v1",
		LastSeen:  time.Now(),
		Transport: protocol.TransportWS, // relay mode
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "cold-model", Backend: "llama-cpp", Loaded: false},
			},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	srv.cache.cache[worker.ID] = worker
	srv.cache.mu.Unlock()

	// Mock client that fails fast for unloaded models (simulates relay worker
	// returning error immediately for cold model)
	fc := newMockWorkerClient()
	fc.results["relay-unloaded"] = mockResult{
		err: errRelayDisconnected, // worker not responding (cold load)
	}
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }

	// Request the cold model
	ctx := context.Background()
	body := []byte(`{"model":"cold-model","messages":[{"role":"user","content":"hi"}],"stream":false}`)

	// dispatchWithFailover should fail fast (<1s), not hang 60s
	start := time.Now()
	_, err := srv.dispatchWithFailover(ctx, "cold-model", body, "chat", 1, 1*time.Second)
	elapsed := time.Since(start)

	// Should fail (no workers available for cold model in relay mode)
	if err == nil {
		t.Fatal("expected error for unloaded model in relay mode, got success")
	}

	// Should fail FAST (<1s), not hang for 60s
	if elapsed > 1*time.Second {
		t.Fatalf("dispatch took %v, expected <1s (fails fast, not 60s hang)", elapsed)
	}
}
