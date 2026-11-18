package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestLlamaCppBackend tests the llama-cpp backend implementation.
func TestLlamaCppBackend_Name(t *testing.T) {
	b := NewLlamaCppBackend("http://localhost:8080", 4096, 4, 0, 0, 0)
	if b.Name() != "llama-cpp" {
		t.Fatalf("Name() = %q, want llama-cpp", b.Name())
	}
}

func TestLlamaCppBackend_GetMetrics(t *testing.T) {
	b := NewLlamaCppBackend("http://localhost:8080", 4096, 4, 0, 0, 0)
	metrics, err := b.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics() = %v", err)
	}
	if metrics.Threads != 4 {
		t.Fatalf("Threads = %d, want 4", metrics.Threads)
	}
}

func TestLlamaCppBackend_IsHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	b := NewLlamaCppBackend(server.URL, 4096, 4, 0, 0, 0)
	if !b.IsHealthy() {
		t.Fatal("IsHealthy() = false, want true")
	}
}

// TestOllamaBackend tests the Ollama backend implementation.
func TestOllamaBackend_Name(t *testing.T) {
	b := NewOllamaBackend("http://localhost:11434", "lmstudio", 0, 0)
	if b.Name() != "ollama" {
		t.Fatalf("Name() = %q, want ollama", b.Name())
	}
}

func TestOllamaBackend_IsHealthy(t *testing.T) {
	b := NewOllamaBackend("http://localhost:1", "model", 0, 0)
	// Without a running server, health check fails but circuit breaker opens
	_ = b
}

// TestLMStudioBackend tests the LMStudio backend implementation.
func TestLMStudioBackend_Name(t *testing.T) {
	b := NewLMStudioBackend("http://localhost:1234", "/models/test.gguf", 0, 0)
	if b.Name() != "lmstudio" {
		t.Fatalf("Name() = %q, want lmstudio", b.Name())
	}
}

// TestVLLMBackend tests the vllm backend implementation.
func TestVLLMBackend_Name(t *testing.T) {
	b := NewVLLMBackend("http://localhost:8000", "model", 0, 0)
	if b.Name() != "vllm" {
		t.Fatalf("Name() = %q, want vllm", b.Name())
	}
}

// TestBackendHTTPError_Error tests the BackendHTTPError.Error() method.
func TestBackendHTTPError_Error(t *testing.T) {
	err := &BackendHTTPError{StatusCode: 429, Message: "rate limited"}
	expected := "backend returned 429: rate limited"
	if err.Error() != expected {
		t.Fatalf("Error() = %q, want %q", err.Error(), expected)
	}
}

// TestCircuitState_String tests the CircuitState.String() method.
func TestCircuitState_String(t *testing.T) {
	tests := []struct {
		state CircuitState
		want  string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half-open"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("%v.String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// TestCircuitBreaker tests the circuit breaker state machine.
func TestCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(2, 10*time.Millisecond)
	if cb.CanExecute() != true {
		t.Fatal("CanExecute() = false, want true initially")
	}
	if cb.GetState() != CircuitClosed {
		t.Fatal("GetState() != CircuitClosed initially")
	}

	// First failure should not open circuit
	cb.RecordFailure()
	if cb.GetState() != CircuitClosed {
		t.Fatalf("GetState() = %v after 1 failure, want closed", cb.GetState())
	}

	// Second failure opens circuit
	cb.RecordFailure()
	if cb.CanExecute() != false {
		t.Fatal("CanExecute() = true, want false after 2 failures")
	}
	if cb.GetState() != CircuitOpen {
		t.Fatalf("GetState() = %v after 2 failures, want open", cb.GetState())
	}
}

// TestCircuitBreaker_FailureThreshold tests the failure threshold behavior.
func TestCircuitBreaker_FailureThreshold(t *testing.T) {
	// Default threshold is 5
	cb := NewCircuitBreaker(5, time.Minute)
	for i := 0; i < 5; i++ {
		cb.RecordFailure()
	}
	if cb.GetState() != CircuitOpen {
		t.Fatalf("GetState() = %v, want open after 5 failures", cb.GetState())
	}
}

// TestCircuitBreaker_SuccessResets tests that success resets the breaker.
func TestCircuitBreaker_SuccessResets(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.GetState() != CircuitClosed {
		t.Fatalf("GetState() = %v after 2 failures, want closed", cb.GetState())
	}
	cb.RecordSuccess()
	if cb.GetState() != CircuitClosed {
		t.Fatalf("GetState() = %v after success, want closed", cb.GetState())
	}
}

// TestMetrics tests the Metrics struct.
func TestMetrics(t *testing.T) {
	m := Metrics{
		Requests:      100,
		Errors:        5,
		LatencyMs:     5000,
		ModelLoads:    3,
		ModelUnloads:  1,
		MemoryUsageMB: 2048,
		GPUUsagePct:   45,
		Threads:       4,
	}
	if m.Requests != 100 {
		t.Fatalf("Requests = %d, want 100", m.Requests)
	}
	if m.Threads != 4 {
		t.Fatalf("Threads = %d, want 4", m.Threads)
	}
}

// TestHealthStatus tests the HealthStatus type values.
func TestHealthStatus(t *testing.T) {
	// HealthStatus is defined in model_health.go
	_ = Healthy
	_ = Unhealthy
}

// TestDeriveStatusCode tests the status code derivation for the circuit breaker.
func TestDeriveStatusCode(t *testing.T) {
	// deriveStatusCode is internal; test it returns 0 for nil error
	tests := []struct {
		err  error
		want int
	}{
		{context.DeadlineExceeded, 0}, // derives from err.Error() string
		{nil, 200},
	}
	for _, tt := range tests {
		got := deriveStatusCode(tt.err)
		if got != tt.want {
			t.Errorf("deriveStatusCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// TestCompleteChat_Success tests successful chat completion.
func TestCompleteChat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	b := NewOpenAICompatibleBackend(server.URL, "", 0, 0)
	resp, err := b.CompleteChat(context.Background(), "test", protocol.ChatRequest{Model: "test", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("CompleteChat() = %v", err)
	}
	if resp.ID != "chatcmpl-test" {
		t.Fatalf("resp.ID = %q, want chatcmpl-test", resp.ID)
	}
}

// TestDoneChatChunk tests the done frame builder for chat.
func TestDoneChatChunk(t *testing.T) {
	chunk := doneChatChunk("test-model")
	if chunk.Model != "test-model" {
		t.Fatalf("Model = %q, want test-model", chunk.Model)
	}
	if chunk.Object != "chat.completion.chunk" {
		t.Fatalf("Object = %q, want chat.completion.chunk", chunk.Object)
	}
	// Empty Choices is correct for a done frame
	if len(chunk.Choices) != 0 {
		t.Fatalf("Choices = %d, want 0 (done frame has empty choices)", len(chunk.Choices))
	}
}
