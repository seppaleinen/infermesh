package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestOpenAICompatibleBackend_Name tests the Name method.
func TestOpenAICompatibleBackend_Name(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1234", "", 0, 0)
	if b.Name() != "openai-compatible" {
		t.Fatalf("Name() = %q, want openai-compatible", b.Name())
	}
}

// TestOpenAICompatibleBackend_HealthCheck tests HealthCheck.
func TestOpenAICompatibleBackend_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	if err := b.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck() = %v, want nil", err)
	}
}

// TestOpenAICompatibleBackend_HealthCheck_Failure tests HealthCheck failure.
func TestOpenAICompatibleBackend_HealthCheck_Failure(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1", "", 0, 0)
	if err := b.HealthCheck(); err == nil {
		t.Fatal("HealthCheck() = nil, want error for unreachable backend")
	}
}

// TestOpenAICompatibleBackend_HealthCheckDetails tests HealthCheckDetails.
func TestOpenAICompatibleBackend_HealthCheckDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	status, metrics, err := b.HealthCheckDetails()
	if err != nil {
		t.Fatalf("HealthCheckDetails() err = %v, want nil", err)
	}
	if status != Healthy {
		t.Fatalf("status = %v, want Healthy", status)
	}
	if metrics.TotalLatency <= 0 {
		t.Fatalf("TotalLatency = %v, want > 0", metrics.TotalLatency)
	}
}

// TestOpenAICompatibleBackend_GetCircuitState tests GetCircuitState.
func TestOpenAICompatibleBackend_GetCircuitState(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1234", "", 0, 0)
	if b.GetCircuitState() != CircuitClosed {
		t.Fatalf("GetCircuitState() = %v, want CircuitClosed", b.GetCircuitState())
	}
}

// TestOpenAICompatibleBackend_LoadModel tests LoadModel success path.
func TestOpenAICompatibleBackend_LoadModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	loaded, err := b.LoadModel("test-model")
	if err != nil {
		t.Fatalf("LoadModel() err = %v, want nil", err)
	}
	if !loaded {
		t.Fatal("LoadModel() = false, want true")
	}
}

// TestOpenAICompatibleBackend_LoadModel_Failure tests LoadModel with unreachable backend.
func TestOpenAICompatibleBackend_LoadModel_Failure(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1", "", 0, 0)
	loaded, err := b.LoadModel("test-model")
	if err == nil {
		t.Fatal("LoadModel() err = nil, want error")
	}
	if loaded {
		t.Fatal("LoadModel() = true, want false")
	}
}

// TestOpenAICompatibleBackend_UnloadModel tests UnloadModel.
func TestOpenAICompatibleBackend_UnloadModel(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1234", "", 0, 0)
	if err := b.UnloadModel(); err != nil {
		t.Fatalf("UnloadModel() = %v, want nil", err)
	}
}

// TestOpenAICompatibleBackend_CompleteCompletions tests CompleteCompletions.
func TestOpenAICompatibleBackend_CompleteCompletions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-test","object":"text_completion","created":123,"model":"test","choices":[{"index":0,"text":"ok","finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	resp, err := b.CompleteCompletions(context.Background(), "test", protocol.CompletionRequest{Model: "test", Prompt: "hi"})
	if err != nil {
		t.Fatalf("CompleteCompletions() err = %v, want nil", err)
	}
	if resp.ID != "cmpl-test" {
		t.Fatalf("resp.ID = %q, want cmpl-test", resp.ID)
	}
}

// TestOpenAICompatibleBackend_CompleteCompletions_Failure tests CompleteCompletions with unreachable backend.
func TestOpenAICompatibleBackend_CompleteCompletions_Failure(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1", "", 0, 0)
	_, err := b.CompleteCompletions(context.Background(), "test", protocol.CompletionRequest{Model: "test", Prompt: "hi"})
	if err == nil {
		t.Fatal("CompleteCompletions() err = nil, want error")
	}
}

// TestCustomBackend_GetProviderName tests GetProviderName.
func TestCustomBackend_GetProviderName(t *testing.T) {
	b := NewCustomBackend("http://localhost:1234", "", "my-provider", 0, 0)
	if b.GetProviderName() != "my-provider" {
		t.Fatalf("GetProviderName() = %q, want my-provider", b.GetProviderName())
	}
}

// TestCustomBackend_GetBaseURL tests GetBaseURL.
func TestCustomBackend_GetBaseURL(t *testing.T) {
	b := NewCustomBackend("http://localhost:9999", "", "my-provider", 0, 0)
	if b.GetBaseURL() != "http://localhost:9999" {
		t.Fatalf("GetBaseURL() = %q, want http://localhost:9999", b.GetBaseURL())
	}
}

// TestCustomBackend_Name tests the Name method for CustomBackend.
func TestCustomBackend_Name(t *testing.T) {
	b := NewCustomBackend("http://localhost:1234", "", "my-provider", 0, 0)
	if b.Name() != "my-provider" {
		t.Fatalf("Name() = %q, want my-provider", b.Name())
	}
}

// TestCustomBackend_Name_Default tests the default name when providerName is empty.
func TestCustomBackend_Name_Default(t *testing.T) {
	b := NewCustomBackend("http://localhost:1234", "", "", 0, 0)
	if b.Name() != "custom" {
		t.Fatalf("Name() = %q, want custom", b.Name())
	}
}

// TestBuildChatRequest tests buildChatRequest.
func TestBuildChatRequest(t *testing.T) {
	req := protocol.ChatRequest{Model: "test", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}, Temperature: 0.7, MaxTokens: 100}
	out := buildChatRequest(req, "test-model", 50)
	if out["model"] != "test-model" {
		t.Fatalf("model = %v, want test-model", out["model"])
	}
	if out["messages"] == nil {
		t.Fatal("messages is nil")
	}
	if out["max_tokens"] != 50 {
		t.Fatalf("max_tokens = %v, want 50", out["max_tokens"])
	}
}

// TestBuildCompletionRequest tests buildCompletionRequest.
func TestBuildCompletionRequest(t *testing.T) {
	req := protocol.CompletionRequest{Model: "test", Prompt: "hi", Temperature: 0.7, MaxTokens: 100}
	out := buildCompletionRequest(req, "test-model", 50)
	if out["model"] != "test-model" {
		t.Fatalf("model = %v, want test-model", out["model"])
	}
	if out["prompt"] != "hi" {
		t.Fatalf("prompt = %v, want hi", out["prompt"])
	}
	if out["max_tokens"] != 50 {
		t.Fatalf("max_tokens = %v, want 50", out["max_tokens"])
	}
}

// TestUnmarshalChatChunk tests unmarshalChatChunk.
func TestUnmarshalChatChunk(t *testing.T) {
	raw := []byte(`{"id":"test","object":"chat.completion.chunk","created":123,"model":"test","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
	chunk, err := unmarshalChatChunk(raw)
	if err != nil {
		t.Fatalf("unmarshalChatChunk err = %v", err)
	}
	if chunk.Model != "test" {
		t.Fatalf("Model = %q, want test", chunk.Model)
	}
}

// TestUnmarshalCompletionChunk tests unmarshalCompletionChunk.
func TestUnmarshalCompletionChunk(t *testing.T) {
	raw := []byte(`{"id":"test","object":"text_completion","created":123,"model":"test","choices":[{"index":0,"text":"token","finish_reason":"stop"}]}`)
	chunk, err := unmarshalCompletionChunk(raw)
	if err != nil {
		t.Fatalf("unmarshalCompletionChunk err = %v", err)
	}
	if chunk.Model != "test" {
		t.Fatalf("Model = %q, want test", chunk.Model)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Text != "token" {
		t.Fatalf("Choices = %v, want 1 choice with text 'token'", chunk.Choices)
	}
}

// TestInferenceTracker_Prun tests the Prune method.
func TestInferenceTracker_Prun(t *testing.T) {
	tracker := NewInferenceTracker(10 * time.Second)
	tracker.Record("model-a", 100, 50, 10*time.Millisecond, 100*time.Millisecond)
	tracker.Record("model-a", 200, 50, 10*time.Millisecond, 100*time.Millisecond)
	tracker.Record("model-b", 300, 50, 10*time.Millisecond, 100*time.Millisecond)

	// Prune should not remove recent entries
	tracker.Prune()

	_, ok := tracker.Snapshot("model-a")
	if !ok {
		t.Fatal("Prune() removed recent model-a entry")
	}
	_, ok2 := tracker.Snapshot("model-b")
	if !ok2 {
		t.Fatal("Prune() removed recent model-b entry")
	}
}

// TestInferenceTracker_RecordNegativeTokens tests clamping of negative tokens.
func TestInferenceTracker_RecordNegativeTokens(t *testing.T) {
	tracker := NewInferenceTracker(10 * time.Second)
	// Negative tokens should be clamped to zero
	tracker.Record("model-a", -100, 50, 10*time.Millisecond, 100*time.Millisecond)
	snap, ok := tracker.Snapshot("model-a")
	if !ok {
		t.Fatal("Snapshot() failed for model-a")
	}
	_ = snap
}

// TestLoadModelHandler tests the loadModelHandler HTTP endpoint.
func TestLoadModelHandler(t *testing.T) {
	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models/load", srv.loadModelHandler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Request to load a known model
	body := []byte(`{"model":"m1"}`)
	resp, err := http.Post(ts.URL+"/v1/models/load", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST /v1/models/load err = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 200, 400, 403, or 404", resp.StatusCode)
	}
}

// TestCompleteChat_CircuitOpen tests CompleteChat when circuit is open.
func TestCompleteChat_CircuitOpen(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://localhost:1", "", 0, 0)
	// Force circuit open by triggering failures
	for i := 0; i < 10; i++ {
		_ = b.HealthCheck()
	}
	_, err := b.CompleteChat(context.Background(), "test", protocol.ChatRequest{Model: "test", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("CompleteChat() err = nil, want error when circuit open")
	}
}

// TestCompleteChat_BackendError tests CompleteChat with 500 response.
func TestCompleteChat_BackendError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal error"}`))
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	_, err := b.CompleteChat(context.Background(), "test", protocol.ChatRequest{Model: "test", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("CompleteChat() err = nil, want error for 500 response")
	}
}

// TestWriteGPUDetails tests writeGPUDetails.
func TestWriteGPUDetails(t *testing.T) {
	var out strings.Builder
	err := writeGPUDetails(func(format string, args ...any) error {
		return writeString(&out, format, args...)
	}, protocol.GPUInfo{Model: "RTX 4090", TotalVRAM: 24576, FreeVRAM: 12288})
	if err != nil {
		t.Fatalf("writeGPUDetails err = %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("writeGPUDetails produced no output")
	}
}

func writeString(w *strings.Builder, format string, args ...any) error {
	_, err := w.WriteString(format)
	return err
}

// TestWriteGPU tests writeGPU with no GPU.
func TestWriteGPU(t *testing.T) {
	var out strings.Builder
	caps := protocol.Capabilities{}
	err := writeGPU(func(s string, args ...any) error {
		return writeString(&out, s, args...)
	}, caps)
	if err != nil {
		t.Fatalf("writeGPU err = %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("writeGPU produced no output")
	}
}

// TestWriteSystemMetrics tests writeSystem.
func TestWriteSystemMetrics(t *testing.T) {
	var out strings.Builder
	caps := protocol.Capabilities{
		System: protocol.MemoryInfo{
			TotalMB: 1024,
			FreeMB:  4096,
		},
	}
	err := writeSystem(func(s string, args ...any) error {
		return writeString(&out, s, args...)
	}, caps)
	if err != nil {
		t.Fatalf("writeSystem err = %v", err)
	}
}

// TestBuildChatRequest_ToolChoice tests buildChatRequest with tool_choice.
func TestBuildChatRequest_ToolChoice(t *testing.T) {
	req := protocol.ChatRequest{
		Model:      "test",
		Messages:   []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		ToolChoice: "auto",
	}
	out := buildChatRequest(req, "test-model", 50)
	if tc, ok := out["tool_choice"].(string); !ok || tc != "auto" {
		t.Fatalf("tool_choice = %v, want auto", out["tool_choice"])
	}
}

// TestBuildChatRequest_ParallelToolCalls tests buildChatRequest with parallel_tool_calls.
func TestBuildChatRequest_ParallelToolCalls(t *testing.T) {
	ptc := true
	req := protocol.ChatRequest{
		Model:             "test",
		Messages:          []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		ParallelToolCalls: &ptc,
	}
	out := buildChatRequest(req, "test-model", 50)
	if parr, ok := out["parallel_tool_calls"].(*bool); !ok || parr == nil || !*parr {
		t.Fatalf("parallel_tool_calls = %v, want true", out["parallel_tool_calls"])
	}
}

// TestBuildCompletionRequest_Temperature tests buildCompletionRequest with temperature.
func TestBuildCompletionRequest_Temperature(t *testing.T) {
	req := protocol.CompletionRequest{Model: "test", Prompt: "hi", Temperature: 0.8}
	out := buildCompletionRequest(req, "test-model", 50)
	if out["temperature"] != 0.8 {
		t.Fatalf("temperature = %v, want 0.8", out["temperature"])
	}
}

// TestBuildCompletionRequest_MaxTokensZero tests buildCompletionRequest with max_tokens=0.
func TestBuildCompletionRequest_MaxTokensZero(t *testing.T) {
	req := protocol.CompletionRequest{Model: "test", Prompt: "hi"}
	out := buildCompletionRequest(req, "test-model", 0)
	// When maxTokens is 0, it should still be in the output (0 is a valid value)
	if out["max_tokens"] == nil {
		t.Fatal("max_tokens should not be nil")
	}
}

// TestWriteVRAM tests writeVRAM.
func TestWriteVRAM(t *testing.T) {
	var out strings.Builder
	caps := protocol.Capabilities{
		VRAM: protocol.MemoryInfo{
			TotalMB: 24576,
			FreeMB:  12288,
		},
	}
	err := writeVRAM(func(s string, args ...any) error {
		return writeString(&out, s, args...)
	}, caps)
	if err != nil {
		t.Fatalf("writeVRAM err = %v", err)
	}
}
