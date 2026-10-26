package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// healthProbeURL returns the endpoint used to probe backend health.
//
// Historically this was "/health", but LM Studio does not implement that
// endpoint: it logs "Unexpected endpoint or method. (GET /health). Returning
// 200 anyway" and still returns 200, so the probe "worked" while spamming the
// backend log with errors (issue #78). "/v1/models" is the endpoint every
// OpenAI-compatible backend implements (it is also what ListModels uses), so
// it is the correct health probe: a reachable, working backend answers it,
// a dead one does not.
func (b *OpenAICompatibleBackend) healthProbeURL() string {
	return b.baseURL + "/v1/models"
}

// IsHealthy checks if the backend is healthy. It reads the breaker state
// without consuming the half-open transition (unlike CanExecute, which
// advances the state machine). A backend that is open or in cooldown is
// considered unhealthy without probing it.
func (b *OpenAICompatibleBackend) IsHealthy() bool {
	switch b.circuitBreaker.GetState() {
	case CircuitClosed, CircuitHalfOpen:
		// Probe the backend to confirm it is actually reachable.
		_, err := b.client.Get(b.healthProbeURL())
		return err == nil
	default:
		// CircuitOpen: don't waste a probe; the breaker is already failing fast.
		return false
	}
}

// HealthCheck performs a health check on the backend.
func (b *OpenAICompatibleBackend) HealthCheck() error {
	if !b.circuitBreaker.CanExecute() {
		return fmt.Errorf("circuit breaker is open for backend %s", b.baseURL)
	}
	_, err := b.client.Get(b.healthProbeURL())
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
	_, err := b.client.Get(b.healthProbeURL())
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

// OpenAICompatibleBackend implements Backend for OpenAI-compatible APIs.
type OpenAICompatibleBackend struct {
	baseURL        string
	apiKey         string
	client         *http.Client // used for non-streaming calls (CompleteChat, CompleteCompletions, LoadModel, health checks, ListModels)
	streamClient   *http.Client // used ONLY for streaming calls (StreamChat, StreamCompletions); Timeout: 0 (idle timeout applied via context)
	circuitBreaker *CircuitBreaker
	maxTokens      int // default max_tokens to send when request omits it; 0 = no default
}

// defaultBackendTimeout is the HTTP client timeout used when a caller does
// not specify one (zero or negative). It matches the default for the
// worker's --backend-timeout flag. For streaming requests this is an **idle
// timeout** (silence budget, reset on activity); for non-streaming requests
// it is an absolute deadline.
const defaultBackendTimeout = 120 * time.Second

// NewOpenAICompatibleBackend creates a new OpenAI-compatible backend.
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to defaultBackendTimeout (120s). The timeout
// applies as an absolute deadline for non-streaming calls, and as an idle
// timeout (reset on activity) for streaming calls.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default" (the backend will
// use its own built-in default).
func NewOpenAICompatibleBackend(baseURL, apiKey string, timeout time.Duration, maxTokens int) *OpenAICompatibleBackend {
	if timeout <= 0 {
		timeout = defaultBackendTimeout
	}
	return &OpenAICompatibleBackend{
		baseURL: baseURL,
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: timeout,
		},
		// streamClient has no client-level timeout; idle timeout is applied
		// via context in StreamChat/StreamCompletions so it resets on activity.
		streamClient: &http.Client{
			Timeout: 0,
		},
		circuitBreaker: NewCircuitBreaker(5, 30*time.Second),
		maxTokens:      maxTokens,
	}
}

// Name returns the backend name.
func (b *OpenAICompatibleBackend) Name() string {
	return "openai-compatible"
}

// LoadModel loads a model into the backend.
func (b *OpenAICompatibleBackend) LoadModel(_ string) (bool, error) {
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
func (b *OpenAICompatibleBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	// Check circuit breaker
	if !b.circuitBreaker.CanExecute() {
		return protocol.ChatResponse{}, fmt.Errorf("circuit breaker is open for backend %s", b.baseURL)
	}

	var resp protocol.ChatResponse
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

func (b *OpenAICompatibleBackend) completeChatInternal(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	url := b.baseURL + "/v1/chat/completions"

	// Set default max_tokens if not specified. Use backend-configured default,
	// falling back to 100 for backward compatibility with LM Studio.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		if b.maxTokens > 0 {
			maxTokens = b.maxTokens
		} else {
			maxTokens = 100 // fallback for LM Studio compatibility
		}
	}

	// Convert protocol.ChatRequest to backend-specific request
	backendReq := map[string]interface{}{
		"model":       model,
		"messages":    req.Messages,
		"stream":      false, // We handle streaming at the server level
		"max_tokens":  maxTokens,
		"temperature": req.Temperature,
	}

	jsonBody, err := json.Marshal(backendReq)
	if err != nil {
		return protocol.ChatResponse{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return protocol.ChatResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return protocol.ChatResponse{}, fmt.Errorf("backend request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return protocol.ChatResponse{}, &BackendHTTPError{StatusCode: resp.StatusCode, Message: string(body)}
	}

	var chatResp protocol.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return protocol.ChatResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return chatResp, nil
}

// StreamChat and StreamCompletions live in openai_stream.go.

// CompleteCompletions generates a completion via the backend.
func (b *OpenAICompatibleBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	url := b.baseURL + "/v1/completions"

	// Set default max_tokens if not specified. Use backend-configured default,
	// falling back to 100 for backward compatibility.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		if b.maxTokens > 0 {
			maxTokens = b.maxTokens
		} else {
			maxTokens = 100 // fallback for backward compatibility
		}
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
		return protocol.CompletionResponse{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return protocol.CompletionResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return protocol.CompletionResponse{}, fmt.Errorf("backend request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return protocol.CompletionResponse{}, &BackendHTTPError{StatusCode: resp.StatusCode, Message: string(body)}
	}

	var compResp protocol.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&compResp); err != nil {
		return protocol.CompletionResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return compResp, nil
}

// ListModels returns available models from the backend.
func (b *OpenAICompatibleBackend) ListModels() ([]protocol.ModelInfo, error) {
	url := b.baseURL + "/v1/models"

	resp, err := b.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default".
func NewLlamaCppBackend(endpoint string, contextSize, threads, gpuLayers int, timeout time.Duration, maxTokens int) *LlamaCppBackend {
	return &LlamaCppBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, "", timeout, maxTokens),
		contextSize:             contextSize,
		threads:                 threads,
		gpuLayers:               gpuLayers,
	}
}

// Name returns the backend name.
func (b *LlamaCppBackend) Name() string {
	return "llama-cpp"
}

// GetMetrics returns the backend's effective thread count so the worker can
// seed its ConcurrencyEstimator and report it to the router (issue #139).
func (b *LlamaCppBackend) GetMetrics() (Metrics, error) {
	return Metrics{Threads: b.threads}, nil
}

// OllamaBackend implements Backend for Ollama.
type OllamaBackend struct {
	OpenAICompatibleBackend
	model string
}

// NewOllamaBackend creates a new Ollama backend.
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default".
func NewOllamaBackend(endpoint, model string, timeout time.Duration, maxTokens int) *OllamaBackend {
	return &OllamaBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, "", timeout, maxTokens),
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
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default".
func NewLMStudioBackend(endpoint, modelPath string, timeout time.Duration, maxTokens int) *LMStudioBackend {
	return &LMStudioBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, "", timeout, maxTokens),
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
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default".
func NewVLLMBackend(endpoint, modelName string, timeout time.Duration, maxTokens int) *VLLMBackend {
	return &VLLMBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(endpoint, "", timeout, maxTokens),
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
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s.
// maxTokens is the default max_tokens to send to the backend when the
// incoming request omits it. Zero means "no default".
func NewCustomBackend(baseURL, apiKey, providerName string, timeout time.Duration, maxTokens int) *CustomBackend {
	return &CustomBackend{
		OpenAICompatibleBackend: *NewOpenAICompatibleBackend(baseURL, apiKey, timeout, maxTokens),
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
