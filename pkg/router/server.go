package router

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/security"
	"log/slog"
)

// ErrorResponse represents an OpenAI-compatible error response.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail represents the error detail in an OpenAI-compatible error response.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeErrorResponse writes an OpenAI-compatible error response.
func writeErrorResponse(w http.ResponseWriter, statusCode int, errMsg string, errType string, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := ErrorResponse{
		Error: ErrorDetail{
			Message: errMsg,
			Type:    errType,
			Code:    errCode,
		},
	}

	if err := json.NewEncoder(w).Encode(errorResp); err != nil {
		slog.Error("failed to encode error response", "error", err)
	}
}

// WorkerInfo represents a simplified worker information for the /v1/workers endpoint.
type WorkerInfo struct {
	ID            string           `json:"id"`
	Hostname      string           `json:"hostname"`
	IP            string           `json:"ip"`
	Port          int              `json:"port"`
	Status        protocol.WorkerStatus `json:"status"`
	Version       string           `json:"version"`
	LoadedModels  []string         `json:"loaded_models"`
	LastSeen      time.Time        `json:"last_seen"`
}

// WorkersResponse represents the response from the /v1/workers endpoint.
type WorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// ErrorModelNotFound represents an error when a model is not found on any worker.
type ErrModelNotFound struct {
	Model string
}

func (e *ErrModelNotFound) Error() string {
	return fmt.Sprintf("model %s not found on any worker", e.Model)
}

// Server is the HTTP server for the router.
type Server struct {
	log     *slog.Logger
	reg     registry.Registry
	addr    string
	cache   *CapabilityCache
	cfg     security.Config
	server  *http.Server
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

// CompletionRequest represents a request to the completions endpoint.
type CompletionRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Stream      bool    `json:"stream"`
}

// ModelsResponse represents the response from the models endpoint.
type ModelsResponse struct {
	Object string               `json:"object"`
	Data   []protocol.ModelInfo `json:"data"`
}

// NewServer creates a new router HTTP server.
func NewServer(reg registry.Registry, log *slog.Logger, addr string, cfg security.Config) *Server {
	cache := NewCapabilityCache(reg, log)
	return &Server{
		log:     log,
		reg:     reg,
		addr:    addr,
		cache:   cache,
		cfg:     cfg,
	}
}

// Start runs the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/workers", s.handleWorkersList)
	mux.HandleFunc("/v1/dev/register", s.handleDevRegister)

	var handler http.Handler = mux
	if !security.IsDevMode(s.cfg) {
		// Prod mode: enforce mTLS - router requires client cert from worker
		tlsConfig := &tls.Config{
			ClientAuth: tls.RequireAndVerifyClientCert,
		}

		// Load router's certificate for serving HTTPS
		if cert, err := security.LoadTLSCertFromFile(s.cfg.MTLSCert, s.cfg.MTLSKey); err == nil {
			tlsConfig.Certificates = []tls.Certificate{*cert}

			// Load CA cert to verify worker's client certificate
			if s.cfg.CertDir != "" {
				caCertPool := x509.NewCertPool()
				caCertPath := filepath.Join(s.cfg.CertDir, "ca.crt")
				if caCertBytes, err := os.ReadFile(caCertPath); err == nil {
					if caCert, err := x509.ParseCertificate(caCertBytes); err == nil {
						caCertPool.AddCert(caCert)
					}
				}
				tlsConfig.ClientCAs = caCertPool
			}
		}

		s.server = &http.Server{
			Addr:      s.addr,
			Handler:   handler,
			TLSConfig: tlsConfig,
		}
	} else {
		s.server = &http.Server{
			Addr:    s.addr,
			Handler: handler,
		}
	}

	s.log.Info("router server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.cfg))

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

// handleChatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Parse request
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse chat request", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Check if scheduler is configured
	if s.reg == nil {
		s.log.Error("registry not configured")
		writeErrorResponse(w, http.StatusInternalServerError, "registry not configured", "server_error", "internal_error")
		return
	}

	// Select a worker using the registry
	worker, err := s.selectWorker(req.Model)
	if err != nil {
		s.log.Error("failed to select worker", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, "no workers", "server_error", "no_workers")
		return
	}

	// Proxy the request to the selected worker with SSE support
	s.proxyChatStream(w, r, worker, req)
}

// handleCompletions handles the /v1/completions HTTP endpoint.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Parse request
	var req CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse completion request", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Check if scheduler is configured
	if s.reg == nil {
		s.log.Error("registry not configured")
		writeErrorResponse(w, http.StatusInternalServerError, "registry not configured", "server_error", "internal_error")
		return
	}

	// Select a worker using the registry
	worker, err := s.selectWorker(req.Model)
	if err != nil {
		s.log.Error("failed to select worker", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, "no workers", "server_error", "no_workers")
		return
	}

	// Proxy the request to the selected worker with SSE support
	s.proxyCompletionStream(w, r, worker, req)
}

// handleWorkersList handles the /v1/workers HTTP endpoint.
func (s *Server) handleWorkersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	workers := make([]WorkerInfo, 0, len(s.cache.cache))
	for _, w := range s.cache.cache {
		info := WorkerInfo{
			ID:            w.ID,
			Hostname:      w.Hostname,
			IP:            w.IP,
			Port:          w.Port,
			Status:        w.Status,
			Version:       w.Version,
			LoadedModels:  getLoadedModelNames(w.Capabilities.Models),
			LastSeen:      w.LastSeen,
		}
		workers = append(workers, info)
	}

	response := WorkersResponse{Workers: workers}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode workers response", "error", err)
	}
}

// getLoadedModelNames extracts the names of models that are loaded from a model list.
func getLoadedModelNames(models []protocol.ModelInfo) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		if m.Loaded {
			names = append(names, m.Name)
		}
	}
	return names
}

// handleModelsList handles the /v1/models HTTP endpoint.
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Get capabilities from cache
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var workers []protocol.WorkerInfo
	for _, w := range s.cache.cache {
		workers = append(workers, w)
	}

	// Aggregate models from all workers, only including loaded models
	var models []protocol.ModelInfo
	for _, worker := range workers {
		for _, m := range worker.Capabilities.Models {
			if m.Loaded {
				models = append(models, m)
			}
		}
	}

	response := ModelsResponse{Object: "list", Data: models}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "failed to encode models", "server_error", "encoding_error")
	}
}

// handleDevRegister handles the /v1/dev/register HTTP endpoint.
func (s *Server) handleDevRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(r.Body).Decode(&worker); err != nil {
		s.log.Error("failed to parse worker registration", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Validate empty ID
	if worker.ID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "empty ID", "invalid_request_error", "validation_error")
		return
	}

	// Validate port range
	if worker.Port <= 0 || worker.Port > 65535 {
		writeErrorResponse(w, http.StatusBadRequest, "invalid port", "invalid_request_error", "validation_error")
		return
	}

	// Validate IP - only loopback allowed in dev mode
	if worker.IP != "127.0.0.1" && worker.IP != "localhost" {
		writeErrorResponse(w, http.StatusForbidden, "non-loopback IP not allowed", "authentication_error", "ip_validation_error")
		return
	}

	// Force status to available for dev mode
	worker.Status = protocol.StatusAvailable

	// Register worker
	if err := s.reg.HandleEvent(protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
	}); err != nil {
		s.log.Error("failed to register worker", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "failed to register worker", "server_error", "registration_error")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// selectWorker selects a worker for the given model (simple round-robin for now).
func (s *Server) selectWorker(model string) (protocol.WorkerInfo, error) {
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var candidates []protocol.WorkerInfo
	for _, worker := range s.cache.cache {
		// Check if worker has the requested model loaded
		for _, m := range worker.Capabilities.Models {
			if m.Name == model && m.Loaded {
				candidates = append(candidates, worker)
				break
			}
		}
	}

	if len(candidates) == 0 {
		// Try to auto-load the model on any worker that knows about it.
		// The cache only refreshes on the next heartbeat, so a re-fetch would
		// see the same stale state and recurse forever; instead we build a
		// local deep copy of the freshly loaded worker without mutating the
		// (read-locked) cache.
		for _, worker := range s.cache.cache {
			for _, m := range worker.Capabilities.Models {
				if m.Name == model {
					if s.loadModelOnWorker(worker, model) {
						loaded := worker
						loaded.Capabilities.Models = append([]protocol.ModelInfo(nil), worker.Capabilities.Models...)
						for i := range loaded.Capabilities.Models {
							if loaded.Capabilities.Models[i].Name == model {
								loaded.Capabilities.Models[i].Loaded = true
								break
							}
						}
						candidates = append(candidates, loaded)
						break
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return protocol.WorkerInfo{}, fmt.Errorf("no worker found with model %s loaded", model)
	}

	// Simple round-robin selection (use timestamp for now)
	selected := candidates[0]
	return selected, nil
}

// loadModelOnWorker attempts to load a model on a worker using HTTP proxy
func (s *Server) loadModelOnWorker(worker protocol.WorkerInfo, modelName string) bool {
	url := fmt.Sprintf("http://%s:%d/v1/models/load", worker.IP, worker.Port)

	loadReq := map[string]string{"model": modelName}
	body, err := json.Marshal(loadReq)
	if err != nil {
		return false
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// retryDoRequestWithRetry executes an HTTP request with exponential backoff retry logic.
// Max 3 retries with 1s, 2s, 4s backoff intervals.
// Returns the response or an error after all retries are exhausted.
func retryDoRequestWithRetry(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt < maxRetries {
			backoff := time.Duration(1<<attempt) * time.Second
			time.Sleep(backoff)
		}
	}
	return nil, fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
}

// proxyChatStream proxies a chat completion request to a worker with SSE support.
func (s *Server) proxyChatStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req ChatRequest) {
	// Apply context-based timeout (default 30s)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	workerURL := fmt.Sprintf("http://%s:%d", worker.IP, worker.Port)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Prepare request to worker
	jsonBody, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal chat request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", workerURL+"/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		s.log.Error("failed to create request to worker", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "request_error")
		return
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Forward request to worker with retry logic
	resp, err := retryDoRequestWithRetry(client, httpReq, 3)
	if err != nil {
		// Check if the error was due to timeout
		if ctx.Err() == context.DeadlineExceeded {
			s.log.Error("request timed out after retries", "worker", workerURL, "timeout", "30s")
			writeErrorResponse(w, http.StatusGatewayTimeout, "request timeout", "server_error", "timeout_error")
			return
		}
		s.log.Error("failed to connect to worker after retries", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
		return
	}
	defer resp.Body.Close()

	// Check worker's status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		s.log.Error("worker returned error", "status", resp.StatusCode, "body", string(body))
		writeErrorResponse(w, resp.StatusCode, "worker error", "server_error", "worker_error")
		return
	}

	// Set headers for SSE response
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		writeErrorResponse(w, http.StatusInternalServerError, "streaming unsupported", "server_error", "streaming_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Copy SSE stream from worker to client
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		s.log.Error("failed to stream response from worker", "error", err)
	}
	flusher.Flush()
}

// proxyCompletionStream proxies a completion request to a worker with SSE support.
func (s *Server) proxyCompletionStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req CompletionRequest) {
	// Apply context-based timeout (default 30s)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	workerURL := fmt.Sprintf("http://%s:%d", worker.IP, worker.Port)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Prepare request to worker
	jsonBody, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal completion request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", workerURL+"/v1/completions", bytes.NewReader(jsonBody))
	if err != nil {
		s.log.Error("failed to create request to worker", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "request_error")
		return
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Forward request to worker with retry logic
	resp, err := retryDoRequestWithRetry(client, httpReq, 3)
	if err != nil {
		// Check if the error was due to timeout
		if ctx.Err() == context.DeadlineExceeded {
			s.log.Error("request timed out after retries", "worker", workerURL, "timeout", "30s")
			writeErrorResponse(w, http.StatusGatewayTimeout, "request timeout", "server_error", "timeout_error")
			return
		}
		s.log.Error("failed to connect to worker after retries", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
		return
	}
	defer resp.Body.Close()

	// Check worker's status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		s.log.Error("worker returned error", "status", resp.StatusCode, "body", string(body))
		writeErrorResponse(w, resp.StatusCode, "worker error", "server_error", "worker_error")
		return
	}

	// Set headers for SSE response
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		writeErrorResponse(w, http.StatusInternalServerError, "streaming unsupported", "server_error", "streaming_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Copy SSE stream from worker to client
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		s.log.Error("failed to stream response from worker", "error", err)
	}
	flusher.Flush()
}