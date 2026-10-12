package integration

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
)

// toSchedWorker mirrors the router's protocol.WorkerInfo -> scheduler.WorkerInfo
// conversion (see pkg/router/server.go toSchedulerWorker). Unknown live signals
// (GPU util, queue depth, latency) are left negative so the scorer treats them
// as neutral.
func toSchedWorker(w protocol.WorkerInfo) scheduler.WorkerInfo {
	sw := scheduler.WorkerInfo{
		ID:            w.ID,
		Addr:          w.IP,
		GPUUtilPct:    -1,
		QueueDepth:    -1,
		AvgLatencyMS:  -1,
		LastHeartbeat: w.LastSeen,
	}
	if w.Capabilities.VRAM.TotalMB > 0 {
		sw.VRAMTotalMB = int(w.Capabilities.VRAM.TotalMB)
		sw.VRAMFreeMB = int(w.Capabilities.VRAM.FreeMB)
	}
	sw.Models = append(sw.Models, w.Capabilities.Models...)
	sw.AllowedModels = append(sw.AllowedModels, w.Capabilities.AllowedModels...)
	sw.ExcludedModels = append(sw.ExcludedModels, w.Capabilities.ExcludedModels...)
	return sw
}

func newSchedWorkers(reg registry.Registry) []scheduler.WorkerInfo {
	list := reg.ListAvailable()
	out := make([]scheduler.WorkerInfo, 0, len(list))
	for _, w := range list {
		out = append(out, toSchedWorker(w))
	}
	return out
}

func modelInfo(name string, loaded bool) protocol.ModelInfo {
	return protocol.ModelInfo{Name: name, Loaded: loaded}
}

// TestRegistryRegisterAppears verifies that a worker registered via a discovery
// "added" event immediately appears in the registry (Get + List + ListAvailable).
func TestRegistryRegisterAppears(t *testing.T) {
	reg, err := registry.New(registry.Defaults(), slog.Default())
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	w := protocol.WorkerInfo{
		ID:           "w-1",
		IP:           "10.0.0.5",
		Port:         8081,
		Status:       protocol.StatusAvailable,
		Capabilities: protocol.Capabilities{Models: []protocol.ModelInfo{modelInfo("llama-3-8b", true)}},
	}
	if err := reg.HandleEvent(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: w}); err != nil {
		t.Fatalf("HandleEvent added: %v", err)
	}

	got, ok := reg.Get("w-1")
	if !ok {
		t.Fatal("Get(w-1) not found after add event")
	}
	if got.ID != "w-1" || got.Port != 8081 {
		t.Errorf("Get(w-1) = %+v, want ID w-1 port 8081", got)
	}
	if len(reg.List()) != 1 {
		t.Errorf("List() len = %d, want 1", len(reg.List()))
	}
	if len(reg.ListAvailable()) != 1 {
		t.Errorf("ListAvailable() len = %d, want 1", len(reg.ListAvailable()))
	}
}

// TestRegistryDeterministicScoring verifies that, with two registered workers
// serving the same model, the scheduler deterministically picks the one with
// more free VRAM, and that the choice is stable across repeated calls.
func TestRegistryDeterministicScoring(t *testing.T) {
	reg, err := registry.New(registry.Defaults(), slog.Default())
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	// Both workers serve "llama-3-8b" (loaded) and share identical neutral
	// signals; the only differentiator is free VRAM ratio. Worker A has far
	// more free VRAM, so it must win.
	workers := []protocol.WorkerInfo{
		{
			ID:   "w-low",
			IP:   "10.0.0.6",
			Port: 8082,
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{modelInfo("llama-3-8b", true)},
				VRAM:   protocol.MemoryInfo{TotalMB: 10000, FreeMB: 1000},
			},
		},
		{
			ID:   "w-high",
			IP:   "10.0.0.7",
			Port: 8083,
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{modelInfo("llama-3-8b", true)},
				VRAM:   protocol.MemoryInfo{TotalMB: 10000, FreeMB: 9000},
			},
		},
	}
	for _, w := range workers {
		w.Status = protocol.StatusAvailable
		if err := reg.HandleEvent(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: w}); err != nil {
			t.Fatalf("HandleEvent added %s: %v", w.ID, err)
		}
	}

	// Force fresh heartbeats so the scorer considers both available.
	for i := range workers {
		workers[i].LastSeen = time.Now()
	}
	schedWorkers := newSchedWorkers(reg)
	for i := range schedWorkers {
		schedWorkers[i].LastHeartbeat = time.Now()
	}

	scorer := scheduler.NewWeightedScorer()
	req := scheduler.ModelRequest{Model: "llama-3-8b"}

	var firstID string
	for i := 0; i < 5; i++ {
		sel, err := scheduler.SelectWorker(context.Background(), scorer, req, schedWorkers, scorer.GetUnavailableTTL())
		if err != nil {
			t.Fatalf("SelectWorker: %v", err)
		}
		if sel.ID == "" {
			t.Fatal("SelectWorker returned empty ID")
		}
		if sel.ID != "w-high" {
			t.Fatalf("SelectWorker picked %q, want w-high (more free VRAM)", sel.ID)
		}
		if firstID == "" {
			firstID = sel.ID
		}
		if sel.ID != firstID {
			t.Fatalf("scoring not deterministic: iteration %d picked %q, first picked %q", i, sel.ID, firstID)
		}
	}
}

// TestRegistryRemovesStaleWorker verifies that a worker which stops sending
// events is first marked unavailable and then removed once the RemoveTTL
// elapses. Uses a fast registry config so the test completes quickly.
func TestRegistryRemovesStaleWorker(t *testing.T) {
	cfg := registry.Config{
		UnavailableTTL: 1 * time.Second,
		RemoveTTL:      3 * time.Second,
		CheckInterval:  600 * time.Millisecond,
	}
	reg, err := registry.New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	ctx := context.Background()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = reg.Stop() })

	w := protocol.WorkerInfo{
		ID:   "w-stale",
		IP:   "10.0.0.9",
		Port: 8084,
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{modelInfo("stale-model", true)},
		},
	}
	w.Status = protocol.StatusAvailable
	if err := reg.HandleEvent(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: w}); err != nil {
		t.Fatalf("HandleEvent added: %v", err)
	}

	// No further events from the worker: it must be marked unavailable (>= 1s)
	// and then removed (>= 3s).
	deadline := time.Now().Add(8 * time.Second)
	removed := false
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("w-stale"); !ok {
			removed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !removed {
		t.Fatalf("worker w-stale not removed after RemoveTTL; List: %+v", reg.List())
	}
	if len(reg.List()) != 0 {
		t.Errorf("List() len = %d after removal, want 0", len(reg.List()))
	}
	if len(reg.ListAvailable()) != 0 {
		t.Errorf("ListAvailable() len = %d after removal, want 0", len(reg.ListAvailable()))
	}
}

// TestRegistryCapabilityMatching verifies that a worker advertising only model X
// is selected only for requests for X, and that requests for a model no worker
// has yield ErrNoMatchingWorkers.
func TestRegistryCapabilityMatching(t *testing.T) {
	reg, err := registry.New(registry.Defaults(), slog.Default())
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	// Worker A only serves model X; worker B only serves model Y.
	workers := []protocol.WorkerInfo{
		{
			ID: "w-x",
			IP: "10.0.0.11",
			Port: 8085,
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{modelInfo("model-x", true)},
			},
		},
		{
			ID: "w-y",
			IP: "10.0.0.12",
			Port: 8086,
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{modelInfo("model-y", true)},
			},
		},
	}
	for i := range workers {
		workers[i].Status = protocol.StatusAvailable
		if err := reg.HandleEvent(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: workers[i]}); err != nil {
			t.Fatalf("HandleEvent added %s: %v", workers[i].ID, err)
		}
		workers[i].LastSeen = time.Now()
	}
	schedWorkers := newSchedWorkers(reg)
	for i := range schedWorkers {
		schedWorkers[i].LastHeartbeat = time.Now()
	}

	scorer := scheduler.NewWeightedScorer()
	ctx := context.Background()

	// Request for model-x selects w-x only.
	sel, err := scheduler.SelectWorker(ctx, scorer, scheduler.ModelRequest{Model: "model-x"}, schedWorkers, scorer.GetUnavailableTTL())
	if err != nil {
		t.Fatalf("SelectWorker(model-x): %v", err)
	}
	if sel.ID != "w-x" {
		t.Errorf("model-x selected %q, want w-x", sel.ID)
	}

	// Request for model-y selects w-y only.
	sel, err = scheduler.SelectWorker(ctx, scorer, scheduler.ModelRequest{Model: "model-y"}, schedWorkers, scorer.GetUnavailableTTL())
	if err != nil {
		t.Fatalf("SelectWorker(model-y): %v", err)
	}
	if sel.ID != "w-y" {
		t.Errorf("model-y selected %q, want w-y", sel.ID)
	}

	// Request for a model no worker advertises -> ErrNoMatchingWorkers.
	_, err = scheduler.SelectWorker(ctx, scorer, scheduler.ModelRequest{Model: "model-z"}, schedWorkers, scorer.GetUnavailableTTL())
	if !errors.Is(err, scheduler.ErrNoMatchingWorkers) {
		t.Errorf("SelectWorker(model-z) error = %v, want ErrNoMatchingWorkers", err)
	}

	// FilterByModel sanity: only w-x matches model-x.
	byX := scheduler.FilterByModel(schedWorkers, "model-x")
	if len(byX) != 1 || byX[0].ID != "w-x" {
		t.Errorf("FilterByModel(model-x) = %+v, want exactly w-x", byX)
	}
}
