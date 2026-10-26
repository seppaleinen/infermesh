package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

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
			chatReq := protocol.ChatRequest{
				Model:  "llama-3-8b",
				Stream: true,
				Messages: []protocol.ChatMessage{
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
		})
	}
}

// TestChatCompletionsHandlerInvalidRequest tests invalid request body handling.
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

// TestChatCompletionsProxiesToWorker tests streaming chat completions proxy.
func TestChatCompletionsProxiesToWorker(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "stream-worker",
		Hostname: "stream-worker",
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

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker.ID] = worker
	server.cache.mu.Unlock()

	fc := newMockWorkerClient()
	server.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }

	fc.results["stream-worker"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"Hello, world!"}}]}`),
	}

	chatReq := protocol.ChatRequest{
		Model:  "llama-3-8b",
		Stream: true,
		Messages: []protocol.ChatMessage{
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

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChatCompletionsNonStreamingProxiesToWorker tests non-streaming chat completions proxy.
func TestChatCompletionsNonStreamingProxiesToWorker(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "nonstream-worker",
		Hostname: "nonstream-worker",
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

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	server.cache = NewCapabilityCache(tr.reg, testLogger())
	server.cache.Start(tr.ctx)
	server.cache.mu.Lock()
	server.cache.cache[worker.ID] = worker
	server.cache.mu.Unlock()

	fc := newMockWorkerClient()
	server.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }

	fc.results["nonstream-worker"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"non-streaming response"}}]}`),
	}

	chatReq := protocol.ChatRequest{
		Model:  "llama-3-8b",
		Stream: false,
		Messages: []protocol.ChatMessage{
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

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}