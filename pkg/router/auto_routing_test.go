package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func TestAutoRouting_DisabledByDefault(t *testing.T) {
	// Worker with NO "auto" model - so literal "auto" fails
	workers := []protocol.WorkerInfo{mkWorker("worker-1", "other-model")}
	srv, _, tr := setupAutoRoutingHandler(t, workers, nil)
	defer func() { _ = tr.reg.Stop() }()

	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no workers") {
		t.Errorf("expected no_workers error, got: %s", w.Body.String())
	}
}
func TestAutoRouting_SimplePrompt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-small"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"small"}}]}`),
	}

	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("small")) {
		t.Errorf("expected response from small-model, got: %s", w.Body.String())
	}
	if fc.calls["worker-small"] != 1 {
		t.Errorf("expected 1 call to worker-small, got %d", fc.calls["worker-small"])
	}
}
func TestAutoRouting_MediumPrompt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
		mkWorker("worker-medium", "medium-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-medium"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"medium"}}]}`),
	}

	// ~300 token prompt
	longPrompt := string(make([]byte, 1200))
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: longPrompt}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("medium")) {
		t.Errorf("expected response from medium-model, got: %s", w.Body.String())
	}
	if fc.calls["worker-medium"] != 1 {
		t.Errorf("expected 1 call to worker-medium, got %d", fc.calls["worker-medium"])
	}
}
func TestAutoRouting_ComplexPrompt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
		mkWorker("worker-medium", "medium-model"),
		mkWorker("worker-large", "large-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-large"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"large"}}]}`),
	}

	// ~800 token prompt
	longPrompt := string(make([]byte, 3200))
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: longPrompt}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("large")) {
		t.Errorf("expected response from large-model, got: %s", w.Body.String())
	}
	if fc.calls["worker-large"] != 1 {
		t.Errorf("expected 1 call to worker-large, got %d", fc.calls["worker-large"])
	}
}
func TestAutoRouting_ReasoningKeyword(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
		mkWorker("worker-medium", "medium-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
		ReasoningKeywords: []string{"think", "reason", "step by step"},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-medium"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"medium"}}]}`),
	}

	// Short prompt with "think" keyword -> TierMedium
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Please think about this"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("medium")) {
		t.Errorf("expected response from medium-model due to keyword, got: %s", w.Body.String())
	}
	if fc.calls["worker-medium"] != 1 {
		t.Errorf("expected 1 call to worker-medium, got %d", fc.calls["worker-medium"])
	}
}
func TestAutoRouting_MaxTokensSignal(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
		mkWorker("worker-medium", "medium-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-medium"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"medium"}}]}`),
	}

	// Short prompt but large max_tokens -> TierMedium
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 1000, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("medium")) {
		t.Errorf("expected response from medium-model due to max_tokens, got: %s", w.Body.String())
	}
	if fc.calls["worker-medium"] != 1 {
		t.Errorf("expected 1 call to worker-medium, got %d", fc.calls["worker-medium"])
	}
}
func TestAutoRouting_NoModelForTier(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
			// TierMedium not configured
			// TierComplex not configured
		},
	}

	srv, _, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	// Medium prompt -> no model configured for TierMedium
	longPrompt := string(make([]byte, 1200))
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: longPrompt}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no model configured") {
		t.Errorf("expected 'no model configured' error, got: %s", w.Body.String())
	}
}
func TestAutoRouting_ModelsListIncludesAuto(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "concrete-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}

	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetAutoRouting(autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	hasAuto := false
	for _, m := range resp.Data {
		if m.Name == "auto" {
			hasAuto = true
			if !m.Loaded {
				t.Errorf("auto model should be Loaded=true")
			}
			if m.Backend != "auto" {
				t.Errorf("auto model backend should be 'auto', got %s", m.Backend)
			}
		}
	}
	if !hasAuto {
		t.Error("expected synthetic 'auto' model in /v1/models when auto routing enabled")
	}
}
func TestAutoRouting_ModelsListExcludesAutoWhenDisabled(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "concrete-model"),
	}

	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	// No SetAutoRouting call -> disabled
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	for _, m := range resp.Data {
		if m.Name == "auto" {
			t.Error("expected NO 'auto' model when auto routing disabled")
		}
	}
}

func TestAutoRouting_CompletionEndpoint(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
		mkWorker("worker-medium", "medium-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-small"] = mockResult{
		body: []byte(`{"id":"cmpl-1","object":"text_completion","choices":[{"text":"done"}]}`),
	}

	w, req := makeCompletionRequest("auto", "short", 100)
	srv.handleCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("done")) {
		t.Errorf("expected response from small-model, got: %s", w.Body.String())
	}
	if fc.calls["worker-small"] != 1 {
		t.Errorf("expected 1 call to worker-small, got %d", fc.calls["worker-small"])
	}
}
func TestAutoRouting_NonAutoModelUnchanged(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-llama", "llama-3-8b"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-llama"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"llama"}}]}`),
	}

	w, req := makeChatRequest("llama-3-8b", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("llama")) {
		t.Errorf("expected response from llama-3-8b, got: %s", w.Body.String())
	}
	if fc.calls["worker-llama"] != 1 {
		t.Errorf("expected 1 call to worker-llama, got %d", fc.calls["worker-llama"])
	}
}
func TestAutoRouting_StreamingPath(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}

	srv, _, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	// Streaming doesn't use mock client's Complete; just verify selectWorker
	// gets the right model by checking the internal resolve logic.
	model, err := srv.resolveAutoModelChat(&protocol.ChatRequest{
		Model:     "auto",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "Hi"}},
		Stream:    true,
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("resolveAutoModelChat failed: %v", err)
	}
	if model != "small-model" {
		t.Errorf("expected small-model for streaming, got %s", model)
	}
}
func TestAutoRouting_CounterRecordsConcreteModel(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}

	srv, fc, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-small"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"small"}}]}`),
	}

	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Counter should have recorded "small-model", not "auto"
	if srv.counter == nil {
		t.Fatal("counter is nil")
	}
	snapshot := srv.counter.Snapshot()
	// The counter is now recorded exactly once by dispatchWithFailover.
	if snapshot["small-model"] != 1 {
		t.Errorf("expected counter for small-model == 1 (single recording), got %v", snapshot)
	}
	if _, ok := snapshot["auto"]; ok {
		t.Errorf("expected no 'auto' entry in counter, got %v", snapshot)
	}
}
func TestAutoRouting_StreamingCounterRecordsConcreteModel(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-small", "small-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}

	srv, _, tr := setupAutoRoutingHandler(t, workers, autoCfg)
	defer func() { _ = tr.reg.Stop() }()

	// For streaming, the mock returns empty stream (channel closed immediately)
	// but we can still verify the model resolution and counter
	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, true)
	srv.handleChatCompletions(w, req)

	// Streaming may return 200 with empty SSE or may fail due to mock setup,
	// so we just verify the model was resolved correctly by checking the counter
	// (which is recorded before streaming starts)

	// Counter should have recorded "small-model"
	if srv.counter == nil {
		t.Fatal("counter is nil")
	}
	snapshot := srv.counter.Snapshot()
	if snapshot["small-model"] != 1 {
		t.Errorf("expected counter for small-model=1, got %v", snapshot)
	}
	if _, ok := snapshot["auto"]; ok {
		t.Errorf("expected no 'auto' entry in counter, got %v", snapshot)
	}
}
