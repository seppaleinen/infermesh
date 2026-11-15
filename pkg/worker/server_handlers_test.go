package worker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestHealthHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})

	tests := []struct {
		name           string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "health check success",
			expectedStatus: http.StatusOK,
			expectedBody:   "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/health", nil)
			server.health(w, r)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if w.Body.String() != tt.expectedBody {
				t.Errorf("expected body '%s', got '%s'", tt.expectedBody, w.Body.String())
			}
		})
	}
}
func TestCapabilitiesHandler(t *testing.T) {
	// Create a temp model config file
	tmpFile, err := os.CreateTemp("", "models-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	yamlContent := `
models:
  - name: "model-1"
    size_bytes: 1000000
    quantization: "Q4_K_M"
    max_tokens: 8192
    backend: "llama-cpp"
    loaded: false
`
	if _, err := tmpFile.WriteString(yamlContent); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	cfg := capabilities.Defaults()
	cfg.ModelConfigPath = tmpFile.Name()
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	// Override the capabilities aggregator with one that uses the test config
	server.capabilities = capabilities.NewAggregator(cfg, testLogger())

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "capabilities with models",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/capabilities", nil)
			server.handleCapabilities(w, r)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			var worker protocol.WorkerInfo
			err := json.NewDecoder(w.Body).Decode(&worker)
			if err != nil {
				t.Errorf("failed to decode response: %v", err)
			}

			if worker.ID != "worker-1" {
				t.Errorf("expected worker ID 'worker-1', got '%s'", worker.ID)
			}

			if len(worker.Capabilities.Models) != 1 {
				t.Errorf("expected 1 model, got %d", len(worker.Capabilities.Models))
			}
		})
	}
}
func TestCapabilitiesHandler_IncludesModelFilters(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetModelFilters(
		[]string{"llama3-8b", "mistral-7b"},
		[]string{"gpt-4"},
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/capabilities", nil)
	server.handleCapabilities(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(w.Body).Decode(&worker); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(worker.Capabilities.AllowedModels) != 2 {
		t.Errorf("expected 2 allowed models, got %d: %v", len(worker.Capabilities.AllowedModels), worker.Capabilities.AllowedModels)
	}
	if worker.Capabilities.AllowedModels[0] != "llama3-8b" || worker.Capabilities.AllowedModels[1] != "mistral-7b" {
		t.Errorf("allowed models = %v", worker.Capabilities.AllowedModels)
	}
	if len(worker.Capabilities.ExcludedModels) != 1 {
		t.Errorf("expected 1 excluded model, got %d: %v", len(worker.Capabilities.ExcludedModels), worker.Capabilities.ExcludedModels)
	}
	if worker.Capabilities.ExcludedModels[0] != "gpt-4" {
		t.Errorf("excluded models = %v", worker.Capabilities.ExcludedModels)
	}
}
func TestSetBackend_SeesConcurrencyEstimatorWithThreads(t *testing.T) {
	// The estimator persists to /tmp/infermesh-concurrency-cache.json, so
	// isolate each subtest by clearing the persisted cap first. A persisted
	// cap from a previous run overrides the seed (the dynamic part of #139);
	// with no persisted value the seed wins.
	persistPath := "/tmp/infermesh-concurrency-cache.json"
	defer func() { _ = os.Remove(persistPath) }()
	_ = os.Remove(persistPath)

	tests := []struct {
		name    string
		threads int
		wantCap int
	}{
		{"llama-cpp with 8 threads", 8, 8},
		{"llama-cpp with 1 thread", 1, 1},
		{"backend without thread concept", 0, defaultInitCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(persistPath) // isolate from previous subtest
			server := NewServer(testLogger(), "", security.Config{DevMode: true})
			backend := newMockBackend()
			backend.SetThreads(tt.threads)
			server.SetBackend(backend, "test-model")

			if server.concurrencyEstimator == nil {
				t.Fatal("expected concurrency estimator to be initialized")
			}
			if got := server.concurrencyEstimator.Cap(); got != tt.wantCap {
				t.Errorf("estimator cap = %d, want %d", got, tt.wantCap)
			}
		})
	}
}
func TestCapabilitiesHandler_ReportsThreads(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	backend := newMockBackend()
	backend.SetThreads(8)
	server.SetBackend(backend, "test-model")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/capabilities", nil)
	server.handleCapabilities(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(w.Body).Decode(&worker); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if worker.Capabilities.Threads != 8 {
		t.Errorf("expected Threads=8, got %d", worker.Capabilities.Threads)
	}
}
func TestCapabilitiesHandler_NoThreadsWhenUnset(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "test-model")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/capabilities", nil)
	server.handleCapabilities(w, r)

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(w.Body).Decode(&worker); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if worker.Capabilities.Threads != 0 {
		t.Errorf("expected Threads=0 when unset, got %d", worker.Capabilities.Threads)
	}
}
func TestCapabilitiesHandler_NoFiltersWhenUnset(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/capabilities", nil)
	server.handleCapabilities(w, r)

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(w.Body).Decode(&worker); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(worker.Capabilities.AllowedModels) != 0 {
		t.Errorf("expected no allowed models, got %v", worker.Capabilities.AllowedModels)
	}
	if len(worker.Capabilities.ExcludedModels) != 0 {
		t.Errorf("expected no excluded models, got %v", worker.Capabilities.ExcludedModels)
	}
}
func TestMetricsHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.models = []protocol.ModelInfo{{Name: "model-1"}}

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "metrics with models",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/metrics", nil)
			server.metrics(w, r)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if !strings.Contains(w.Body.String(), "models_total") {
				t.Errorf("response should contain 'models_total'")
			}

			if !strings.Contains(w.Body.String(), "workers_total") {
				t.Errorf("response should contain 'workers_total'")
			}
		})
	}
}
func TestModelsListHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.models = []protocol.ModelInfo{{Name: "model-1", Size: 1000000}}

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "models list with data",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/v1/models", nil)
			server.modelsList(w, r)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			var response protocol.ModelsResponse
			err := json.NewDecoder(w.Body).Decode(&response)
			if err != nil {
				t.Errorf("failed to decode response: %v", err)
			}

			if response.Object != "list" {
				t.Errorf("expected 'list', got '%s'", response.Object)
			}

			if len(response.Data) != 1 {
				t.Errorf("expected 1 model, got %d", len(response.Data))
			}
		})
	}
}
func TestChatCompletionsHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "chat completions with SSE",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chatReq := protocol.ChatRequest{
				Model:    "gpt-4",
				Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
				Stream:   true,
			}

			body, err := json.Marshal(chatReq)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.chatCompletions(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if w.Header().Get("Content-Type") != "text/event-stream" {
				t.Errorf("expected Content-Type 'text/event-stream', got '%s'", w.Header().Get("Content-Type"))
			}

			if !strings.Contains(w.Body.String(), "data:") {
				t.Errorf("SSE response should contain 'data:'")
			}
		})
	}
}
func TestChatCompletionsHandler_NonStreaming(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	chatReq := protocol.ChatRequest{
		Model:    "gpt-4",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
		Stream:   false,
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.chatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "data:") {
		t.Errorf("non-streaming response must not contain SSE framing, got: %s", respBody)
	}

	var chatResp protocol.ChatResponse
	if err := json.Unmarshal([]byte(respBody), &chatResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if chatResp.ID != "chatcmpl-test" {
		t.Errorf("expected ID 'chatcmpl-test', got '%s'", chatResp.ID)
	}
	if len(chatResp.Choices) != 1 || chatResp.Choices[0].Message.Content != "Mock response" {
		t.Errorf("unexpected choices: %+v", chatResp.Choices)
	}
}
func TestChatCompletionsHandler_StreamingChunkFormat(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	chatReq := protocol.ChatRequest{
		Model:    "gpt-4",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
		Stream:   true,
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.chatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	respBody := w.Body.String()
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Errorf("streaming response must end with 'data: [DONE]', got: %s", respBody)
	}

	// Every non-[DONE] data frame must decode as a protocol.ChatChunk.
	frames := strings.Split(strings.TrimRight(respBody, "\n"), "\n\n")
	foundChunk := false
	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk protocol.ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("SSE frame is not valid protocol.ChatChunk JSON: %v\nframe: %s", err, payload)
		}
		foundChunk = true
	}
	if !foundChunk {
		t.Errorf("expected at least one chunk frame, got: %s", respBody)
	}
}

// TestChatCompletionsHandler_NonStreamingReasoningUsage verifies that a
// non-streaming backend response carrying usage.completion_tokens_details
// and prompt_tokens_details survives the worker's HTTP handler
// unmarshal→marshal round-trip and reaches the client. Reasoning models
// (LM Studio gpt-oss-20b, DeepSeek-R1-style backends) report reasoning tokens
// here; without TokenDetails fields the worker's json.Unmarshal discards them
// and the client cannot see reasoning-token cost (issue #204 follow-up).
func TestChatCompletionsHandler_NonStreamingReasoningUsage(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})

	reasoning := 3
	cached := 4
	server.SetBackend(func() *mockBackend {
		m := newMockBackend()
		m.SetUsage(&protocol.Usage{
			PromptTokens:            10,
			CompletionTokens:        5,
			TotalTokens:             15,
			PromptTokensDetails:     &protocol.TokenDetails{CachedTokens: cached},
			CompletionTokensDetails: &protocol.TokenDetails{ReasoningTokens: reasoning},
		})
		return m
	}(), "gpt-4")

	chatReq := protocol.ChatRequest{
		Model:    "gpt-4",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
		Stream:   false,
	}
	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.chatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var chatResp protocol.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if chatResp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if chatResp.Usage.CompletionTokensDetails == nil || chatResp.Usage.CompletionTokensDetails.ReasoningTokens != reasoning {
		t.Errorf("completion_tokens_details.reasoning_tokens lost: %+v", chatResp.Usage.CompletionTokensDetails)
	}
	if chatResp.Usage.PromptTokensDetails == nil || chatResp.Usage.PromptTokensDetails.CachedTokens != cached {
		t.Errorf("prompt_tokens_details.cached_tokens lost: %+v", chatResp.Usage.PromptTokensDetails)
	}

	// Re-marshal the handler's wire payload to prove the details survive to
	// the client (the router re-marshals this exact body).
	out, err := json.Marshal(chatResp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	usage := check["usage"].(map[string]any)
	if d, ok := usage["completion_tokens_details"].(map[string]any); !ok || d["reasoning_tokens"] != float64(reasoning) {
		t.Errorf("round-trip lost completion_tokens_details: %v", usage["completion_tokens_details"])
	}
	if d, ok := usage["prompt_tokens_details"].(map[string]any); !ok || d["cached_tokens"] != float64(cached) {
		t.Errorf("round-trip lost prompt_tokens_details: %v", usage["prompt_tokens_details"])
	}
}

// TestCompletionsHandler_NonStreamingReasoningUsage mirrors the chat handler
// test for the /v1/completions endpoint.
func TestCompletionsHandler_NonStreamingReasoningUsage(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})

	reasoning := 7
	cached := 2
	server.SetBackend(func() *mockBackend {
		m := newMockBackend()
		m.SetUsage(&protocol.Usage{
			PromptTokens:            8,
			CompletionTokens:        6,
			TotalTokens:             14,
			PromptTokensDetails:     &protocol.TokenDetails{CachedTokens: cached},
			CompletionTokensDetails: &protocol.TokenDetails{ReasoningTokens: reasoning},
		})
		return m
	}(), "gpt-4")

	completionReq := protocol.CompletionRequest{
		Model:     "gpt-4",
		Prompt:    "Hello",
		MaxTokens: 100,
		Stream:    false,
	}
	body, err := json.Marshal(completionReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.completions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var compResp protocol.CompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &compResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if compResp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if compResp.Usage.CompletionTokensDetails == nil || compResp.Usage.CompletionTokensDetails.ReasoningTokens != reasoning {
		t.Errorf("completion_tokens_details.reasoning_tokens lost: %+v", compResp.Usage.CompletionTokensDetails)
	}
	if compResp.Usage.PromptTokensDetails == nil || compResp.Usage.PromptTokensDetails.CachedTokens != cached {
		t.Errorf("prompt_tokens_details.cached_tokens lost: %+v", compResp.Usage.PromptTokensDetails)
	}
}
