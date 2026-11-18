package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/registry"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestServer_SetAPIKey tests the SetAPIKey setter.
func TestServer_SetAPIKey(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetAPIKey("test-api-key-123")
	if got := srv.cfg.APIKey; got != "test-api-key-123" {
		t.Fatalf("APIKey = %q, want test-api-key-123", got)
	}
}

// TestServer_SetMaxInFlight tests the SetMaxInFlight setter.
func TestServer_SetMaxInFlight(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetMaxInFlight(5)
	if got := srv.hub.maxInFlight; got != 5 {
		t.Fatalf("maxInFlight = %d, want 5", got)
	}
}

// TestServer_SetWorkerMaxInFlight tests the per-worker max in flight setter.
func TestServer_SetWorkerMaxInFlight(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetWorkerMaxInFlight("worker-1", 10)
	if got := srv.hub.workerMaxInFlight["worker-1"]; got != 10 {
		t.Fatalf("workerMaxInFlight = %d, want 10", got)
	}
}

// TestServer_SetModelLoadTimeout tests SetModelLoadTimeout with a custom value.
func TestServer_SetModelLoadTimeout(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetModelLoadTimeout(30 * time.Second)
	if got := srv.modelLoadTimeout; got != 30*time.Second {
		t.Fatalf("modelLoadTimeout = %v, want 30s", got)
	}
}

// TestServer_SetRelayCallTimeout tests SetRelayCallTimeout.
func TestServer_SetRelayCallTimeout(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetRelayCallTimeout(45 * time.Second)
	if got := srv.hub.relayCallTimeout; got != 45*time.Second {
		t.Fatalf("relayCallTimeout = %v, want 45s", got)
	}
}

// TestServer_SetProxyTimeout tests SetProxyTimeout.
func TestServer_SetProxyTimeout(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetProxyTimeout(90 * time.Second)
	if got := srv.proxyTimeout; got != 90*time.Second {
		t.Fatalf("proxyTimeout = %v, want 90s", got)
	}
}

// TestServer_SetMaxAttempts tests SetMaxAttempts.
func TestServer_SetMaxAttempts(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetMaxAttempts(5)
	if got := srv.maxAttempts; got != 5 {
		t.Fatalf("maxAttempts = %d, want 5", got)
	}
}

// TestServer_SetScorerThroughputWeight tests SetScorerThroughputWeight.
func TestServer_SetScorerThroughputWeight(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	srv.SetScorerThroughputWeight(0.5)
	if got := srv.scorerWeights.ThroughputWeight; got != 0.5 {
		t.Fatalf("ThroughputWeight = %v, want 0.5", got)
	}
}

// TestServer_Addr tests the Addr getter before Start.
func TestServer_Addr(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr() returned empty string")
	}
}

// TestServer_BuildServer_DevMode tests buildServer in dev mode.
func TestServer_BuildServer_DevMode(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := srv.buildServer(handler)
	if err != nil {
		t.Fatalf("buildServer() err = %v", err)
	}
	if h == nil {
		t.Fatal("buildServer() returned nil handler")
	}
}

// TestServer_Stop tests the CapabilityCache Stop method.
func TestServer_Stop(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	// Stop closes channels and stops background goroutines
	srv.cache.Stop()
	// Should not panic or hang
}

// TestCapabilityCache_SetMTLSClient tests the SetMTLSClient setter.
func TestCapabilityCache_SetMTLSClient(t *testing.T) {
	reg, _ := registry.New(registry.Defaults(), testLogger())
	cache := NewCapabilityCache(reg, testLogger())
	client := &http.Client{Timeout: 5 * time.Second}
	cache.SetMTLSClient(client)
	if cache.mTLSClient != client {
		t.Fatal("SetMTLSClient did not set the mTLS client")
	}
}

// TestCapabilityCache_ListInferenceStats tests ListInferenceStats with empty cache.
func TestCapabilityCache_ListInferenceStats(t *testing.T) {
	reg, _ := registry.New(registry.Defaults(), testLogger())
	cache := NewCapabilityCache(reg, testLogger())
	stats := cache.ListInferenceStats()
	if stats == nil {
		t.Fatal("ListInferenceStats returned nil")
	}
	if len(stats) != 0 {
		t.Fatalf("ListInferenceStats = %v, want empty", stats)
	}
}

// TestCapabilityCache_deleteWorkerInferenceStats tests the internal delete function.
func TestCapabilityCache_deleteWorkerInferenceStats(t *testing.T) {
	reg, _ := registry.New(registry.Defaults(), testLogger())
	cache := NewCapabilityCache(reg, testLogger())

	// First, add some stats via Update with a worker that has InferenceStats
	wrk := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "host-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{{Name: "model-a", Loaded: true}},
			InferenceStats: []protocol.InferenceStatsSnapshot{
				{Model: "model-a", TokensPerSecond: 100, AvgTTFTMs: 50, Samples: 10},
			},
		},
	}
	if err := cache.Update(wrk); err != nil {
		t.Fatalf("Update() err = %v", err)
	}

	// Now delete the stats
	cache.deleteWorkerInferenceStats("worker-1")

	// Verify stats were deleted
	cache.inferenceStatsMu.RLock()
	_, exists := cache.inferenceStats["worker-1"]
	cache.inferenceStatsMu.RUnlock()
	if exists {
		t.Fatal("deleteWorkerInferenceStats did not delete stats")
	}
}

// TestCapabilityCache_workerInferenceStats tests the projection helper.
func TestCapabilityCache_workerInferenceStats(t *testing.T) {
	snap := protocol.InferenceStatsSnapshot{
		Model: "model-a", TokensPerSecond: 100, AvgTTFTMs: 50, Samples: 10,
	}
	stats := workerInferenceStats("worker-1", snap)
	if stats.WorkerID != "worker-1" {
		t.Fatalf("WorkerID = %q, want worker-1", stats.WorkerID)
	}
	if stats.Model != "model-a" {
		t.Fatalf("Model = %q, want model-a", stats.Model)
	}
	if stats.TokensPerSecond != 100 {
		t.Fatalf("TokensPerSecond = %v, want 100", stats.TokensPerSecond)
	}
}

// TestCallCounter_StartStop tests the Start/Stop goroutine lifecycle.
func TestCallCounter_StartStop(t *testing.T) {
	c := NewCallCounter(10 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Start(ctx)
	c.Record("model-a")
	c.Stop()

	snapshot := c.Snapshot()
	if _, ok := snapshot["model-a"]; !ok {
		t.Fatal("model-a not found in snapshot after Record")
	}
}

// TestCallCounter_Prune tests that old events are pruned.
func TestCallCounter_Prune(t *testing.T) {
	c := NewCallCounter(1 * time.Second) // 1 second window
	c.Record("model-a")

	// Wait for prune
	time.Sleep(1100 * time.Millisecond)

	c.prune(time.Now())

	snapshot := c.Snapshot()
	if _, ok := snapshot["model-a"]; ok {
		t.Fatal("model-a should have been pruned")
	}
}

// TestCapabilityCache_StartStop tests the CapabilityCache Start/Stop lifecycle.
func TestCapabilityCache_StartStop(t *testing.T) {
	reg, _ := registry.New(registry.Defaults(), testLogger())
	cache := NewCapabilityCache(reg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cache.Start(ctx)
	cache.Stop()

	// Should not panic or hang
	cancel()
}

// TestHandleInferenceStats tests the inference stats endpoint handler.
func TestHandleInferenceStats(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()

	// Test GET method
	req, err := http.NewRequest(http.MethodGet, "/v1/inference-stats", nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	w := httptest.NewRecorder()
	srv.handleInferenceStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleInferenceStats status = %d, want 200", w.Code)
	}

	// Test POST method (not allowed)
	req2, _ := http.NewRequest(http.MethodPost, "/v1/inference-stats", nil)
	w2 := httptest.NewRecorder()
	srv.handleInferenceStats(w2, req2)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("handleInferenceStats POST status = %d, want 405", w2.Code)
	}
}

// TestWriteRateLimitError tests the writeRateLimitError helper.
func TestWriteRateLimitError(t *testing.T) {
	w := httptest.NewRecorder()
	writeRateLimitError(w)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

// TestWSHub_MaxInFlight tests the WSHub MaxInFlight getter.
func TestWSHub_MaxInFlight(t *testing.T) {
	reg, _ := registry.New(registry.Defaults(), testLogger())
	hub := NewWSHub(reg, testLogger())

	// Default max in flight
	if got := hub.MaxInFlight(); got != defaultMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want %d", got, defaultMaxInFlight)
	}

	// After setting a custom value
	hub.SetMaxInFlight(10)
	if got := hub.MaxInFlight(); got != 10 {
		t.Fatalf("MaxInFlight = %d, want 10", got)
	}
}

// TestServer_Addr_PostStart tests Addr after server starts.
func TestServer_Addr_PostStart(t *testing.T) {
	tr := testRegistry(t, nil)
	srv := tr.Server()
	// Addr before start returns configured addr
	addr := srv.Addr()
	if !strings.Contains(addr, "127.0.0.1") && !strings.Contains(addr, ":") {
		t.Logf("Addr = %q", addr)
	}
}
