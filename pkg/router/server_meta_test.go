package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestModelsListHandler tests the /v1/models endpoint when there are loaded models.
func TestModelsListHandler(t *testing.T) {
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

	tests := []struct {
		name           string
		method         string
		expectedStatus int
		expectModels   bool
		expectedCount  int
	}{
		{
			name:           "GET returns model list",
			method:         http.MethodGet,
			expectedStatus: http.StatusOK,
			expectModels:   true,
			expectedCount:  2, // only loaded models
		},
		{
			name:           "POST returns method not allowed",
			method:         http.MethodPost,
			expectedStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/v1/models", nil)
			w := httptest.NewRecorder()
			server.handleModelsList(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if tt.expectModels {
				var resp protocol.ModelsResponse
				if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if resp.Object != "list" {
					t.Errorf("expected object 'list', got '%s'", resp.Object)
				}

				if len(resp.Data) != tt.expectedCount {
					t.Errorf("expected %d models, got %d", tt.expectedCount, len(resp.Data))
				}

				// Verify only loaded models are returned
				for _, m := range resp.Data {
					if !m.Loaded {
						t.Errorf("model %s should be loaded", m.Name)
					}
				}
			}
		})
	}
}

// TestModelsListHandlerNoWorkers tests the /v1/models endpoint when no workers are available.
func TestModelsListHandlerNoWorkers(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	server.handleModelsList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("expected object 'list', got '%s'", resp.Object)
	}

	if len(resp.Data) != 0 {
		t.Errorf("expected 0 models, got %d", len(resp.Data))
	}
}

func TestDevRegisterValidWorker(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	worker := protocol.WorkerInfo{
		ID:   "dev-worker-1",
		IP:   "127.0.0.1",
		Port: 8081,
	}

	body, _ := json.Marshal(worker)
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()

	server.handleDevRegister(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	// Verify worker was registered in the registry
	got, ok := tr.reg.Get("dev-worker-1")
	if !ok {
		t.Fatal("worker not found in registry after registration")
	}
	if got.ID != "dev-worker-1" {
		t.Errorf("expected worker ID 'dev-worker-1', got '%s'", got.ID)
	}
	if got.IP != "127.0.0.1" {
		t.Errorf("expected worker IP '127.0.0.1', got '%s'", got.IP)
	}
	if got.Status != protocol.StatusAvailable {
		t.Errorf("expected status 'available', got '%s'", got.Status)
	}
}
func TestDevRegisterEmptyID(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	worker := protocol.WorkerInfo{
		ID:   "",
		IP:   "127.0.0.1",
		Port: 8081,
	}

	body, _ := json.Marshal(worker)
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleDevRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}
func TestDevRegisterInvalidPort(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name string
		port int
	}{
		{"port zero", 0},
		{"port negative", -1},
		{"port too high", 65536},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := protocol.WorkerInfo{
				ID:   "dev-worker-bad-port",
				IP:   "127.0.0.1",
				Port: tt.port,
			}
			body, _ := json.Marshal(worker)
			req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			server.handleDevRegister(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status %d for port %d, got %d", http.StatusBadRequest, tt.port, w.Code)
			}
		})
	}
}
func TestDevRegisterIPDerivedFromRemoteAddr(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name          string
		workerID      string
		bodyIP        string
		remoteAddr    string
		expectStatus  int
		expectRegIP   string // expected registered IP for 200 responses
		expectErrCode string // expected error code for 4xx responses
	}{
		{
			name:         "single-host, body matches",
			workerID:     "ip-derived-match",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "127.0.0.1:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "127.0.0.1",
		},
		{
			name:         "cross-machine LAN",
			workerID:     "ip-derived-lan",
			bodyIP:       "192.168.1.216",
			remoteAddr:   "192.168.1.216:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:         "derived overrides wrong body IP",
			workerID:     "ip-derived-override",
			bodyIP:       "10.0.0.99",
			remoteAddr:   "192.168.1.216:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:         "empty body IP, derived fills it",
			workerID:     "ip-derived-empty",
			bodyIP:       "",
			remoteAddr:   "192.168.1.216:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:          "empty RemoteAddr",
			workerID:      "ip-derived-empty-remote",
			bodyIP:        "127.0.0.1",
			remoteAddr:    "",
			expectStatus:  http.StatusBadRequest,
			expectErrCode: "ip_validation_error",
		},
		{
			name:          "unparseable host",
			workerID:      "ip-derived-bad-host",
			bodyIP:        "127.0.0.1",
			remoteAddr:    "not-an-ip:1234",
			expectStatus:  http.StatusBadRequest,
			expectErrCode: "ip_validation_error",
		},
		{
			name:          "IPv6 host",
			workerID:      "ip-derived-ipv6",
			bodyIP:        "127.0.0.1",
			remoteAddr:    "[2001:db8::5]:1234",
			expectStatus:  http.StatusBadRequest,
			expectErrCode: "ip_validation_error",
		},
		{
			name:         "localhost host",
			workerID:     "ip-derived-localhost",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "localhost:1234",
			expectStatus: http.StatusOK,
			expectRegIP:  "127.0.0.1",
		},
		{
			name:         "IPv4-mapped IPv6",
			workerID:     "ip-derived-v4mapped",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "[::ffff:192.168.1.216]:1234",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:         "IPv4 host with brackets",
			workerID:     "ip-derived-bracketed",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "[192.168.1.216]:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:         "host without port",
			workerID:     "ip-derived-noprt",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "192.168.1.216",
			expectStatus: http.StatusOK,
			expectRegIP:  "192.168.1.216",
		},
		{
			name:          "port-only address",
			workerID:      "ip-derived-portonly",
			bodyIP:        "127.0.0.1",
			remoteAddr:    "50000",
			expectStatus:  http.StatusBadRequest,
			expectErrCode: "ip_validation_error",
		},
		{
			name:         "IPv6 loopback",
			workerID:     "ip-derived-v6loop",
			bodyIP:       "127.0.0.1",
			remoteAddr:   "[::1]:50000",
			expectStatus: http.StatusOK,
			expectRegIP:  "127.0.0.1",
		},
		{
			name:          "hostname peer",
			workerID:      "ip-derived-hostname",
			bodyIP:        "127.0.0.1",
			remoteAddr:    "worker.local:50000",
			expectStatus:  http.StatusBadRequest,
			expectErrCode: "ip_validation_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := protocol.WorkerInfo{
				ID:   tt.workerID,
				IP:   tt.bodyIP,
				Port: 8081,
			}
			body, _ := json.Marshal(worker)
			req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
			req.RemoteAddr = tt.remoteAddr
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			server.handleDevRegister(w, req)

			if w.Code != tt.expectStatus {
				t.Fatalf("expected status %d, got %d, body: %s", tt.expectStatus, w.Code, w.Body.String())
			}

			if tt.expectStatus == http.StatusOK {
				got, ok := tr.reg.Get(tt.workerID)
				if !ok {
					t.Fatal("worker not found in registry after registration")
				}
				if got.IP != tt.expectRegIP {
					t.Errorf("expected registered IP '%s', got '%s'", tt.expectRegIP, got.IP)
				}
				if got.Status != protocol.StatusAvailable {
					t.Errorf("expected status 'available', got '%s'", got.Status)
				}
			} else {
				var errResp ErrorResponse
				if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
					t.Fatalf("failed to decode error body: %v", err)
				}
				if errResp.Error.Code != tt.expectErrCode {
					t.Errorf("expected error code '%s', got '%s'", tt.expectErrCode, errResp.Error.Code)
				}
			}
		})
	}
}
func TestDevRegisterMethodNotAllowed(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/dev/register", nil)
			w := httptest.NewRecorder()

			server.handleDevRegister(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected status %d for %s, got %d", http.StatusMethodNotAllowed, method, w.Code)
			}
		})
	}
}
func TestDevRegisterMalformedJSON(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader([]byte("{invalid json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleDevRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}
func TestDevRegisterForcesAvailableStatus(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	worker := protocol.WorkerInfo{
		ID:     "dev-worker-status",
		IP:     "127.0.0.1",
		Port:   9090,
		Status: protocol.StatusBusy, // should be overridden to available
	}

	body, _ := json.Marshal(worker)
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()

	server.handleDevRegister(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	// Verify the status was forced to available
	got, ok := tr.reg.Get("dev-worker-status")
	if !ok {
		t.Fatal("worker not found in registry")
	}
	if got.Status != protocol.StatusAvailable {
		t.Errorf("expected status 'available', got '%s'", got.Status)
	}
	if got.Port != 9090 {
		t.Errorf("expected port 9090, got %d", got.Port)
	}
}
func TestDevRegisterProdModeIPValidation(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.ServerWithCfg(security.Config{DevMode: false})

	t.Run("non-loopback IP rejected", func(t *testing.T) {
		worker := protocol.WorkerInfo{
			ID:   "prod-worker-lan",
			IP:   "192.168.1.216",
			Port: 8081,
		}
		body, _ := json.Marshal(worker)
		req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.handleDevRegister(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected status %d, got %d, body: %s", http.StatusForbidden, w.Code, w.Body.String())
		}
		var errResp ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("failed to decode error body: %v", err)
		}
		if errResp.Error.Code != "ip_validation_error" {
			t.Errorf("expected error code 'ip_validation_error', got '%s'", errResp.Error.Code)
		}
	})

	t.Run("loopback IP accepted without override", func(t *testing.T) {
		worker := protocol.WorkerInfo{
			ID:   "prod-worker-loopback",
			IP:   "127.0.0.1",
			Port: 8081,
		}
		body, _ := json.Marshal(worker)
		req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.handleDevRegister(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
		}
		got, ok := tr.reg.Get("prod-worker-loopback")
		if !ok {
			t.Fatal("worker not found in registry after registration")
		}
		if got.IP != "127.0.0.1" {
			t.Errorf("expected IP '127.0.0.1', got '%s'", got.IP)
		}
	})
}
func TestWorkerUnavailable(t *testing.T) {
	// Create a mock worker server that only serves /capabilities
	// but the actual endpoint will fail
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			caps := protocol.WorkerInfo{
				ID:       "dead-worker",
				Hostname: "dead-worker",
				IP:       "127.0.0.1",
				Port:     59999,
				Status:   protocol.StatusAvailable,
				Version:  "v1",
				Capabilities: protocol.Capabilities{
					Models: []protocol.ModelInfo{
						{Name: "dead-model", Quantization: "Q4_K_M", Loaded: true},
					},
					VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
				},
			}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}
		// For any other endpoint, return 503 to simulate unreachable
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer workerServer.Close()

	// Parse worker port
	_, portStr, _ := strings.Cut(strings.TrimPrefix(workerServer.URL, "http://"), ":")
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	worker := protocol.WorkerInfo{
		ID:       "dead-worker",
		Hostname: "dead-worker",
		IP:       "127.0.0.1",
		Port:     port,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "dead-model", Quantization: "Q4_K_M", Loaded: true},
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
	_ = server.cache.Update(worker)

	chatReq := protocol.ChatRequest{
		Model: "dead-model",
		Messages: []protocol.ChatMessage{
			{Role: "user", Content: "Hello"},
		},
		Stream: true,
	}

	body, _ := json.Marshal(chatReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleChatCompletions(w, req)

	// Should return 503 since the worker is reachable but returns service unavailable
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
}
func TestQueueStatsEndpoint(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	hub := NewWSHub(tr.reg, testLogger())
	hub.Start(context.Background())
	wsServer := httptest.NewServer(hub.Handler())
	defer wsServer.Close()

	srv := tr.Server()
	srv.hub = hub

	info := protocol.WorkerInfo{
		ID: "stats-worker", Hostname: "stats-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "stats-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
	w := dialFakeWorker(t, wsServer.URL+"/v1/connect")
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	req := httptest.NewRequest(http.MethodGet, "/v1/queue/stats", nil)
	wrec := httptest.NewRecorder()
	srv.handleQueueStats(wrec, req)

	if wrec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusOK, wrec.Code, wrec.Body.String())
	}
	if wrec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json, got %q", wrec.Header().Get("Content-Type"))
	}

	var resp QueueStatsResponse
	if err := json.NewDecoder(wrec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode queue stats: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(resp.Workers))
	}
	if resp.Workers[0].WorkerID != "stats-worker" {
		t.Errorf("expected worker_id stats-worker, got %s", resp.Workers[0].WorkerID)
	}
	if resp.Workers[0].InFlight != 0 {
		t.Errorf("expected in_flight=0, got %d", resp.Workers[0].InFlight)
	}
	if resp.Pool.TotalInFlight != 0 {
		t.Errorf("expected total_in_flight=0, got %d", resp.Pool.TotalInFlight)
	}
	if resp.Pool.QueueTimeP50Ms < 0 {
		t.Errorf("expected p50 >= 0, got %f", resp.Pool.QueueTimeP50Ms)
	}
	if resp.Pool.QueueTimeP95Ms < 0 {
		t.Errorf("expected p95 >= 0, got %f", resp.Pool.QueueTimeP95Ms)
	}
	if resp.Pool.QueueTimeP99Ms < 0 {
		t.Errorf("expected p99 >= 0, got %f", resp.Pool.QueueTimeP99Ms)
	}
}
func TestCallCounter(t *testing.T) {
	counter := NewCallCounter(10 * time.Minute)

	// Record some calls
	counter.Record("model-a")
	counter.Record("model-a")
	counter.Record("model-b")

	snapshot := counter.Snapshot()
	if snapshot["model-a"] != 2 {
		t.Errorf("expected model-a count 2, got %d", snapshot["model-a"])
	}
	if snapshot["model-b"] != 1 {
		t.Errorf("expected model-b count 1, got %d", snapshot["model-b"])
	}

	// Snapshot should return a copy
	snapshot["model-a"] = 999
	snapshot2 := counter.Snapshot()
	if snapshot2["model-a"] != 2 {
		t.Errorf("expected model-a count 2 after copy mutation, got %d", snapshot2["model-a"])
	}
}
func TestWorkersListMethodNotAllowed(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	req := httptest.NewRequest(http.MethodPost, "/v1/workers", nil)
	w := httptest.NewRecorder()
	server.handleWorkersList(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}
func TestQueueStatsEndpointMethodNotAllowed(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	req := httptest.NewRequest(http.MethodPost, "/v1/queue/stats", nil)
	w := httptest.NewRecorder()
	server.handleQueueStats(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// TestHandleConnectionCount_ReturnsWorkerCount verifies the /meta/connections/count
// endpoint returns the worker count from the capability cache.
func TestHandleConnectionCount_ReturnsWorkerCount(t *testing.T) {
	workers := []protocol.WorkerInfo{
		{
			ID:       "worker-1",
			Hostname: "worker-1",
			IP:       "127.0.0.1",
			Port:     8081,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-a", Quantization: "Q4_K_M", Loaded: true},
				},
			},
		},
		{
			ID:       "worker-2",
			Hostname: "worker-2",
			IP:       "127.0.0.1",
			Port:     8082,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "model-b", Quantization: "Q4_K_M", Loaded: true},
				},
			},
		},
	}

	tr := testRegistry(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	// Populate the capability cache
	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	for _, w := range workers {
		server.cache.cache[w.ID] = w
	}
	server.cache.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/meta/connections/count", nil)
	w := httptest.NewRecorder()
	server.handleConnectionCount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp struct {
		Workers int `json:"workers"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Workers != 2 {
		t.Errorf("expected 2 workers, got %d", resp.Workers)
	}
}

// TestWorkerCount_CacheAndRegistryFallback verifies the WorkerCount() method
// works with both cache and registry fallback.
func TestWorkerCount_CacheAndRegistryFallback(t *testing.T) {
	workers := []protocol.WorkerInfo{
		{
			ID:       "worker-1",
			Hostname: "worker-1",
			IP:       "127.0.0.1",
			Port:     8081,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
		},
		{
			ID:       "worker-2",
			Hostname: "worker-2",
			IP:       "127.0.0.1",
			Port:     8082,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
		},
	}

	t.Run("cache_populated_returns_cache_count", func(t *testing.T) {
		tr := testRegistry(t, workers)
		defer tr.cancel()
		defer func() { _ = tr.reg.Stop() }()
		server := tr.Server()

		// Populate the capability cache
		server.cache = NewCapabilityCache(tr.reg, testLogger())
		server.cache.Start(tr.ctx)
		server.cache.mu.Lock()
		for _, w := range workers {
			server.cache.cache[w.ID] = w
		}
		server.cache.mu.Unlock()

		// Cache has 2 workers, registry has 2 workers
		if server.WorkerCount() != 2 {
			t.Errorf("expected 2 workers from cache, got %d", server.WorkerCount())
		}
	})

	t.Run("cache_empty_falls_back_to_registry", func(t *testing.T) {
		tr := testRegistry(t, workers)
		defer tr.cancel()
		defer func() { _ = tr.reg.Stop() }()
		server := tr.Server()

		// Create cache but don't populate it (empty)
		server.cache = NewCapabilityCache(tr.reg, testLogger())
		server.cache.Start(tr.ctx)

		// Cache is empty, should fall back to registry (which has 2 workers)
		if server.WorkerCount() != 2 {
			t.Errorf("expected 2 workers from registry fallback, got %d", server.WorkerCount())
		}
	})

	t.Run("cache_nil_falls_back_to_registry", func(t *testing.T) {
		tr := testRegistry(t, workers)
		defer tr.cancel()
		defer func() { _ = tr.reg.Stop() }()
		server := tr.Server()

		// cache is nil, should fall back to registry
		server.cache = nil

		if server.WorkerCount() != 2 {
			t.Errorf("expected 2 workers from registry fallback (cache=nil), got %d", server.WorkerCount())
		}
	})

	t.Run("no_workers_returns_zero", func(t *testing.T) {
		tr := testRegistry(t, nil)
		defer tr.cancel()
		defer func() { _ = tr.reg.Stop() }()
		server := tr.Server()

		if server.WorkerCount() != 0 {
			t.Errorf("expected 0 workers, got %d", server.WorkerCount())
		}
	})
}
