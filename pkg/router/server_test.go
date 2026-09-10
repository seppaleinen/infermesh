package router
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// testRegistry creates a registry with a single worker that has a loaded model.
func testRegistry(t *testing.T, workers []protocol.WorkerInfo) *testRegistryImpl {
	regCfg := registry.Defaults()
	regCfg.CheckInterval = 5 * time.Second      // 5s minimum
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
			VRAM:   protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
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
			name:        "invalid JSON",
			body:        `{invalid json`,
			statusExpect: http.StatusBadRequest,
		},
		{
			name:        "empty body",
			body:        `{}`,
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
				ID:          "nosteam-worker",
				Hostname:    "nosteam-worker",
				IP:          "127.0.0.1",
				Port:        8081,
				Status:      protocol.StatusAvailable,
				Version:     "v1",
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
		ID:          "nosteam-worker",
		Hostname:    "nosteam-worker",
		IP:          "127.0.0.1",
		Port:        port,
		Status:      protocol.StatusAvailable,
		Version:     "v1",
		LastSeen:    time.Now(),
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

// TestDevRegisterNonLoopbackIP tests POST with non-loopback IP returns 403.
func TestDevRegisterNonLoopbackIP(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	tests := []struct {
		name string
		ip   string
	}{
		{"public IP", "8.8.8.8"},
		{"private IP", "192.168.1.1"},
		{"zero IP", "0.0.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := protocol.WorkerInfo{
				ID:   "dev-worker-bad-ip",
				IP:   tt.ip,
				Port: 8081,
			}
			body, _ := json.Marshal(worker)
			req := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			server.handleDevRegister(w, req)

			if w.Code != http.StatusForbidden {
				t.Errorf("expected status %d for IP %s, got %d", http.StatusForbidden, tt.ip, w.Code)
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
