package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func TestCapabilityCache_UpdateFetchesCapabilities(t *testing.T) {
	// Create a mock HTTP server that serves /capabilities
	expectedCaps := protocol.Capabilities{
		GPU: protocol.GPUInfo{
			Vendor:      "nvidia",
			Model:       "RTX 4090",
			ComputeCore: 16384,
			TotalVRAM:   24576,
			FreeVRAM:    20480,
		},
		Models: []protocol.ModelInfo{
			{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
		},
		Engines: []string{"llama-cpp"},
		VRAM:    protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		System:  protocol.MemoryInfo{TotalMB: 32768, FreeMB: 24576},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capabilities" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resp := protocol.WorkerInfo{
			ID:           "test-worker",
			Hostname:     "test-host",
			IP:           "127.0.0.1",
			Port:         8081,
			Capabilities: expectedCaps,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Parse port from httptest server URL
	_, portStr, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	port, _ := strconv.Atoi(portStr)

	// Create cache (nil registry since we don't use the event loop here)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cache := NewCapabilityCache(nil, log)

	// Worker pointing at the mock server
	worker := protocol.WorkerInfo{
		ID:       "test-worker",
		Hostname: "test-host",
		IP:       "127.0.0.1",
		Port:     port,
	}

	err := cache.Update(worker)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Verify worker is in cache with hydrated capabilities
	got, ok := cache.Get("test-worker")
	if !ok {
		t.Fatal("expected worker to be in cache")
	}
	if len(got.Capabilities.Models) != 1 {
		t.Errorf("expected 1 model, got %d", len(got.Capabilities.Models))
	}
	if got.Capabilities.Models[0].Name != "llama-3-8b" {
		t.Errorf("expected model name llama-3-8b, got %s", got.Capabilities.Models[0].Name)
	}
	if got.Capabilities.GPU.Vendor != "nvidia" {
		t.Errorf("expected GPU vendor nvidia, got %s", got.Capabilities.GPU.Vendor)
	}
	if got.Capabilities.GPU.Model != "RTX 4090" {
		t.Errorf("expected GPU model RTX 4090, got %s", got.Capabilities.GPU.Model)
	}
}

func TestCapabilityCache_Update_FetchFails_StillVisible(t *testing.T) {
	// Create a server that returns 500 for /capabilities
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	// Parse port from httptest server URL
	_, portStr, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	port, _ := strconv.Atoi(portStr)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cache := NewCapabilityCache(nil, log)

	worker := protocol.WorkerInfo{
		ID:       "failing-worker",
		Hostname: "test-host",
		IP:       "127.0.0.1",
		Port:     port,
	}

	err := cache.Update(worker)
	if err == nil {
		t.Fatal("expected error from Update when server returns 500")
	}

	// Worker should still be visible in cache (seeded before fetch)
	got, ok := cache.Get("failing-worker")
	if !ok {
		t.Fatal("expected worker to still be in cache despite fetch failure")
	}
	if got.ID != "failing-worker" {
		t.Errorf("expected ID failing-worker, got %s", got.ID)
	}
	if got.Hostname != "test-host" {
		t.Errorf("expected hostname test-host, got %s", got.Hostname)
	}
	// Capabilities should be empty (fetch failed)
	if len(got.Capabilities.Models) != 0 {
		t.Errorf("expected empty Models after fetch failure, got %d", len(got.Capabilities.Models))
	}
}

func TestCapabilityCache_List(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cache := NewCapabilityCache(nil, log)

	// Seed two workers directly
	cache.mu.Lock()
	cache.cache["w1"] = protocol.WorkerInfo{ID: "w1", Hostname: "host1", Port: 8080}
	cache.cache["w2"] = protocol.WorkerInfo{ID: "w2", Hostname: "host2", Port: 8081}
	cache.mu.Unlock()

	workers := cache.List()
	if len(workers) != 2 {
		t.Errorf("expected 2 workers, got %d", len(workers))
	}
}

func TestCapabilityCache_GetMiss(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cache := NewCapabilityCache(nil, log)

	_, ok := cache.Get("nonexistent")
	if ok {
		t.Error("expected cache miss for nonexistent worker")
	}
}
