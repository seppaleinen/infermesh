package worker

import (
	"context"
	"fmt"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// Backend defines the interface for backend adapters that handle
// OpenAI-compatible API calls and model lifecycle operations.
type Backend interface {
	Name() string
	LoadModel(path string) (bool, error)
	UnloadModel() error
	CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error)
	CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error)
	ListModels() ([]protocol.ModelInfo, error)
	GetMetrics() (Metrics, error)
	// Health check
	IsHealthy() bool
	HealthCheck() error
	// Circuit breaker status
	GetCircuitState() CircuitState
	// Streaming methods
	StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error)
	StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error)
}

// BackendHTTPError carries an HTTP status code from a backend response.
type BackendHTTPError struct {
	StatusCode int
	Message    string
}

func (e *BackendHTTPError) Error() string {
	return fmt.Sprintf("backend returned %d: %s", e.StatusCode, e.Message)
}

// Metrics represents backend performance metrics.
type Metrics struct {
	Requests       int   `json:"requests_total"`
	Errors         int   `json:"errors_total"`
	LatencyMs      int64 `json:"latency_ms_total"`
	ModelLoads     int   `json:"model_loads_total"`
	ModelUnloads   int   `json:"model_unloads_total"`
	MemoryUsageMB  int   `json:"memory_usage_mb"`
	GPUUsagePct    int   `json:"gpu_usage_pct"`
	// Threads is the backend's effective thread count (llama-cpp only).
	// Zero means the backend does not expose a thread count.
	Threads int `json:"threads,omitempty"`
}