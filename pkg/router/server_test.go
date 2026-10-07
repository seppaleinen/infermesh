package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// testRegistry creates a registry with a single worker that has a loaded model.
func testRegistry(t *testing.T, workers []protocol.WorkerInfo) *testRegistryImpl {
	regCfg := registry.Defaults()
	regCfg.CheckInterval = 5 * time.Second // 5s minimum
	regCfg.UnavailableTTL = 60 * time.Second
	regCfg.RemoveTTL = 120 * time.Second

	reg, err := registry.New(regCfg, testLogger())
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	for _, w := range workers {
		_ = reg.HandleEvent(protocol.DiscoveryEvent{
			Type:   protocol.EventAdded,
			Worker: w,
		})
	}

	return &testRegistryImpl{
		reg:    reg,
		ctx:    ctx,
		cancel: cancel,
	}
}

type testRegistryImpl struct {
	reg    registry.Registry
	ctx    context.Context
	cancel context.CancelFunc
}

func (tr *testRegistryImpl) Server() *Server {
	return NewServer(tr.reg, testLogger(), ":0", security.Config{DevMode: true})
}

// ServerWithCfg builds a server over the same registry with an explicit
// security config (used to pin prod-mode registration behavior).
func (tr *testRegistryImpl) ServerWithCfg(cfg security.Config) *Server {
	return NewServer(tr.reg, testLogger(), ":0", cfg)
}

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
				var resp ModelsResponse
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

	var resp ModelsResponse
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

// TestChatCompletionsHandlerNoWorkers tests error handling when no workers are available.
func TestChatCompletionsHandlerNoWorkers(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name           string
		statusExpected int
	}{
		{
			name:           "chat completions with no workers",
			statusExpected: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chatReq := ChatRequest{
				Model:  "llama-3-8b",
				Stream: true,
				Messages: []ChatMessage{
					{Role: "user", Content: "Hello"},
				},
			}

			body, err := json.Marshal(chatReq)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.handleChatCompletions(w, req)

			if w.Code != tt.statusExpected {
				t.Errorf("expected status %d, got %d", tt.statusExpected, w.Code)
			}

			if !strings.Contains(w.Body.String(), "no workers") {
				t.Errorf("expected error about no workers, got: %s", w.Body.String())
			}
		})
	}
}

// TestChatCompletionsHandlerInvalidRequest tests invalid request handling.
func TestChatCompletionsHandlerInvalidRequest(t *testing.T) {
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
				{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name         string
		body         string
		statusExpect int
	}{
		{
			name:         "invalid JSON",
			body:         `{invalid json`,
			statusExpect: http.StatusBadRequest,
		},
		{
			name:         "empty body",
			body:         `{}`,
			statusExpect: http.StatusServiceUnavailable, // no model specified, scheduler fails
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.handleChatCompletions(w, req)

			if w.Code != tt.statusExpect {
				t.Errorf("expected status %d, got %d", tt.statusExpect, w.Code)
			}
		})
	}
}

// TestChatCompletionsHandlerMethodNotAllowed tests that non-POST methods are rejected.
func TestChatCompletionsHandlerMethodNotAllowed(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name           string
		method         string
		expectedStatus int
	}{
		{
			name:           "GET not allowed",
			method:         http.MethodGet,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "PUT not allowed",
			method:         http.MethodPut,
			expectedStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			server.handleChatCompletions(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

// TestCompletionsHandlerNoWorkers tests error handling when no workers are available.
func TestCompletionsHandlerNoWorkers(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	completionReq := CompletionRequest{
		Model:     "llama-3-8b",
		Prompt:    "Hello, world!",
		MaxTokens: 100,
		Stream:    true,
	}

	body, err := json.Marshal(completionReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleCompletions(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, w.Code)
	}

	if !strings.Contains(w.Body.String(), "no workers") {
		t.Errorf("expected error about no workers, got: %s", w.Body.String())
	}
}

// TestCompletionsHandlerInvalidRequest tests invalid request handling.
func TestCompletionsHandlerInvalidRequest(t *testing.T) {
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
				{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name         string
		body         string
		statusExpect int
	}{
		{
			name:         "invalid JSON",
			body:         `{invalid json`,
			statusExpect: http.StatusBadRequest,
		},
		{
			name:         "empty body",
			body:         `{}`,
			statusExpect: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.handleCompletions(w, req)

			if w.Code != tt.statusExpect {
				t.Errorf("expected status %d, got %d", tt.statusExpect, w.Code)
			}
		})
	}
}

// TestCompletionsHandlerMethodNotAllowed tests that non-POST methods are rejected.
func TestCompletionsHandlerMethodNotAllowed(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	req := httptest.NewRequest(http.MethodGet, "/v1/completions", nil)
	w := httptest.NewRecorder()
	server.handleCompletions(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// TestChatCompletionsProxiesToWorker tests that the router proxies
// chat completions requests to the selected worker and streams the response.
func TestChatCompletionsProxiesToWorker(t *testing.T) {
	// Create a mock worker server that simulates SSE streaming
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capabilities" {
			// Return capabilities for the mock worker
			w.Header().Set("Content-Type", "application/json")
			caps := protocol.WorkerInfo{
				ID:       "proxy-worker",
				Hostname: "proxy-worker",
				IP:       "127.0.0.1",
				Port:     8081,
				Status:   protocol.StatusAvailable,
				Version:  "v1",
				Capabilities: protocol.Capabilities{
					Models: []protocol.ModelInfo{
						{Name: "test-model", Quantization: "Q4_K_M", Loaded: true},
					},
					VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
				},
			}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}

		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Verify request format
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Send SSE chunks
		for i, msg := range req.Messages {
			chunk := CompletionChunk{
				ID:      "chatcmpl-test-" + string(rune('0'+i)),
				Object:  "chat.completion.chunk",
				Created: 1700000000,
				Model:   req.Model,
				Choices: []Choice{
					{
						Index: i,
						Message: Message{
							Role:    "assistant",
							Content: "Echo: " + msg.Content,
						},
						FinishReason: "stop",
					},
				},
			}

			jsonData, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(jsonData))
			w.(http.Flusher).Flush()
		}

		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer workerServer.Close()

	// Parse worker port
	_, portStr, _ := strings.Cut(strings.TrimPrefix(workerServer.URL, "http://"), ":")
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	worker := protocol.WorkerInfo{
		ID:       "proxy-worker",
		Hostname: "proxy-worker",
		IP:       "127.0.0.1",
		Port:     port,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "test-model", Quantization: "Q4_K_M", Loaded: true},
			},
			VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	defer workerServer.Close()
	server := tr.Server()

	// We need to start the cache to fetch capabilities, but the worker server is a mock
	// So let's directly set the worker in the cache instead
	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)

	// Manually update the cache with capabilities fetched from the mock server
	_ = server.cache.Update(worker)

	chatReq := ChatRequest{
		Model:  "test-model",
		Stream: true,
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello from OpenAI client"},
		},
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected Content-Type 'text/event-stream', got '%s'", w.Header().Get("Content-Type"))
	}

	response := w.Body.String()
	if !strings.Contains(response, "data:") {
		t.Error("response should contain SSE data lines")
	}

	if !strings.Contains(response, "Echo: Hello from OpenAI client") {
		t.Errorf("response should contain echoed message, got: %s", response)
	}

	if !strings.Contains(response, "[DONE]") {
		t.Error("response should contain [DONE] marker")
	}
}

// TestCompletionsProxiesToWorker tests that the router proxies completions
// requests to the selected worker and streams the response.
func TestCompletionsProxiesToWorker(t *testing.T) {
	// Create a mock worker server that simulates SSE streaming
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			caps := protocol.WorkerInfo{
				ID:       "completion-proxy-worker",
				Hostname: "completion-proxy-worker",
				IP:       "127.0.0.1",
				Port:     8081,
				Status:   protocol.StatusAvailable,
				Version:  "v1",
				Capabilities: protocol.Capabilities{
					Models: []protocol.ModelInfo{
						{Name: "completion-model", Quantization: "Q4_K_M", Loaded: true},
					},
					VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
				},
			}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}

		if r.URL.Path != "/v1/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var req CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Send a few chunks
		for i := 0; i < 3; i++ {
			chunk := CompletionChunk{
				ID:      "cmpl-test-" + string(rune('0'+i)),
				Object:  "text_completion",
				Created: 1700000000,
				Model:   req.Model,
				Choices: []Choice{
					{
						Index:        i,
						Text:         "chunk " + string(rune('0'+i)),
						FinishReason: "length",
					},
				},
			}

			jsonData, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(jsonData))
			w.(http.Flusher).Flush()
		}

		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer workerServer.Close()

	// Parse worker port
	_, portStr, _ := strings.Cut(strings.TrimPrefix(workerServer.URL, "http://"), ":")
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	worker := protocol.WorkerInfo{
		ID:       "completion-proxy-worker",
		Hostname: "completion-proxy-worker",
		IP:       "127.0.0.1",
		Port:     port,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "completion-model", Quantization: "Q4_K_M", Loaded: true},
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

	completionReq := CompletionRequest{
		Model:     "completion-model",
		Prompt:    "Once upon a time",
		MaxTokens: 50,
		Stream:    true,
	}

	body, err := json.Marshal(completionReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected Content-Type 'text/event-stream', got '%s'", w.Header().Get("Content-Type"))
	}

	response := w.Body.String()
	if !strings.Contains(response, "data:") {
		t.Error("response should contain SSE data lines")
	}

	if !strings.Contains(response, "[DONE]") {
		t.Error("response should contain [DONE] marker")
	}
}

// TestChatCompletionsNonStreamingProxiesToWorker tests that non-streaming
// chat completions requests are also proxied correctly.
func TestChatCompletionsNonStreamingProxiesToWorker(t *testing.T) {
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			caps := protocol.WorkerInfo{
				ID:       "nosteam-worker",
				Hostname: "nosteam-worker",
				IP:       "127.0.0.1",
				Port:     8081,
				Status:   protocol.StatusAvailable,
				Version:  "v1",
				Capabilities: protocol.Capabilities{
					Models: []protocol.ModelInfo{
						{Name: "nosteam-model", Quantization: "Q4_K_M", Loaded: true},
					},
					VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
				},
			}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}

		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Non-streaming response
		chunk := CompletionChunk{
			ID:      "chatcmpl-nosteam",
			Object:  "chat.completion",
			Created: 1700000000,
			Model:   req.Model,
			Choices: []Choice{
				{
					Index: 0,
					Message: Message{
						Role:    "assistant",
						Content: "This is a non-streaming response",
					},
					FinishReason: "stop",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chunk)
	}))
	defer workerServer.Close()

	_, portStr, _ := strings.Cut(strings.TrimPrefix(workerServer.URL, "http://"), ":")
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	worker := protocol.WorkerInfo{
		ID:       "nosteam-worker",
		Hostname: "nosteam-worker",
		IP:       "127.0.0.1",
		Port:     port,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "nosteam-model", Quantization: "Q4_K_M", Loaded: true},
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

	chatReq := ChatRequest{
		Model:  "nosteam-model",
		Stream: false,
		Messages: []ChatMessage{
			{Role: "user", Content: "Tell me something"},
		},
	}

	body, _ := json.Marshal(chatReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "This is a non-streaming response") {
		t.Errorf("response should contain the worker response, got: %s", w.Body.String())
	}
}

// --- Dev Register Handler Tests ---

// TestDevRegisterValidWorker tests POST with valid WorkerInfo returns 200 and registers worker.
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

// TestDevRegisterEmptyID tests POST with empty ID returns 400.
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

// TestDevRegisterInvalidPort tests POST with out-of-range port returns 400.
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

// TestDevRegisterIPDerivedFromRemoteAddr verifies that in dev mode the
// router derives the worker's routable IP from the request's RemoteAddr
// (authoritative) instead of trusting the client-provided IP. Loopback
// peers (including IPv6 loopback ::1, which a localhost dial can land
// on) are normalized to the canonical 127.0.0.1; other non-IPv4 peer
// addresses are rejected with 400.
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

// TestDevRegisterMethodNotAllowed tests that non-POST methods return 405.
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

// TestDevRegisterMalformedJSON tests POST with malformed JSON body returns 400.
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

// TestDevRegisterForcesAvailableStatus verifies that the handler forces Status=available
// and emits a DiscoveryEvent with EventUpdated type.
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

// TestDevRegisterProdModeIPValidation pins the legacy prod-mode behavior:
// the body-provided IP is used as-is and only loopback is accepted; no
// RemoteAddr override is applied in prod mode.
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

// TestWorkerUnavailable tests that 502 is returned when the worker endpoint is unreachable.
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

	chatReq := ChatRequest{
		Model: "dead-model",
		Messages: []ChatMessage{
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

// TestChatCompletionsRateLimitRejectsAtCap tests that when a worker is at its
// MaxInFlight cap the router returns an OpenAI-shaped 429 BEFORE SSE headers
// are flushed (so the client sees a clean JSON error, not a half-open stream).
func TestChatCompletionsRateLimitRejectsAtCap(t *testing.T) {
	// Use a WebSocket worker with an active hub connection so the first
	// request registers a pending call that never completes (the fake worker
	// never responds), keeping the worker at its cap for the second request.
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	hub := NewWSHub(tr.reg, testLogger())
	hub.SetMaxInFlight(1)
	hub.Start(context.Background())
	wsServer := httptest.NewServer(hub.Handler())
	defer wsServer.Close()

	srv := tr.Server()
	// Swap the server's internal hub for the one we configured above so the
	// HTTP handlers read the same in-flight state the WS connection populates.
	srv.hub = hub

	info := protocol.WorkerInfo{
		ID: "rl-worker", Hostname: "rl-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "rl-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
	w := dialFakeWorker(t, wsServer.URL+"/v1/connect")
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// Populate the capability cache so selectWorker finds the worker.
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(context.Background())
	srv.cache.mu.Lock()
	srv.cache.cache[info.ID] = info
	srv.cache.mu.Unlock()

	chatReq := ChatRequest{
		Model: "rl-model", Stream: true,
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}
	body, _ := json.Marshal(chatReq)

	// First request: should succeed (in_flight=0 < max=1). The fake worker
	// never responds, so the request stays in-flight. Run it in a goroutine
	// with a short timeout so the test doesn't block on the 30s proxyStream
	// deadline.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	req = req.WithContext(ctx1)
	wrec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleChatCompletions(wrec, req)
	}()

	// Wait until the first request has registered its pending call so the
	// second request hits the cap.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn := srv.hub.Client("rl-worker")
		if conn != nil {
			c := conn.(*wsWorkerClient).conn
			if c.workerInFlight() >= 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first request did not register a pending call in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second request: should be rejected with 429 BEFORE SSE headers.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.handleChatCompletions(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: expected status %d, got %d, body: %s", http.StatusTooManyRequests, w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json content type, got %q", w2.Header().Get("Content-Type"))
	}
	if w2.Header().Get("Retry-After") != "1" {
		t.Errorf("expected Retry-After: 1, got %q", w2.Header().Get("Retry-After"))
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w2.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Type != "rate_limit_error" {
		t.Errorf("expected error type rate_limit_error, got %q", errResp.Error.Type)
	}

	// Clean up the first request's goroutine.
	cancel1()
	<-done
}

// TestQueueStatsEndpoint tests the /v1/queue/stats endpoint. It uses a real
// WebSocket worker connection so the hub's connection map (which the endpoint
// reads) has a registered worker.
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

// TestQueueStatsEndpointMethodNotAllowed tests that non-GET methods are rejected.
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

// TestCallCounter tests the rolling window call counter.
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

// TestPopularModelsHandler tests the /meta/models/popular endpoint.
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
			name:           "GET returns popular models sorted by worker_count desc",
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

				// Verify sorted by worker_count desc (all have 1 worker)
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

// TestPopularModelsMultipleWorkers tests worker_count aggregation across multiple workers.
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

	// llama-3-8b has 2 workers, qwen-72b and mistral-7b have 1 each
	// Should be sorted by worker_count desc
	if len(resp.Models) != 3 {
		t.Errorf("expected 3 models, got %d", len(resp.Models))
	}
	if resp.Models[0].Model != "llama-3-8b" || resp.Models[0].WorkerCount != 2 {
		t.Errorf("expected llama-3-8b first with worker_count=2, got %s worker_count=%d", resp.Models[0].Model, resp.Models[0].WorkerCount)
	}
	if resp.Models[0].CallCount != 1 {
		t.Errorf("llama-3-8b call count: expected 1, got %d", resp.Models[0].CallCount)
	}
}

// TestPopularModelsLoadedVsAdvertised tests that loaded_worker_count tracks
// how many workers have the model currently LOADED vs merely advertising it.
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

// TestChatCompletionsTimeoutRecords504 verifies that when proxyStream's
// per-dispatch timeout expires it emits a 504 and forwards it to the worker's
// RecordTimeout hook so /v1/queue/stats surfaces a non-zero
// rejected_504_total. The 504 counter was declared and read back by the stats
// endpoint but never incremented by any code path (issue #54).
func TestChatCompletionsTimeoutRecords504(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	hub := NewWSHub(tr.reg, testLogger())
	hub.SetMaxInFlight(10)
	hub.Start(context.Background())
	wsServer := httptest.NewServer(hub.Handler())
	defer wsServer.Close()

	srv := tr.Server()
	srv.hub = hub
	srv.SetProxyTimeout(200 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "to-worker", Hostname: "to-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "to-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
	// Register the worker in the hub so recordTimeout has a connection to
	// bump the 504 counter against.
	w := dialFakeWorker(t, wsServer.URL+"/v1/connect")
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// A fake client that never produces a chunk and surfaces a timeout error
	// once the context is cancelled — this is exactly the path a stalled
	// worker takes to reach the 504 branch in proxyStream. Its RecordTimeout
	// forwards to the hub's per-worker 504 counter.
	fc := &timeoutClient{workerID: info.ID, hub: hub}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()
	srv.proxyStream(wrec, req, fc, info, "chat", []byte(`{"model":"to-model","messages":[]}`))

	if wrec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusGatewayTimeout, wrec.Code, wrec.Body.String())
	}

	snap := hub.QueueStats()
	if len(snap.Workers) != 1 {
		t.Fatalf("expected 1 worker in stats, got %d", len(snap.Workers))
	}
	if snap.Workers[0].Rejected504Total != 1 {
		t.Errorf("expected rejected_504_total=1, got %d", snap.Workers[0].Rejected504Total)
	}
	if snap.Pool.TotalRejected504 != 1 {
		t.Errorf("expected pool rejected_504=1, got %d", snap.Pool.TotalRejected504)
	}
}

// timeoutClient implements WorkerClient: it never streams a chunk and reports
// a deadline-exceeded error as soon as its context is cancelled, mimicking a
// worker whose backend stalls past the dispatch timeout. Its RecordTimeout
// forwards to the hub's per-worker 504 counter so the stats endpoint
// reflects the timeout.
type timeoutClient struct {
	workerID string
	hub      *WSHub
}

func (c *timeoutClient) Transport() string { return "http" }

func (c *timeoutClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent)
	errCh := make(chan error, 1)
	go func() {
		// Never produces a chunk; fires errTimeout on a short timer so
		// proxyStream's errCh case fires with errors.Is(err, errTimeout)
		// true (the 504 path), before the dispatch context expires.
		select {
		case <-ctx.Done():
			errCh <- fmt.Errorf("%w: %w", errTimeout, ctx.Err())
		case <-time.After(50 * time.Millisecond):
			errCh <- errTimeout
		}
	}()
	return chunkCh, errCh
}

func (c *timeoutClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

func (c *timeoutClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	return false, nil
}

func (c *timeoutClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, nil
}

func (c *timeoutClient) Close() error { return nil }

func (c *timeoutClient) RecordTimeout(workerID string) {
	if c.hub != nil {
		c.hub.recordTimeout(workerID)
	}
}

// scorerTestWorkers returns two identical workers advertising the same model,
// so every static scoring signal ties and only load data can break symmetry.
func scorerTestWorkers() []protocol.WorkerInfo {
	mk := func(id string) protocol.WorkerInfo {
		return protocol.WorkerInfo{
			ID:       id,
			Hostname: id,
			IP:       "127.0.0.1",
			Port:     8081,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "llama-3-8b", Size: 4820000000, Quantization: "Q4_K_M", MaxTokens: 8192, Backend: "llama-cpp", Loaded: true},
				},
				VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
			},
		}
	}
	return []protocol.WorkerInfo{mk("worker-b"), mk("worker-a")}
}

// scorerTestServer builds a server whose capability cache holds two identical
// workers for "llama-3-8b".
func scorerTestServer(t *testing.T) (*testRegistryImpl, *Server) {
	t.Helper()
	workers := scorerTestWorkers()
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	return tr, srv
}

// TestServerScorerWeightsDefault verifies NewServer starts with the built-in
// weighted scorer and its default weights.
func TestServerScorerWeightsDefault(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	w := srv.ScorerWeights()
	want := schedulerWeights{QuantMatch: 0.40, VRAMFree: 0.25, GPUUtil: 0.15, QueueDepth: 0.10, Latency: 0.10, MaxQueueDepth: 10}
	if w != want {
		t.Errorf("default weights: got %+v, want %+v", w, want)
	}
	if srv.scorer == nil || srv.scorer.Name() != "weighted" {
		t.Fatalf("expected non-nil weighted scorer, got %v", srv.scorer)
	}
}

// TestServerSetScorerWeightsZeroIsNoOp verifies an all-zero call keeps the
// current weights (built-in defaults on a fresh server).
func TestServerSetScorerWeightsZeroIsNoOp(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	before := srv.ScorerWeights()
	srv.SetScorerWeights(0, 0, 0, 0, 0, 0)
	if after := srv.ScorerWeights(); after != before {
		t.Errorf("all-zero SetScorerWeights changed weights: got %+v, want %+v", after, before)
	}
}

// TestServerSetScorerWeightsPartialZero verifies zero entries keep the
// previously configured values while non-zero entries are applied.
func TestServerSetScorerWeightsPartialZero(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	srv.SetScorerWeights(0.5, 0.3, 0.1, 0.05, 0.05, 20)
	w := srv.ScorerWeights()
	wantCustom := schedulerWeights{QuantMatch: 0.5, VRAMFree: 0.3, GPUUtil: 0.1, QueueDepth: 0.05, Latency: 0.05, MaxQueueDepth: 20}
	if w != wantCustom {
		t.Fatalf("custom weights not applied: got %+v, want %+v", w, wantCustom)
	}

	srv.SetScorerWeights(0, 0.6, 0, 0.2, 0, 0)
	w = srv.ScorerWeights()
	want := schedulerWeights{QuantMatch: 0.5, VRAMFree: 0.6, GPUUtil: 0.1, QueueDepth: 0.2, Latency: 0.05, MaxQueueDepth: 20}
	if w != want {
		t.Errorf("partial-zero weights: got %+v, want %+v", w, want)
	}
}

// TestSelectWorkerDeterminism verifies repeated selection over identical
// candidates always returns the same worker (lowest ID on ties).
func TestSelectWorkerDeterminism(t *testing.T) {
	tr, srv := scorerTestServer(t)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	first, err := srv.selectWorker("llama-3-8b")
	if err != nil {
		t.Fatalf("selectWorker: %v", err)
	}
	for i := 0; i < 25; i++ {
		w, err := srv.selectWorker("llama-3-8b")
		if err != nil {
			t.Fatalf("selectWorker (iter %d): %v", i, err)
		}
		if w.ID != first.ID {
			t.Fatalf("selection not deterministic: got %s, previously %s", w.ID, first.ID)
		}
	}
	if first.ID != "worker-a" {
		t.Errorf("expected lowest-ID worker to win ties, got %s", first.ID)
	}
}

// TestSelectWorkerRespectsQueueDepth verifies a worker with queued load is
// scored below an idle worker and loses selection.
func TestSelectWorkerRespectsQueueDepth(t *testing.T) {
	tr, srv := scorerTestServer(t)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	workers := scorerTestWorkers()
	busy, idle := workers[0], workers[1] // worker-b (queued), worker-a (idle)

	load := map[string]WorkerQueueStats{
		busy.ID: {WorkerID: busy.ID, QueueDepth: 8, InFlight: 4, AvgWaitMs: 400},
	}
	srv.loadSnapshot = func() map[string]WorkerQueueStats { return load }

	w, err := srv.selectWorker("llama-3-8b")
	if err != nil {
		t.Fatalf("selectWorker: %v", err)
	}
	if w.ID != idle.ID {
		t.Errorf("expected idle worker to be selected over queued worker, got %s", w.ID)
	}

	selBusy, err := srv.scoreCandidates("llama-3-8b", []protocol.WorkerInfo{busy}, load)
	if err != nil {
		t.Fatalf("scoreCandidates (busy): %v", err)
	}
	selIdle, err := srv.scoreCandidates("llama-3-8b", []protocol.WorkerInfo{idle}, load)
	if err != nil {
		t.Fatalf("scoreCandidates (idle): %v", err)
	}
	if selBusy.Score >= selIdle.Score {
		t.Errorf("expected queued worker to score lower: busy %.4f vs idle %.4f", selBusy.Score, selIdle.Score)
	}
}

// mockWorkerClient is a WorkerClient implementation that returns configured
// responses for testing dispatchWithFailover.
type mockWorkerClient struct {
	mu      sync.Mutex
	results map[string]mockResult
	calls   map[string]int
}

type mockResult struct {
	body  []byte
	err   error
	block time.Duration
}

func newMockWorkerClient() *mockWorkerClient {
	return &mockWorkerClient{
		results: make(map[string]mockResult),
		calls:   make(map[string]int),
	}
}

func (c *mockWorkerClient) Transport() string { return "http" }

func (c *mockWorkerClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	c.mu.Lock()
	c.calls[worker.ID]++
	r, ok := c.results[worker.ID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no mock response for worker %s", worker.ID)
	}
	if r.block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.block):
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte(nil), r.body...), nil
}

func (c *mockWorkerClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent)
	errCh := make(chan error, 1)
	close(ch)
	return ch, errCh
}

func (c *mockWorkerClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	return false, nil
}

func (c *mockWorkerClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, nil
}

func (c *mockWorkerClient) Close() error { return nil }

func (c *mockWorkerClient) RecordTimeout(workerID string) {}

// failoverTestServer sets up a server with two workers in the cache and a
// mockWorkerClient that returns configured responses per worker ID.
func failoverTestServer(t *testing.T, workers []protocol.WorkerInfo) (*testRegistryImpl, *Server, *mockWorkerClient) {
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	fc := newMockWorkerClient()
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }
	return tr, srv, fc
}

func mkWorker(id, model string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID: id, Hostname: id, IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{{Name: model, Quantization: "Q4_K_M", Loaded: true}},
			VRAM:   protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}
}

// TestDispatchWithFailover_SuccessOnFirstAttempt verifies the failover loop
// succeeds on the first attempt when the primary worker returns a valid
// response.
func TestDispatchWithFailover_SuccessOnFirstAttempt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"success"}}]}`),
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"should-not-be-used"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("dispatchWithFailover failed: %v", err)
	}
	if !bytes.Contains(resp, []byte("success")) {
		t.Errorf("expected response from worker-a, got: %s", string(resp))
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call to worker-a, got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 0 {
		t.Errorf("expected 0 calls to worker-b, got %d", fc.calls["worker-b"])
	}
}

// TestDispatchWithFailover_SuccessOnSecondAttempt verifies failover to the
// second worker when the first returns a retryable error.
func TestDispatchWithFailover_SuccessOnSecondAttempt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"success"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("dispatchWithFailover failed: %v", err)
	}
	if !bytes.Contains(resp, []byte("success")) {
		t.Errorf("expected response from worker-b, got: %s", string(resp))
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call to worker-a, got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to worker-b, got %d", fc.calls["worker-b"])
	}
}

// TestDispatchWithFailover_ExhaustedAttempts verifies the loop exhausts
// all attempts when all workers return retryable errors.
func TestDispatchWithFailover_ExhaustedAttempts(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-b"},
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 2, 10*time.Second)
	if err == nil {
		t.Fatal("expected error after exhausted attempts")
	}
	if fc.calls["worker-a"] != 1 || fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to each worker, got a=%d b=%d", fc.calls["worker-a"], fc.calls["worker-b"])
	}
}

// TestDispatchWithFailover_NonRetryableErrorStops verifies a non-retryable
// error (e.g., 400 Bad Request) fails fast without trying the next worker.
func TestDispatchWithFailover_NonRetryableErrorStops(t *testing.T) {
	tr, srv, fc := failoverTestServer(t, []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 400, Body: "bad request", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"should-not-be-used"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err == nil {
		t.Fatal("expected error for non-retryable 400")
	}
	var herr *WorkerHTTPError
	if !errors.As(err, &herr) || herr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected WorkerHTTPError 400, got %v", err)
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call (fail-fast), got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 0 {
		t.Errorf("expected 0 calls to worker-b, got %d", fc.calls["worker-b"])
	}
}

// TestSelectWorkerExcluding_FiltersExcluded_FiltersExcluded verifies that the exclusion
// set filters out workers from selection.
func TestSelectWorkerExcluding_FiltersExcluded(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, _ := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Exclude worker-a, should select worker-b
	w, err := srv.selectWorkerExcluding("failover-model", map[string]struct{}{"worker-a": {}})
	if err != nil {
		t.Fatalf("selectWorkerExcluding: %v", err)
	}
	if w.ID != "worker-b" {
		t.Errorf("expected worker-b, got %s", w.ID)
	}

	// Exclude both workers - should return error
	_, err = srv.selectWorkerExcluding("failover-model", map[string]struct{}{
		"worker-a": {},
		"worker-b": {},
	})
	if err == nil {
		t.Fatal("expected error when all workers excluded")
	}
	if !errors.Is(err, scheduler.ErrNoMatchingWorkers) {
		t.Errorf("expected ErrNoMatchingWorkers, got %v", err)
	}
}

// TestIsRetryableError_Taxonomy verifies the retryable error taxonomy.
func TestIsRetryableError_Taxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", errTimeout, true},
		{"connClosed", errConnClosed, true},
		{"relayDisconnected", errRelayDisconnected, true},
		{"rateLimited", errRateLimited, true},
		{"deadlineExceeded", context.DeadlineExceeded, true},
		{"worker500", &WorkerHTTPError{StatusCode: 500}, true},
		{"worker503", &WorkerHTTPError{StatusCode: 503}, true},
		{"worker429", &WorkerHTTPError{StatusCode: 429}, true},
		{"worker400", &WorkerHTTPError{StatusCode: 400}, false},
		{"worker404", &WorkerHTTPError{StatusCode: 404}, false},
		{"noMatchingWorkers", scheduler.ErrNoMatchingWorkers, false},
		{"allWorkersUnavailable", scheduler.ErrAllWorkersUnavailable, false},
		{"canceled", context.Canceled, false},
		{"parseError", fmt.Errorf("parse error"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.want {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRetryBudget_DeadlineEnforced verifies the retry budget deadline is
// enforced across failover attempts.
func TestRetryBudget_DeadlineEnforced(t *testing.T) {
	tr, srv, fc := failoverTestServer(t, []protocol.WorkerInfo{
		mkWorker("worker-a", "budget-model"),
	})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Mock worker that blocks until context is cancelled, then returns
	// context.DeadlineExceeded.
	fc.results["worker-a"] = mockResult{
		block: 200 * time.Millisecond,
	}

	body := []byte(`{"model":"budget-model","messages":[{"role":"user","content":"hi"}]}`)
	start := time.Now()
	_, err := srv.dispatchWithFailover(context.Background(), "budget-model", body, "chat", 3, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error due to budget deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errAllAttemptsFailed) {
		t.Errorf("expected deadline/timeout error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("budget not enforced: took %v", elapsed)
	}
}

// TestWrapInferenceEndpointsDevModeAPIKey pins the issue #77 fix: an
// api-key configured in dev mode must still gate the inference endpoints.
// Previously the router only installed the API-key middleware inside the
// prod-only block, so --dev-mode --api-key accepted any key.
func TestWrapInferenceEndpointsDevModeAPIKey(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Dev mode with an api-key configured.
	server := tr.ServerWithCfg(security.Config{DevMode: true, APIKey: "heybaby"})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	wrapped := server.wrapInferenceEndpoints(next, "heybaby")

	tests := []struct {
		name           string
		path           string
		header         string
		expectedStatus int
	}{
		{"inference endpoint missing key", "/v1/chat/completions", "", http.StatusUnauthorized},
		{"inference endpoint wrong key", "/v1/chat/completions", "wrong", http.StatusUnauthorized},
		{"inference endpoint valid key", "/v1/chat/completions", "heybaby", http.StatusOK},
		{"completion endpoint valid key", "/v1/completions", "heybaby", http.StatusOK},
		{"non-inference endpoint passes through", "/v1/models", "", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("X-API-Key", tt.header)
			}
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d, body: %s", tt.expectedStatus, w.Code, w.Body.String())
			}
		})
	}
}

// TestWrapInferenceEndpointsNoKeyDevMode verifies that without an api-key
// configured, dev mode passes through unauthenticated (backward compatible).
func TestWrapInferenceEndpointsNoKeyDevMode(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	server := tr.Server() // dev mode, no api-key
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := server.wrapInferenceEndpoints(next, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected pass-through 200, got %d", w.Code)
	}
}

// TestConnStateCounter verifies the atomic connection counter increments on
// http.StateNew and decrements on http.StateClosed. Keep-alive (StateIdle) and
// hijacked (StateHijacked) transitions are neutral so the count reflects live
// sockets, not in-flight requests. This is a unit test that simulates the
// ConnState callbacks directly.
func TestConnStateCounter(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	// Attach the same ConnState hook Start() installs so the test drives the
	// exact production code path without binding a socket.
	server.server = &http.Server{}
	server.server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			server.connections.Add(1)
		case http.StateClosed:
			server.connections.Add(-1)
		}
	}

	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected initial count 0, got %d", got)
	}

	server.server.ConnState(nil, http.StateNew)
	server.server.ConnState(nil, http.StateNew)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count 2 after two opens, got %d", got)
	}

	// Keep-alive and hijack transitions must not change the count.
	server.server.ConnState(nil, http.StateIdle)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count unchanged (2) after StateIdle, got %d", got)
	}
	server.server.ConnState(nil, http.StateHijacked)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count unchanged (2) after StateHijacked, got %d", got)
	}

	server.server.ConnState(nil, http.StateClosed)
	if got := server.connections.Load(); got != 1 {
		t.Errorf("expected count 1 after one close, got %d", got)
	}

	server.server.ConnState(nil, http.StateClosed)
	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected count 0 after closing remaining connection, got %d", got)
	}
}

// TestConnectionCountEndpointIntegration verifies GET /meta/connections/count works
// end-to-end through a real HTTP server and that the ConnState counter tracks
// live TCP connections. It uses the production handleConnectionCount handler and
// the production ConnState hook.
func TestConnectionCountEndpointIntegration(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	server := tr.Server()

	// Build a mux with the production handler. mTLS/API-key wrapping is dev
	// mode-only in this test, so a plain mux mirrors the behaviour exercised
	// by the real server.
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/connections/count", server.handleConnectionCount)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	server.server = &http.Server{Handler: mux}
	server.server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			server.connections.Add(1)
		case http.StateClosed:
			server.connections.Add(-1)
		}
	}

	go func() { _ = server.server.Serve(ln) }()
	defer func() { _ = server.server.Close() }()

	addr := ln.Addr().String()
	url := "http://" + addr + "/meta/connections/count"

	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected initial connection count 0, got %d", got)
	}

	// GET should return 200 with JSON payload "clients": >=0.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /meta/connections/count: %v", err)
	}
	var body struct{ Connections int `json:"connections"` }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if body.Connections < 0 {
		t.Errorf("expected connections >= 0, got %d", body.Connections)
	}

	// After a request with keep-alive the server should have an idle connection.
	if got := server.connections.Load(); got != 1 {
		t.Errorf("expected connection count 1 after request (keep-alive), got %d", got)
	}

	// POST method must be rejected with 405 Method Not Allowed.
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new POST request: %v", err)
	}
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /meta/connections/count: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for POST, got %d", resp2.StatusCode)
	}

	// Close idle connections so StateClosed callbacks fire.
	if trpt, ok := http.DefaultTransport.(*http.Transport); ok {
		trpt.CloseIdleConnections()
	}

	// Poll until the counter returns to zero or timeout to avoid flakiness.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.connections.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected connection count 0 after closing idle connections, got %d", got)
	}
}
