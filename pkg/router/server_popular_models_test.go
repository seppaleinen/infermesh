package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)
func TestPopularModelsHandler(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "worker-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "llama-3-8b", Size: 4820000000, Quantization: "Q4_K_M", MaxTokens: 8192, Backend: "llama-cpp", Loaded: true},
				{Name: "mistral-7b", Size: 4370000000, Quantization: "Q5_K_M", MaxTokens: 8192, Backend: "llama-cpp", Loaded: true},
				{Name: "qwen-72b", Size: 45000000000, Quantization: "Q4_K_M", MaxTokens: 4096, Backend: "vllm", Loaded: false},
			},
			VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	// Start cache and directly populate it with worker capabilities (bypass HTTP fetch)
	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	// Directly insert into cache since there's no real HTTP server for /capabilities
	server.cache.mu.Lock()
	server.cache.cache[worker.ID] = worker
	server.cache.mu.Unlock()

	// Record some calls
	server.counter.Record("llama-3-8b")
	server.counter.Record("llama-3-8b")
	server.counter.Record("mistral-7b")

	tests := []struct {
		name           string
		method         string
		expectedStatus int
	}{
		{
			name:           "GET returns popular models sorted by loaded_worker_count desc",
			method:         http.MethodGet,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "POST returns method not allowed",
			method:         http.MethodPost,
			expectedStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/meta/models/popular", nil)
			w := httptest.NewRecorder()
			server.handlePopularModels(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if tt.method == http.MethodGet {
				var resp PopularModelsResponse
				if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				// Should have 3 models (all available regardless of Loaded)
				if len(resp.Models) != 3 {
					t.Errorf("expected 3 models, got %d", len(resp.Models))
				}

				// Verify sorted by loaded_worker_count desc, then model name asc.
				// llama-3-8b and mistral-7b are both loaded (count 1) with
				// worker_count 1; llama-3-8b comes first alphabetically. qwen-72b is not
				// loaded (count 0) so it is last.
				if len(resp.Models) < 3 || resp.Models[0].Model != "llama-3-8b" {
					t.Errorf("expected llama-3-8b first, got order: %+v", resp.Models)
				}
				if resp.Models[1].Model != "mistral-7b" {
					t.Errorf("expected mistral-7b second, got order: %+v", resp.Models)
				}
				if resp.Models[2].Model != "qwen-72b" {
					t.Errorf("expected qwen-72b last, got order: %+v", resp.Models)
				}
				// Verify call counts
				for _, m := range resp.Models {
					switch m.Model {
					case "llama-3-8b":
						if m.CallCount != 2 {
							t.Errorf("llama-3-8b call count: expected 2, got %d", m.CallCount)
						}
						if m.WorkerCount != 1 {
							t.Errorf("llama-3-8b worker count: expected 1, got %d", m.WorkerCount)
						}
					case "mistral-7b":
						if m.CallCount != 1 {
							t.Errorf("mistral-7b call count: expected 1, got %d", m.CallCount)
						}
						if m.WorkerCount != 1 {
							t.Errorf("mistral-7b worker count: expected 1, got %d", m.WorkerCount)
						}
					case "qwen-72b":
						if m.CallCount != 0 {
							t.Errorf("qwen-72b call count: expected 0, got %d", m.CallCount)
						}
						if m.WorkerCount != 1 {
							t.Errorf("qwen-72b worker count: expected 1, got %d", m.WorkerCount)
						}
					}
				}
			}
		})
	}
}
func TestPopularModelsRepeatedCallsSameOrder(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "worker-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "zeta-9b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "alpha-9b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "mid-9b", Quantization: "Q4_K_M", Loaded: false},
				{Name: "beta-9b", Quantization: "Q4_K_M", Loaded: false},
			},
			VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker.ID] = worker
	server.cache.mu.Unlock()

	var first []string
	for n := 0; n < 10; n++ {
		req := httptest.NewRequest(http.MethodGet, "/meta/models/popular", nil)
		w := httptest.NewRecorder()
		server.handlePopularModels(w, req)

		var resp PopularModelsResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("iteration %d: failed to decode response: %v", n, err)
		}
		if len(resp.Models) != 4 {
			t.Fatalf("iteration %d: expected 4 models, got %d", n, len(resp.Models))
		}
		if n == 0 {
			for _, m := range resp.Models {
				first = append(first, m.Model)
			}
		} else {
			for i, m := range resp.Models {
				if m.Model != first[i] {
					t.Fatalf("iteration %d: position %d: expected %s (from iteration 0), got %s (full order: %+v)",
						n, i, first[i], m.Model, resp.Models)
				}
			}
		}
	}
}
func TestPopularModelsMultipleWorkers(t *testing.T) {
	worker1 := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "worker-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "mistral-7b", Quantization: "Q5_K_M", Loaded: false},
			},
		},
	}

	worker2 := protocol.WorkerInfo{
		ID:       "worker-2",
		Hostname: "worker-2",
		IP:       "127.0.0.1",
		Port:     8082,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "qwen-72b", Quantization: "Q4_K_M", Loaded: false},
			},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker1, worker2})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker1.ID] = worker1
	server.cache.cache[worker2.ID] = worker2
	server.cache.mu.Unlock()

	server.counter.Record("llama-3-8b")
	server.counter.Record("qwen-72b")

	req := httptest.NewRequest(http.MethodGet, "/meta/models/popular", nil)
	w := httptest.NewRecorder()
	server.handlePopularModels(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp PopularModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// llama-3-8b has 2 workers (both loaded), qwen-72b and mistral-7b have 1
	// each (not loaded). Sorted by the total order: loaded_worker_count desc
	// puts llama-3-8b first.
	if len(resp.Models) != 3 {
		t.Errorf("expected 3 models, got %d", len(resp.Models))
	}
	if resp.Models[0].Model != "llama-3-8b" || resp.Models[0].WorkerCount != 2 {
		t.Errorf("expected llama-3-8b first with worker_count=2, got %s worker_count=%d", resp.Models[0].Model, resp.Models[0].WorkerCount)
	}
	if resp.Models[0].LoadedWorkerCount != 2 {
		t.Errorf("expected llama-3-8b loaded_worker_count=2, got %d", resp.Models[0].LoadedWorkerCount)
	}
	if resp.Models[0].CallCount != 1 {
		t.Errorf("llama-3-8b call count: expected 1, got %d", resp.Models[0].CallCount)
	}
}
func TestPopularModelsDeterministicSort(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "worker-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "zeta-9b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "alpha-9b", Quantization: "Q4_K_M", Loaded: true},
				{Name: "mid-9b", Quantization: "Q4_K_M", Loaded: false},
				{Name: "beta-9b", Quantization: "Q4_K_M", Loaded: false},
			},
			VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker.ID] = worker
	server.cache.mu.Unlock()

	// All four models share worker_count=1 and call_count=0, so the only
	// meaningful key is LoadedWorkerCount (1 vs 0) and the model-name tiebreak.
	// Expected: loaded models first (alpha, zeta — alphabetical), then
	// unloaded (beta, mid — alphabetical).
	want := []string{"alpha-9b", "zeta-9b", "beta-9b", "mid-9b"}

	for n := 0; n < 5; n++ {
		req := httptest.NewRequest(http.MethodGet, "/meta/models/popular", nil)
		w := httptest.NewRecorder()
		server.handlePopularModels(w, req)

		var resp PopularModelsResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("iteration %d: failed to decode response: %v", n, err)
		}
		if len(resp.Models) != len(want) {
			t.Fatalf("iteration %d: expected %d models, got %d", n, len(want), len(resp.Models))
		}
		for i, m := range resp.Models {
			if m.Model != want[i] {
				t.Fatalf("iteration %d: position %d: expected %s, got %s (full order: %+v)",
					n, i, want[i], m.Model, resp.Models)
			}
		}
	}
}
func TestPopularModelsLoadedVsAdvertised(t *testing.T) {
	// worker-1: shared-model Loaded=true, both-loaded Loaded=true
	worker1 := protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "worker-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "shared-model", Quantization: "Q4_K_M", Loaded: true},
				{Name: "both-loaded", Quantization: "Q4_K_M", Loaded: true},
				{Name: "only-advertised", Quantization: "Q4_K_M", Loaded: false},
			},
		},
	}

	// worker-2: shared-model Loaded=false, both-loaded Loaded=true
	worker2 := protocol.WorkerInfo{
		ID:       "worker-2",
		Hostname: "worker-2",
		IP:       "127.0.0.1",
		Port:     8082,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "shared-model", Quantization: "Q4_K_M", Loaded: false},
				{Name: "both-loaded", Quantization: "Q4_K_M", Loaded: true},
				{Name: "never-loaded", Quantization: "Q4_K_M", Loaded: false},
			},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker1, worker2})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker1.ID] = worker1
	server.cache.cache[worker2.ID] = worker2
	server.cache.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/meta/models/popular", nil)
	w := httptest.NewRecorder()
	server.handlePopularModels(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp PopularModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Build a map for easier assertion
	modelMap := make(map[string]PopularModelInfo)
	for _, m := range resp.Models {
		modelMap[m.Model] = m
	}

	// shared-model: advertised by 2 workers, loaded on 1
	if m, ok := modelMap["shared-model"]; !ok {
		t.Fatal("shared-model not found in response")
	} else {
		if m.WorkerCount != 2 {
			t.Errorf("shared-model worker_count: expected 2, got %d", m.WorkerCount)
		}
		if m.LoadedWorkerCount != 1 {
			t.Errorf("shared-model loaded_worker_count: expected 1, got %d", m.LoadedWorkerCount)
		}
	}

	// both-loaded: advertised by 2 workers, loaded on 2
	if m, ok := modelMap["both-loaded"]; !ok {
		t.Fatal("both-loaded not found in response")
	} else {
		if m.WorkerCount != 2 {
			t.Errorf("both-loaded worker_count: expected 2, got %d", m.WorkerCount)
		}
		if m.LoadedWorkerCount != 2 {
			t.Errorf("both-loaded loaded_worker_count: expected 2, got %d", m.LoadedWorkerCount)
		}
	}

	// only-advertised: advertised by 1 worker, loaded on 0
	if m, ok := modelMap["only-advertised"]; !ok {
		t.Fatal("only-advertised not found in response")
	} else {
		if m.WorkerCount != 1 {
			t.Errorf("only-advertised worker_count: expected 1, got %d", m.WorkerCount)
		}
		if m.LoadedWorkerCount != 0 {
			t.Errorf("only-advertised loaded_worker_count: expected 0, got %d", m.LoadedWorkerCount)
		}
	}

	// never-loaded: advertised by 1 worker, loaded on 0
	if m, ok := modelMap["never-loaded"]; !ok {
		t.Fatal("never-loaded not found in response")
	} else {
		if m.WorkerCount != 1 {
			t.Errorf("never-loaded worker_count: expected 1, got %d", m.WorkerCount)
		}
		if m.LoadedWorkerCount != 0 {
			t.Errorf("never-loaded loaded_worker_count: expected 0, got %d", m.LoadedWorkerCount)
		}
	}
}
