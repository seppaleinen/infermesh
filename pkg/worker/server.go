package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// Server is the HTTP server for the worker.
type Server struct {
	log          *slog.Logger
	server       *http.Server
	addr         string
	models       []protocol.ModelInfo
	chatHistory  []ChatRequest
	capabilities *capabilities.Aggregator
	config       security.Config
	backend      Backend // Backend adapter for model inference
}

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest represents a request to the chat completions endpoint.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

// CompletionRequest represents a request to the completions endpoint.
type CompletionRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Stream      bool    `json:"stream"`
}

// Message represents a message in the chat completion response.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Choice represents a single choice in a completion response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	Text         string  `json:"text,omitempty"`
	FinishReason string  `json:"finish_reason"`
}

// CompletionChunk represents a streaming chunk of a completion response.
type CompletionChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model,omitempty"`
	Choices []Choice `json:"choices"`
}

// ChatChunk represents a streaming chunk of a chat completion response.
type ChatChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model,omitempty"`
	Choices []Choice `json:"choices"`
}

// ModelsResponse represents the response from the models endpoint.
type ModelsResponse struct {
	Object string               `json:"object"`
	Data   []protocol.ModelInfo `json:"data"`
}

// BackendAdapter wraps the backend interface for the server.
type BackendAdapter struct {
	Backend Backend
	Model   string // The model name to use for this worker
}

// NewBackendAdapter creates a new backend adapter.
func NewBackendAdapter(backend Backend, model string) *BackendAdapter {
	return &BackendAdapter{
		Backend: backend,
		Model:   model,
	}
}

// ChatCompletion handles a chat completion request using the backend.
func (a *BackendAdapter) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	return a.Backend.CompleteChat(ctx, a.Model, req)
}

// Completion handles a completion request using the backend.
func (a *BackendAdapter) Completion(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	return a.Backend.CompleteCompletions(ctx, a.Model, req)
}

// CompleteChat implements the Backend interface.
func (a *BackendAdapter) CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
	return a.Backend.CompleteChat(ctx, model, req)
}

// CompleteCompletions implements the Backend interface.
func (a *BackendAdapter) CompleteCompletions(ctx context.Context, model string, req CompletionRequest) (CompletionResponse, error) {
	return a.Backend.CompleteCompletions(ctx, model, req)
}

// StreamChat implements the Backend interface.
func (a *BackendAdapter) StreamChat(ctx context.Context, model string, req ChatRequest) (<-chan ChatChunk, <-chan error) {
	return a.Backend.StreamChat(ctx, model, req)
}

// StreamCompletions implements the Backend interface.
func (a *BackendAdapter) StreamCompletions(ctx context.Context, model string, req CompletionRequest) (<-chan CompletionChunk, <-chan error) {
	return a.Backend.StreamCompletions(ctx, model, req)
}

// LoadModel implements the Backend interface.
func (a *BackendAdapter) LoadModel(path string) (bool, error) {
	return a.Backend.LoadModel(path)
}

// UnloadModel implements the Backend interface.
func (a *BackendAdapter) UnloadModel() error {
	return a.Backend.UnloadModel()
}

// ListModels implements the Backend interface.
func (a *BackendAdapter) ListModels() ([]protocol.ModelInfo, error) {
	return a.Backend.ListModels()
}

// GetMetrics implements the Backend interface.
func (a *BackendAdapter) GetMetrics() (Metrics, error) {
	return a.Backend.GetMetrics()
}

// Name implements the Backend interface.
func (a *BackendAdapter) Name() string {
	return a.Backend.Name()
}

// IsHealthy implements the Backend interface.
func (a *BackendAdapter) IsHealthy() bool {
	return a.Backend.IsHealthy()
}

// HealthCheck implements the Backend interface.
func (a *BackendAdapter) HealthCheck() error {
	return a.Backend.HealthCheck()
}

// GetCircuitState implements the Backend interface.
func (a *BackendAdapter) GetCircuitState() CircuitState {
	return a.Backend.GetCircuitState()
}

// NewServer creates a new worker HTTP server.
func NewServer(log *slog.Logger, addr string, cfg security.Config) *Server {
	agg := capabilities.NewAggregator(capabilities.Defaults(), log)
	return &Server{
		log:          log,
		addr:         addr,
		models:       []protocol.ModelInfo{},
		chatHistory:  []ChatRequest{},
		capabilities: agg,
		config:       cfg,
	}
}

// SetBackend sets the backend adapter for this server.
func (s *Server) SetBackend(backend Backend, model string) {
	s.backend = NewBackendAdapter(backend, model)
	
	// Try to dynamically discover models from the backend
	if discoveredModels, err := backend.ListModels(); err == nil && len(discoveredModels) > 0 {
		s.log.Info("dynamically discovered models from backend",
			"backend", backend.Name(),
			"count", len(discoveredModels),
		)
		// Merge discovered models with existing models
		s.models = mergeModels(s.models, discoveredModels)
	} else if err != nil {
		s.log.Warn("failed to discover models from backend", "error", err)
	}
}

// mergeModels combines two model lists, avoiding duplicates by name.
func mergeModels(existing, discovered []protocol.ModelInfo) []protocol.ModelInfo {
	seen := make(map[string]protocol.ModelInfo)
	for _, m := range existing {
		seen[m.Name] = m
	}
	for _, m := range discovered {
		seen[m.Name] = m
	}
	result := make([]protocol.ModelInfo, 0, len(seen))
	for _, m := range seen {
		result = append(result, m)
	}
	return result
}

// SetModels sets the list of models known to this worker.
func (s *Server) SetModels(models []protocol.ModelInfo) {
	s.models = models
}

// Start runs the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/capabilities", s.handleCapabilities)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/completions", s.completions)
	mux.HandleFunc("/v1/models", s.modelsList)

	var handler http.Handler = mux
	if !security.IsDevMode(s.config) {
		// Prod mode: enforce mTLS - worker requires client cert from router
		tlsConfig := &tls.Config{
			ClientAuth: tls.RequireAndVerifyClientCert,
		}

		// Load worker's certificate for serving HTTPS
		if cert, err := security.LoadTLSCertFromFile(s.config.MTLSCert, s.config.MTLSKey); err == nil {
			tlsConfig.Certificates = []tls.Certificate{*cert}

			// Load CA cert to verify router's client certificate
			if s.config.CertDir != "" {
				caCertPool := x509.NewCertPool()
				caCertPath := filepath.Join(s.config.CertDir, "ca.crt")
				if caCertBytes, err := os.ReadFile(caCertPath); err == nil {
					if caCert, err := x509.ParseCertificate(caCertBytes); err == nil {
						caCertPool.AddCert(caCert)
					}
				}
				tlsConfig.ClientCAs = caCertPool
			}
		}

		handler = mux // For worker, we handle TLS at the server level, not middleware

		s.server = &http.Server{
			Addr:      s.addr,
			Handler:   handler,
			TLSConfig: tlsConfig,
		}

		s.log.Info("worker server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.config), "tls", "enabled")

		go func() {
			<-ctx.Done()
			s.server.Shutdown(context.Background())
		}()

		return s.server.ListenAndServeTLS("", "")
	}

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: handler,
	}

	s.log.Info("worker server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.config))

	go func() {
		<-ctx.Done()
		s.server.Shutdown(context.Background())
	}()

	return s.server.ListenAndServe()
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string {
	if s.server != nil {
		return s.server.Addr
	}
	return s.addr
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleCapabilities handles the /capabilities HTTP endpoint.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Detect capabilities
	caps, err := s.capabilities.Detect()
	if err != nil {
		s.log.Error("capability detection failed", "error", err)
		caps = s.capabilities.Get()
	}

	worker := protocol.WorkerInfo{
		ID:           "worker-1",
		Hostname:     "infermesh-worker",
		IP:           "127.0.0.1",
		Port:         8081,
		Status:       protocol.StatusAvailable,
		Version:      "v1",
		Capabilities: caps,
	}

	// Include models declared via SetModels (--model-path) so the router
	// can see and schedule them. Detect() only reads the model config file.
	worker.Capabilities.Models = append(worker.Capabilities.Models, s.models...)

	if err := json.NewEncoder(w).Encode(worker); err != nil {
		s.log.Error("failed to encode capabilities", "error", err)
		http.Error(w, "failed to encode capabilities", http.StatusInternalServerError)
	}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metrics := "# HELP workers_total Total number of registered workers\n"
	metrics += "# TYPE workers_total gauge\n"
	metrics += "workers_total 1\n"
	metrics += "# HELP models_total Total number of available models\n"
	metrics += "# TYPE models_total gauge\n"
	metrics += fmt.Sprintf("models_total %d\n", len(s.models))
	w.Write([]byte(metrics))
}

// chatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Parse request
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse chat request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Store in history for debugging
	s.chatHistory = append(s.chatHistory, req)

	// Check if backend is configured
	if s.backend == nil {
		s.log.Error("backend not configured")
		http.Error(w, "backend not configured", http.StatusInternalServerError)
		return
	}

	// Check circuit breaker state
	if !s.backend.IsHealthy() {
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	// Create streaming response
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Process the request via backend
	ctx := r.Context()
	respChan := make(chan interface{}, 10)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		if req.Stream {
			// For streaming, we need to handle chunks
			// This would require the backend to support streaming responses
			// For now, we'll fall back to non-streaming
			s.log.Warn("streaming requested but falling back to non-streaming")
		}

		chatResp, err := s.backend.CompleteChat(ctx, req.Model, req)
		if err != nil {
			errChan <- err
			return
		}
		respChan <- chatResp
	}()

	// Send SSE response
	for {
		select {
		case err, ok := <-errChan:
			if !ok {
				// Channel closed, stop processing
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if err != nil {
				s.log.Error("backend error", "error", err)
				errorData := map[string]string{"error": err.Error()}
				jsonErr, _ := json.Marshal(errorData)
				fmt.Fprintf(w, "data: %s\n\n", string(jsonErr))
				flusher.Flush()
				return
			}
		case resp, ok := <-respChan:
			if !ok {
				// Stream ended
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			switch v := resp.(type) {
			case ChatResponse:
				jsonResp, err := json.Marshal(v)
				if err != nil {
					s.log.Error("failed to marshal response", "error", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", string(jsonResp))
				flusher.Flush()
			case CompletionResponse:
				jsonResp, err := json.Marshal(v)
				if err != nil {
					s.log.Error("failed to marshal response", "error", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", string(jsonResp))
				flusher.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

// completions handles the /v1/completions HTTP endpoint.
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Parse request
	var req CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse completion request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Check if backend is configured
	if s.backend == nil {
		s.log.Error("backend not configured")
		http.Error(w, "backend not configured", http.StatusInternalServerError)
		return
	}

	// Check circuit breaker state
	if !s.backend.IsHealthy() {
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	// Create streaming response
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Process the request via backend
	ctx := r.Context()
	respChan := make(chan interface{}, 10)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		if req.Stream {
			s.log.Warn("streaming requested but falling back to non-streaming")
		}

		compResp, err := s.backend.CompleteCompletions(ctx, req.Model, req)
		if err != nil {
			errChan <- err
			return
		}
		respChan <- compResp
	}()

	// Send SSE response
	for {
		select {
		case err, ok := <-errChan:
			if !ok {
				// Channel closed, stop processing
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if err != nil {
				s.log.Error("backend error", "error", err)
				errorData := map[string]string{"error": err.Error()}
				jsonErr, _ := json.Marshal(errorData)
				fmt.Fprintf(w, "data: %s\n\n", string(jsonErr))
				flusher.Flush()
				return
			}
		case resp, ok := <-respChan:
			if !ok {
				// Stream ended
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			switch v := resp.(type) {
			case CompletionResponse:
				jsonResp, err := json.Marshal(v)
				if err != nil {
					s.log.Error("failed to marshal response", "error", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", string(jsonResp))
				flusher.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

// modelsList handles the /v1/models HTTP endpoint.
func (s *Server) modelsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := ModelsResponse{Object: "list", Data: s.models}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		http.Error(w, "failed to encode models", http.StatusInternalServerError)
	}
}