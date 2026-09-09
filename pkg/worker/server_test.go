package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// mockBackend is a test implementation of the Backend interface.
type mockBackend struct {
	models      []protocol.ModelInfo
	healthCheck error
}

func newMockBackend() *mockBackend {
	return &mockBackend{}
}

func (m *mockBackend) SetHealthCheckError(err error) {
	m.healthCheck = err
}

func (m *mockBackend) Name() string { return "mock" }
func (m *mockBackend) LoadModel(path string) (bool, error) { return true, nil }
func (m *mockBackend) UnloadModel() error { return nil }
func (m *mockBackend) ListModels() ([]protocol.ModelInfo, error) { return m.models, nil }
func (m *mockBackend) GetMetrics() (Metrics, error) { return Metrics{}, nil }
func (m *mockBackend) CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
	return ChatResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Choices: []Choice{
			{
				Index:        0,
				Message:      Message{Role: "assistant", Content: "Mock response"},
				FinishReason: "stop",
			},
		},
	}, nil
}
func (m *mockBackend) CompleteCompletions(ctx context.Context, model string, req CompletionRequest) (CompletionResponse, error) {
	return CompletionResponse{
		ID:      "cmpl-test",
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Choices: []Choice{
			{
				Index:        0,
				Text:         "Mock completion",
				FinishReason: "stop",
			},
		},
	}, nil
}
// IsHealthy implements the Backend interface.
func (m *mockBackend) IsHealthy() bool { return true }
// HealthCheck implements the Backend interface.
func (m *mockBackend) HealthCheck() error { return m.healthCheck }
// GetCircuitState implements the Backend interface.
func (m *mockBackend) GetCircuitState() CircuitState { return CircuitClosed }
// StreamChat implements the Backend interface.
func (m *mockBackend) StreamChat(ctx context.Context, model string, req ChatRequest) (<-chan ChatChunk, <-chan error) {
	chatCh := make(chan ChatChunk, 100)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		chatCh <- ChatChunk{
			ID:      "chatcmpl-test",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []Choice{
				{
					Index:        0,
					Message:      Message{Role: "assistant", Content: "Mock streaming response"},
					FinishReason: "stop",
				},
			},
		}
	}()
	return chatCh, errCh
}
// StreamCompletions implements the Backend interface.
func (m *mockBackend) StreamCompletions(ctx context.Context, model string, req CompletionRequest) (<-chan CompletionChunk, <-chan error) {
	compCh := make(chan CompletionChunk, 100)
	errCh := make(chan error, 1)
	go func() {
		defer close(compCh)
		defer close(errCh)
		compCh <- CompletionChunk{
			ID:      "cmpl-test",
			Object:  "text_completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []Choice{
				{
					Index:        0,
					Text:         "Mock streaming completion",
					FinishReason: "stop",
				},
			},
		}
	}()
	return compCh, errCh
}

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
	defer os.Remove(tmpFile.Name())

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
	tmpFile.Close()

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

			var response ModelsResponse
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
			chatReq := ChatRequest{
				Model: "gpt-4",
				Messages: []ChatMessage{{Role: "user", Content: "Hello"}},
				Stream: true,
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

func TestCompletionsHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "completions with SSE",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionReq := CompletionRequest{
				Model:     "gpt-4",
				Prompt:    "Hello",
				MaxTokens: 100,
				Stream:    true,
			}

			body, err := json.Marshal(completionReq)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.completions(w, req)

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

func TestServerShutdown(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	_ = capabilities.Defaults()

	tests := []struct {
		name string
	}{
		{
			name: "graceful shutdown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			errChan := make(chan error, 1)
			go func() {
				errChan <- server.Start(ctx)
			}()

			time.Sleep(100 * time.Millisecond)

			cancel()

			select {
			case err := <-errChan:
				if err != nil && err.Error() != "http: Server closed" && err.Error() != "http: Server closed\n" {
					t.Logf("Server shutdown error (expected): %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("Server did not shutdown in time")
			}
		})
	}
}

// TestRegisterURL verifies registerURL normalises trailing slashes correctly.
func TestRegisterURL(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		expected string
	}{
		{
			name:     "no trailing slash",
			base:     "http://127.0.0.1:8080",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "single trailing slash",
			base:     "http://127.0.0.1:8080/",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "multiple trailing slashes",
			base:     "http://127.0.0.1:8080///",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "path base without trailing slash",
			base:     "http://127.0.0.1:8080/api/v1",
			expected: "http://127.0.0.1:8080/api/v1/v1/dev/register",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registerURL(tt.base)
			if got != tt.expected {
				t.Errorf("registerURL(%q) = %q, want %q", tt.base, got, tt.expected)
			}
		})
	}
}

// TestRegisterLoopTrailingSlash sends a base URL with a trailing slash and
// verifies the router still receives the request at /v1/dev/register (not //v1/dev/register).
func TestRegisterLoopTrailingSlash(t *testing.T) {
	var receivedPath string
	callCount := 0

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "trailing-slash-worker",
		IP:   "127.0.0.1",
		Port: 8085,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Intentionally append a trailing slash
	go RegisterLoop(ctx, router.URL+"/", info, 50*time.Millisecond, testLogger())

	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if callCount < 2 {
		t.Errorf("expected at least 2 calls, got %d", callCount)
	}
	if receivedPath != "/v1/dev/register" {
		t.Errorf("expected path '/v1/dev/register', got '%s'", receivedPath)
	}
}

// --- RegisterLoop Tests ---

// TestRegisterLoopSendsPost verifies that RegisterLoop posts worker info to the router.
func TestRegisterLoopSendsPost(t *testing.T) {
	var receivedBody protocol.WorkerInfo
	var receivedMethod string
	var receivedPath string
	callCount := 0

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		callCount++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:     "loop-worker-1",
		IP:     "127.0.0.1",
		Port:   8081,
		Status: protocol.StatusUnassigned, // should be sent as-is; router decides final status
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())

	// Wait for at least 2 calls (initial + one ticker)
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond) // let goroutine exit

	if callCount < 2 {
		t.Errorf("expected at least 2 calls, got %d", callCount)
	}
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}
	if receivedPath != "/v1/dev/register" {
		t.Errorf("expected path '/v1/dev/register', got '%s'", receivedPath)
	}
	if receivedBody.ID != "loop-worker-1" {
		t.Errorf("expected worker ID 'loop-worker-1', got '%s'", receivedBody.ID)
	}
	if receivedBody.IP != "127.0.0.1" {
		t.Errorf("expected IP '127.0.0.1', got '%s'", receivedBody.IP)
	}
	if receivedBody.Port != 8081 {
		t.Errorf("expected port 8081, got %d", receivedBody.Port)
	}
}

// TestRegisterLoopContextCancellation verifies the loop stops when context is cancelled.
func TestRegisterLoopContextCancellation(t *testing.T) {
	callCount := 0

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "cancel-worker",
		IP:   "127.0.0.1",
		Port: 8082,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())
		close(done)
	}()

	// Let a couple of heartbeats fire
	time.Sleep(150 * time.Millisecond)
	countBeforeCancel := callCount

	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterLoop did not stop after context cancellation")
	}

	// After cancel, no more calls should happen
	time.Sleep(200 * time.Millisecond)
	countAfterCancel := callCount

	if countAfterCancel != countBeforeCancel {
		t.Errorf("expected no more calls after cancel, got %d before and %d after", countBeforeCancel, countAfterCancel)
	}
}

// TestRegisterLoopNon200DoesNotCrash verifies a non-200 response is logged but doesn't crash.
func TestRegisterLoopNon200DoesNotCrash(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"something went wrong"}`))
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "error-worker",
		IP:   "127.0.0.1",
		Port: 8083,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())
		close(done)
	}()

	// Let a few calls happen with 500 responses
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — didn't crash
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterLoop crashed on non-200 response")
	}
}

// TestRegisterLoopImmediatePost verifies that RegisterLoop posts immediately (before first tick).
func TestRegisterLoopImmediatePost(t *testing.T) {
	var callCount int32

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "immediate-worker",
		IP:   "127.0.0.1",
		Port: 8084,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RegisterLoop(ctx, router.URL, info, 10*time.Second, testLogger()) // long interval

	// Give enough time for the immediate post, but not enough for the 10s ticker
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if callCount < 1 {
		t.Errorf("expected at least 1 immediate call, got %d", callCount)
	}
}

// TestModelHealthTracker_EnabledDisabled verifies that RecordHealthCheck is a no-op
// when the tracker is disabled, and records when enabled.
func TestModelHealthTracker_EnabledDisabled(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		tracker := NewModelHealthTracker(true)
		tracker.SetEnabled(false)

		tracker.RecordHealthCheck(Healthy, ModelMetrics{})

		_, status, failures, _, enabled := tracker.Snapshot()
		if enabled {
			t.Error("expected tracker to be disabled")
		}
		if status != Unknown {
			t.Errorf("expected status Unknown, got %v", status)
		}
		if failures != 0 {
			t.Errorf("expected 0 consecutive failures, got %d", failures)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		tracker := NewModelHealthTracker(true)
		tracker.SetEnabled(true)

		tracker.RecordHealthCheck(Healthy, ModelMetrics{})

		_, status, failures, _, enabled := tracker.Snapshot()
		if !enabled {
			t.Error("expected tracker to be enabled")
		}
		if status != Healthy {
			t.Errorf("expected status Healthy, got %v", status)
		}
		if failures != 0 {
			t.Errorf("expected 0 consecutive failures, got %d", failures)
		}
	})
}

// TestModelHealthTracker_SetEnabled verifies toggling enabled state affects recording.
func TestModelHealthTracker_SetEnabled(t *testing.T) {
	tracker := NewModelHealthTracker(true)

	tracker.RecordHealthCheck(Healthy, ModelMetrics{})
	tracker.SetEnabled(false)
	tracker.RecordHealthCheck(Unhealthy, ModelMetrics{})
	tracker.SetEnabled(true)
	tracker.RecordHealthCheck(Unhealthy, ModelMetrics{})

	_, status, failures, _, _ := tracker.Snapshot()
	if status != Unhealthy {
		t.Errorf("expected status Unhealthy, got %v", status)
	}
	if failures != 1 {
		t.Errorf("expected 1 consecutive failure, got %d", failures)
	}
}

// TestServer_HealthCheckLoop_Enabled verifies that StartHealthCheckLoop runs
// periodic checks when enabled.
func TestServer_HealthCheckLoop_Enabled(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.StartHealthCheckLoop(ctx, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Healthy {
		t.Errorf("expected status Healthy, got %v", status)
	}
	if failures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", failures)
	}
}

// TestServer_HealthCheckLoop_NilBackend verifies that the health check loop
// continues gracefully when no backend is configured.
func TestServer_HealthCheckLoop_NilBackend(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		server.StartHealthCheckLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Unknown {
		t.Errorf("expected status Unknown, got %v", status)
	}
	if failures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", failures)
	}

	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Error("health check loop did not stop after context cancellation")
	}
}

// TestServer_HealthCheckLoop_FailurePath verifies that a backend health check
// error is recorded as an Unhealthy status.
func TestServer_HealthCheckLoop_FailurePath(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	backend.SetHealthCheckError(errors.New("backend unavailable"))
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.StartHealthCheckLoop(ctx, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Unhealthy {
		t.Errorf("expected status Unhealthy, got %v", status)
	}
	if failures == 0 {
		t.Error("expected at least 1 consecutive failure to be recorded")
	}
}

// TestServer_HealthCheckLoop_Cancel verifies that the loop stops gracefully on
// context cancellation.
func TestServer_HealthCheckLoop_Cancel(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		server.StartHealthCheckLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Error("health check loop did not stop after context cancellation")
	}
}

// TestServer_NewServer_HealthTrackerInit verifies that NewServer initializes the
// health tracker with the correct Enabled state based on config.
func TestServer_NewServer_HealthTrackerInit(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		expected      bool
	}{
		{
			name:     "enabled by default",
			enabled:  true,
			expected: true,
		},
		{
			name:     "disabled when configured",
			enabled:  false,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: tt.enabled})

			if server.healthTracker == nil {
				t.Fatal("expected health tracker to be initialized")
			}
			if server.healthTracker.Enabled != tt.expected {
				t.Errorf("expected Enabled=%v, got %v", tt.expected, server.healthTracker.Enabled)
			}
		})
	}
}