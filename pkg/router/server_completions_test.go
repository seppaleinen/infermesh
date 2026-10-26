package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestCompletionsHandlerNoWorkers tests error handling when no workers are available.
func TestCompletionsHandlerNoWorkers(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	completionReq := protocol.CompletionRequest{
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
		t.Errorf("expected 503, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), "no workers") {
		t.Errorf("expected error about no workers, got: %s", w.Body.String())
	}
}

// TestCompletionsHandlerInvalidRequest tests invalid request body handling.
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
			statusExpect: http.StatusServiceUnavailable, // no model specified, scheduler fails
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
			req := httptest.NewRequest(tt.method, "/v1/completions", nil)
			w := httptest.NewRecorder()
			server.handleCompletions(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

// TestCompletionsProxiesToWorker tests non-streaming completions proxy.
func TestCompletionsProxiesToWorker(t *testing.T) {
	worker := protocol.WorkerInfo{
		ID:       "comp-stream-worker",
		Hostname: "comp-stream-worker",
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

	fc.results["comp-stream-worker"] = mockResult{
		body: []byte(`{"id":"cmpl-1","object":"text_completion","choices":[{"text":" completion"}]}`),
	}

	compReq := protocol.CompletionRequest{
		Model:  "llama-3-8b",
		Prompt: "Hello",
		Stream: false,
	}

	body, err := json.Marshal(compReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.handleCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}