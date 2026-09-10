package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"log/slog"
)

// Backend defines the interface for backend adapters that handle
// OpenAI-compatible API calls and model lifecycle operations.
type Backend interface {
	Name() string
	LoadModel(path string) (bool, error)
	UnloadModel() error
	CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error)
	CompleteCompletions(ctx context.Context, model string, req CompletionRequest) (CompletionResponse, error)
	ListModels() ([]protocol.ModelInfo, error)
	GetMetrics() (Metrics, error)
	// Health check
	IsHealthy() bool
	HealthCheck() error
	// Circuit breaker status
	GetCircuitState() CircuitState
	// Streaming methods
	StreamChat(ctx context.Context, model string, req ChatRequest) (<-chan ChatChunk, <-chan error)
	StreamCompletions(ctx context.Context, model string, req CompletionRequest) (<-chan CompletionChunk, <-chan error)
}

// CircuitState represents the state of the circuit breaker.
type CircuitState int

const (
	CircuitClosed CircuitState = iota // Normal operation
	CircuitHalfOpen                   // Testing if backend recovered
	CircuitOpen                       // Backend is down, fail fast
)

func (cs CircuitState) String() string {
	return [...]string{"closed", "half-open", "open"}[cs]
}

// CircuitBreaker implements the circuit breaker pattern for resilience.
type CircuitBreaker struct {
	mu            sync.RWMutex
	state         CircuitState
	failureCount  int
	failureThreshold int
	successThreshold int
	lastFailureTime time.Time
	cooldownPeriod time.Duration
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(failureThreshold int, cooldownPeriod time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		failureThreshold: failureThreshold,
		successThreshold: 1,
		cooldownPeriod:   cooldownPeriod,
	}
}

// RecordSuccess records a successful operation.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == CircuitHalfOpen {
		cb.state = CircuitClosed
		cb.failureCount = 0
	}
}

// RecordFailure records a failed operation.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount++
	cb.lastFailureTime = time.Now()
	if cb.failureCount >= cb.failureThreshold {
		cb.state = CircuitOpen
	}
}

// CanExecute checks if an operation can be executed.
func (cb *CircuitBreaker) CanExecute() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitHalfOpen:
		return true // Allow one request through
	case CircuitOpen:
		// Check if cooldown period has passed
		if time.Since(cb.lastFailureTime) > cb.cooldownPeriod {
			return true // Allow one request through to test
		}
		return false
	}
	return true
}

// GetState returns the current circuit breaker state.
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// IsHealthy checks if the backend is healthy.
func (b *OpenAICompatibleBackend) IsHealthy() bool {
	// Check circuit breaker first
	if !b.circuitBreaker.CanExecute() {
		return false
	}
	// Check if backend is reachable
	_, err := b.client.Get(b.baseURL + "/v1/models")
	return err == nil
}

// HealthCheck performs a health check on the backend.
func (b *OpenAICompatibleBackend) HealthCheck() error {
	if !b.circuitBreaker.CanExecute() {
		return fmt.Errorf("circuit breaker is open for backend %s", b.baseURL)
	}
	_, err := b.client.Get(b.baseURL + "/v1/models")
	if err != nil {
		b.circuitBreaker.RecordFailure()
		return fmt.Errorf("health check failed for %s: %w", b.baseURL, err)
	}
	b.circuitBreaker.RecordSuccess()
	return nil
}

// HealthCheckDetails performs a detailed health check returning status and metrics.
func (b *OpenAICompatibleBackend) HealthCheckDetails() (HealthStatus, ModelMetrics, error) {
	if !b.circuitBreaker.CanExecute() {
		return Unhealthy, ModelMetrics{}, fmt.Errorf("circuit breaker is open for backend %s", b.baseURL)
	}
	start := time.Now()
	_, err := b.client.Get(b.baseURL + "/v1/models")
	elapsed := time.Since(start)
	if err != nil {
		b.circuitBreaker.RecordFailure()
		return Unhealthy, ModelMetrics{TotalLatency: elapsed}, fmt.Errorf("health check failed for %s: %w", b.baseURL, err)
	}
	b.circuitBreaker.RecordSuccess()
	return Healthy, ModelMetrics{TotalLatency: elapsed}, nil
}

// GetCircuitState returns the current circuit breaker state.
func (b *OpenAICompatibleBackend) GetCircuitState() CircuitState {
	return b.circuitBreaker.GetState()
}

// retryWithBackoff implements exponential backoff retry logic.
func retryWithBackoff(ctx context.Context, maxRetries int, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			backoff := time.Duration(1<<attempt) * time.Second
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	return fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
}

// Metrics represents backend performance metrics.
type Metrics struct {
	Requests      int   `json:"requests_total"`
	Errors        int   `json:"errors_total"`
	LatencyMs     int64 `json:"latency_ms_total"`
	ModelLoads    int   `json:"model_loads_total"`
	ModelUnloads  int   `json:"model_unloads_total"`
	MemoryUsageMB int   `json:"memory_usage_mb"`
	GPUUsagePct   int   `json:"gpu_usage_pct"`
}

// ChatResponse represents a chat completion response from a backend.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// CompletionResponse represents a completion response from a backend.
type CompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Usage represents token usage statistics.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAICompatibleBackend implements Backend for OpenAI-compatible APIs.
type OpenAICompatibleBackend struct {
	baseURL string
	apiKey  string
	client  *http.Client
	circuitBreaker *CircuitBreaker
}

// NewOpenAICompatibleBackend creates a new OpenAI-compatible backend.
func NewOpenAICompatibleBackend(baseURL, apiKey string) *OpenAICompatibleBackend {
	return &OpenAICompatibleBackend{
		baseURL: baseURL,
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		circuitBreaker: NewCircuitBreaker(5, 30*time.Second),
	}
}

// Name returns the backend name.
func (b *OpenAICompatibleBackend) Name() string {
	return "openai-compatible"
}

// LoadModel loads a model into the backend.
func (b *OpenAICompatibleBackend) LoadModel(path string) (bool, error) {
	// For OpenAI-compatible backends, model loading is typically
	// handled by the backend itself. We just verify connectivity.
	_, err := b.client.Get(b.baseURL + "/v1/models")
	if err != nil {
		return false, fmt.Errorf("backend not reachable at %s: %w", b.baseURL, err)
	}
	return true, nil
}

// UnloadModel unloads the current model.
func (b *OpenAICompatibleBackend) UnloadModel() error {
	// OpenAI-compatible backends typically don't support explicit unload
	return nil
}

// CompleteChat generates a chat completion via the backend.
func (b *OpenAICompatibleBackend) CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
	// Check circuit breaker
	if !b.circuitBreaker.CanExecute() {
		return ChatResponse{}, fmt.Errorf("circuit breaker is open for backend %s", b.baseURL)
	}

	var resp ChatResponse
	err := retryWithBackoff(ctx, 3, func() error {
		var err error
		resp, err = b.completeChatInternal(ctx, model, req)
		if err != nil {
			b.circuitBreaker.RecordFailure()
			return err
		}
		b.circuitBreaker.RecordSuccess()
		return nil
	})
	return resp, err
}

func (b *OpenAICompatibleBackend) completeChatInternal(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
	url := b.baseURL + "/v1/chat/completions"

	// Set default max_tokens if not specified (LM Studio requires >= 1)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 100 // reasonable default
	}

	// Convert ChatRequest to backend-specific request
	backendReq := map[string]interface{}{
		"model":       model,
		"messages":    req.Messages,
		"stream":      false, // We handle streaming at the server level
		"max_tokens":  maxTokens,
		"temperature": req.Temperature,
	}

	jsonBody, err := json.Marshal(backendReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("backend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return ChatResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return chatResp, nil
}

func (b *OpenAICompatibleBackend) StreamChat(ctx context.Context, model string, req ChatRequest) (<-chan ChatChunk, <-chan error) {
	chatCh := make(chan ChatChunk, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(chatCh)
		defer close(errCh)

		// Set stream to true and build the request
		req.Stream = true
		url := b.baseURL + "/v1/chat/completions"

		// Set default max_tokens if not specified (LM Studio requires >= 1)
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 100 // reasonable default
		}

		// Convert ChatRequest to backend-specific request
		backendReq := map[string]interface{}{
			"model":       model,
			"messages":    req.Messages,
			"stream":      true,
			"max_tokens":  maxTokens,
			"temperature": req.Temperature,
		}

		jsonBody, err := json.Marshal(backendReq)
		if err != nil {
			errCh <- fmt.Errorf("failed to marshal request: %w", err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			errCh <- fmt.Errorf("failed to create request: %w", err)
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if b.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
		}

		resp, err := b.client.Do(httpReq)
		if err != nil {
			errCh <- fmt.Errorf("backend request failed: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
			return
		}

		// Parse SSE stream using bufio.Scanner for proper line reading
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || line == "\r" {
				continue
			}

			// Strip SSE prefix "data: "
			if strings.HasPrefix(line, "data: ") {
				dataStr := strings.TrimPrefix(line, "data: ")
				if dataStr == "[DONE]" {
					return
				}

				var chunk ChatChunk
				if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
					slog.Warn("failed to parse SSE chunk", "error", err)
					continue
				}

				select {
				case chatCh <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("failed to read SSE stream: %w", err)
			return
		}

		// Send [DONE] marker to indicate stream completion
		chatCh <- ChatChunk{
			ID:      "",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []Choice{},
		}
	}()

	return chatCh, errCh
}

// CompleteCompletions generates a completion via the backend.
func (b *OpenAICompatibleBackend) CompleteCompletions(ctx context.Context, model string, req CompletionRequest) (CompletionResponse, error) {
	url := b.baseURL + "/v1/completions"

	// Set default max_tokens if not specified
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 100 // reasonable default
	}

	backendReq := map[string]interface{}{
		"model":       model,
		"prompt":      req.Prompt,
		"stream":      false,
		"max_tokens":  maxTokens,
		"temperature": req.Temperature,
	}

	jsonBody, err := json.Marshal(backendReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("backend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return CompletionResponse{}, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
	}

	var compResp CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&compResp); err != nil {
		return CompletionResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return compResp, nil
}

// StreamCompletions generates a completion via the backend with streaming support.
func (b *OpenAICompatibleBackend) StreamCompletions(ctx context.Context, model string, req CompletionRequest) (<-chan CompletionChunk, <-chan error) {
	compCh := make(chan CompletionChunk, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(compCh)
		defer close(errCh)

		// Set stream to true and build the request
		req.Stream = true
		url := b.baseURL + "/v1/completions"

		// Set default max_tokens if not specified
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 100 // reasonable default
		}

		backendReq := map[string]interface{}{
			"model":       model,
			"prompt":      req.Prompt,
			"stream":      true,
			"max_tokens":  maxTokens,
			"temperature": req.Temperature,
		}

		jsonBody, err := json.Marshal(backendReq)
		if err != nil {
			errCh <- fmt.Errorf("failed to marshal request: %w", err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			errCh <- fmt.Errorf("failed to create request: %w", err)
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if b.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
		}

		resp, err := b.client.Do(httpReq)
		if err != nil {
			errCh <- fmt.Errorf("backend request failed: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
			return
		}

		// Parse SSE stream using bufio.Scanner for proper line reading
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || line == "\r" {
				continue
			}

			// Strip SSE prefix "data: "
			if strings.HasPrefix(line, "data: ") {
				dataStr := strings.TrimPrefix(line, "data: ")
				if dataStr == "[DONE]" {
					return
				}

				var chunk CompletionChunk
				if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
					slog.Warn("failed to parse SSE chunk", "error", err)
					continue
				}

				select {
				case compCh <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("failed to read SSE stream: %w", err)
			return
		}

		// Send [DONE] marker to indicate stream completion
		compCh <- CompletionChunk{
			ID:      "",
			Object:  "text_completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []Choice{},
		}
	}()

	return compCh, errCh
}

// ListModels returns available models from the backend.
func (b *OpenAICompatibleBackend) ListModels() ([]protocol.ModelInfo, error) {
	url := b.baseURL + "/v1/models"

	resp, err := b.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	// Parse OpenAI-compatible model list response. Real backends
	// (LM Studio, Ollama, vLLM) return {"data":[{"id":"...","object":"model",...}]}
	// while our internal ModelInfo uses "name". Accept both shapes and
	// treat catalogue entries as loaded (dev-mode backends serve whatever
	// they list; there is no separate load lifecycle to query).
	var result struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Object       string `json:"object"`
			OwnedBy      string `json:"owned_by"`
			Backend      string `json:"backend"`
			Quantization string `json:"quantization"`
			Loaded       *bool  `json:"loaded"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode models response: %w", err)
	}

	models := make([]protocol.ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		if name == "" {
			continue
		}
		loaded := true
		if m.Loaded != nil {
			loaded = *m.Loaded
		}
		models = append(models, protocol.ModelInfo{
			Name:         name,
			Backend:      m.Backend,
			Quantization: m.Quantization,
			Loaded:       loaded,
		})
	}

	return models, nil
}

// GetMetrics returns backend metrics.
func (b *OpenAICompatibleBackend) GetMetrics() (Metrics, error) {
	return Metrics{}, nil
}

// LlamaCppBackend implements Backend for llama.cpp.
type LlamaCppBackend struct {
	OpenAICompatibleBackend
	contextSize int
	threads     int
	gpuLayers   int
}

// NewLlamaCppBackend creates a new llama.cpp backend.
func NewLlamaCppBackend(endpoint string, contextSize, threads, gpuLayers int) *LlamaCppBackend {
	return &LlamaCppBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, ""),
		contextSize:              contextSize,
		threads:                  threads,
		gpuLayers:                gpuLayers,
	}
}

// Name returns the backend name.
func (b *LlamaCppBackend) Name() string {
	return "llama-cpp"
}

// OllamaBackend implements Backend for Ollama.
type OllamaBackend struct {
	OpenAICompatibleBackend
	model string
}

// NewOllamaBackend creates a new Ollama backend.
func NewOllamaBackend(endpoint, model string) *OllamaBackend {
	return &OllamaBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, ""),
		model:                   model,
	}
}

// Name returns the backend name.
func (b *OllamaBackend) Name() string {
	return "ollama"
}

// LMStudioBackend implements Backend for LM Studio.
type LMStudioBackend struct {
	OpenAICompatibleBackend
	modelPath string
}

// NewLMStudioBackend creates a new LM Studio backend.
func NewLMStudioBackend(endpoint, modelPath string) *LMStudioBackend {
	return &LMStudioBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, ""),
		modelPath:               modelPath,
	}
}

// Name returns the backend name.
func (b *LMStudioBackend) Name() string {
	return "lmstudio"
}

// VLLMBackend implements Backend for vLLM.
type VLLMBackend struct {
	OpenAICompatibleBackend
	modelName string
}

// NewVLLMBackend creates a new vLLM backend.
func NewVLLMBackend(endpoint, modelName string) *VLLMBackend {
	return &VLLMBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, ""),
		modelName:               modelName,
	}
}

// Name returns the backend name.
func (b *VLLMBackend) Name() string {
	return "vllm"
}

// CustomBackend implements Backend for any custom OpenAI-compatible provider.
// This allows connecting to any OpenAI-compatible endpoint, such as:
// - llama-swap
// - LocalAI
// - Text Generation Web UI (Oobabooga)
// - Any custom OpenAI-compatible server
type CustomBackend struct {
	OpenAICompatibleBackend
	providerName string
}

// NewCustomBackend creates a new custom backend for any OpenAI-compatible provider.
// baseURL: The base URL of the OpenAI-compatible API endpoint
// apiKey: The API key for authentication (if required)
// providerName: A human-readable name for the provider (e.g., "llama-swap", "localai")
func NewCustomBackend(baseURL, apiKey, providerName string) *CustomBackend {
	return &CustomBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(baseURL, apiKey),
		providerName:            providerName,
	}
}

// Name returns the backend name.
func (b *CustomBackend) Name() string {
	if b.providerName != "" {
		return b.providerName
	}
	return "custom"
}

// GetProviderName returns the human-readable provider name.
func (b *CustomBackend) GetProviderName() string {
	return b.providerName
}

// GetBaseURL returns the base URL of the custom provider.
func (b *CustomBackend) GetBaseURL() string {
	return b.baseURL
}