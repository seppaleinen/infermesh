package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	ID           string                `json:"id"`
	Hostname     string                `json:"hostname"`
	IP           string                `json:"ip"`
	Port         int                   `json:"port"`
	Status       protocol.WorkerStatus `json:"status"`
	Version      string                `json:"version"`
	APIKey       string                `json:"api_key,omitempty"`
	LoadedModels []string              `json:"loaded_models"`
	LastSeen     time.Time             `json:"last_seen"`
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
	log    *slog.Logger
	reg    registry.Registry
	addr   string
	cache  *CapabilityCache
	cfg    security.Config
	hub    *WSHub
	server *http.Server
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
    hub := NewWSHub(reg, log)
    hub.SetAPIKey(cfg.APIKey)
    return &Server{
        log:   log,
        reg:   reg,
        addr:  addr,
        cache: cache,
        cfg: cfg,
        hub:   hub,
    }
}

// SetRelayURL configures the relay WebSocket URL for outbound-only mode.
func (s *Server) SetRelayURL(url string) {
    s.hub.SetRelayURL(url)
}

// DialRelay dials the relay WebSocket connection and starts the relay reader/writer loops.
func (s *Server) DialRelay(ctx context.Context, url string) error {
    return s.hub.DialRelay(ctx, url)
}

// Start runs the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	// Guard: in prod mode, bind address must be loopback.
	if !security.IsDevMode(s.cfg) {
		if !strings.HasPrefix(s.addr, "127.0.0.1") && !strings.HasPrefix(s.addr, "localhost") {
			panic("production mode requires bind address to be loopback (127.0.0.1 or localhost)")
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/workers", s.handleWorkersList)
	mux.HandleFunc("/v1/dev/register", s.handleDevRegister)
	// Outbound WebSocket worker connectivity: workers dial ws://router:8080/v1/connect
	mux.Handle("/v1/connect", s.hub.Handler())

	// Start the capability cache event loop so registry events populate
	// the cache that /v1/models, /v1/workers and worker selection read from.
	// Without this, dev HTTP registrations land in the registry but the
	// cache stays empty → empty workers list, null models, no_workers.
	if s.cache != nil && s.reg != nil {
		s.cache.Start(ctx)
	}
	// Keep idle WebSocket connections alive with periodic pings.
	s.hub.Start(ctx)

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
		_ = s.server.Shutdown(context.Background())
	}()

	if s.server.TLSConfig != nil {
		return s.server.ListenAndServeTLS("", "")
	}
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

	w.Header().Set("Content-Type", "application/json")

	// Prefer the capability cache (has hydrated capabilities), fall back
	// to the registry so workers are visible even before capability fetch.
	var cached []protocol.WorkerInfo
	if s.cache != nil {
		cached = s.cache.List()
	}
	if len(cached) == 0 && s.reg != nil {
		cached = s.reg.ListAvailable()
	}

	workers := make([]WorkerInfo, 0, len(cached))
	for _, w := range cached {
		workers = append(workers, WorkerInfo{
			ID:           w.ID,
			Hostname:     w.Hostname,
			IP:           w.IP,
			Port:         w.Port,
			Status:       w.Status,
			Version:      w.Version,
			APIKey:       w.APIKey,
			LoadedModels: getLoadedModelNames(w.Capabilities.Models),
			LastSeen:     w.LastSeen,
		})
	}

	response := WorkersResponse{Workers: workers}
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

	if s.cache == nil {
		_ = json.NewEncoder(w).Encode(ModelsResponse{Object: "list", Data: []protocol.ModelInfo{}})
		return
	}

	// Get capabilities from cache
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var workers []protocol.WorkerInfo
	for _, w := range s.cache.cache {
		workers = append(workers, w)
	}

	// Aggregate models from all workers, only including loaded models.
	// Initialized as empty slice (not nil) so JSON encodes as [] not null.
	models := []protocol.ModelInfo{}
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

	// Validate API key if configured
	if s.cfg.APIKey != "" {
		auth := r.Header.Get("Authorization")
		if !strings.EqualFold(auth, "Bearer "+s.cfg.APIKey) {
			writeErrorResponse(w, http.StatusUnauthorized, "invalid or missing API key", "authentication_error", "api_key_error")
			return
		}
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
	if s.cache == nil {
		return protocol.WorkerInfo{}, fmt.Errorf("capability cache not configured")
	}
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

	if len(candidates) > 0 {
		// Simple round-robin selection (use timestamp for now)
		return candidates[0], nil
	}

	// Fallback: if no exact loaded-model match, route to any cached worker
	// that advertises the model (even if not marked loaded), then to any
	// available worker. This keeps dev-mode usable when backends report
	// catalogue models without load state (e.g. LM Studio /v1/models).
	for _, worker := range s.cache.cache {
		for _, m := range worker.Capabilities.Models {
			if m.Name == model {
				s.log.Warn("routing to worker with model not marked loaded", "model", model, "worker", worker.ID)
				return worker, nil
			}
		}
	}
	for _, worker := range s.cache.cache {
		s.log.Warn("no worker has requested model, falling back to available worker", "model", model, "worker", worker.ID)
		return worker, nil
	}

	return protocol.WorkerInfo{}, fmt.Errorf("no worker found with model %s loaded", model)
}

// clientFor returns a WorkerClient for the given worker, preferring the
// active WebSocket connection for WS workers and falling back to HTTP
// dial-back for legacy (mDNS/dev-HTTP) workers.
func (s *Server) clientFor(worker protocol.WorkerInfo) WorkerClient {
	if worker.Transport == protocol.TransportWS {
		if c := s.hub.Client(worker.ID); c != nil {
			return c
		}
		s.log.Warn("worker advertises websocket transport but has no active connection; falling back to http", "worker", worker.ID)
	}
	return newHTTPWorkerClient(s.log)
}

// loadModelOnWorker attempts to load a model on a worker.
func (s *Server) loadModelOnWorker(worker protocol.WorkerInfo, modelName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := s.clientFor(worker).LoadModel(ctx, worker, modelName)
	if err != nil {
		s.log.Warn("failed to load model on worker", "worker", worker.ID, "model", modelName, "error", err)
		return false
	}
	return ok
}

// proxyChatStream proxies a chat completion request to a worker.
func (s *Server) proxyChatStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req ChatRequest) {
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal chat request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}
	s.proxyStream(w, r, s.clientFor(worker), worker, "chat", body)
}

// proxyCompletionStream proxies a completion request to a worker.
func (s *Server) proxyCompletionStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req CompletionRequest) {
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal completion request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}
	s.proxyStream(w, r, s.clientFor(worker), worker, "completion", body)
}

// proxyStream relays an inference request to the worker via the transport
// appropriate for it and streams the response back to the client as SSE.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, client WorkerClient, worker protocol.WorkerInfo, kind string, body []byte) {
	// Apply context-based timeout (default 30s).
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	chunks, errs := client.Stream(ctx, worker, kind, body)
	flusher, _ := w.(http.Flusher)
	started := false
	sawDone := false

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				// Channel closed without an explicit Done sentinel (e.g. the
				// sentinel was dropped when the queue was full). Terminate the
				// SSE stream so clients don't wait forever.
				if started && !sawDone {
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
				}
				return
			}
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				started = true
			}
			sawDone = sawDone || chunk.Done
			_, _ = w.Write(chunk.Data)
			if flusher != nil {
				flusher.Flush()
			}
			if chunk.Done {
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if !started {
				var herr *WorkerHTTPError
				if errors.As(err, &herr) {
					s.log.Error("worker returned error", "worker", worker.ID, "status", herr.StatusCode, "body", herr.Body)
					writeErrorResponse(w, herr.StatusCode, "worker error", "server_error", "worker_error")
				} else if ctx.Err() == context.DeadlineExceeded {
					s.log.Error("request timed out after retries", "worker", worker.ID, "timeout", "30s")
					writeErrorResponse(w, http.StatusGatewayTimeout, "request timeout", "server_error", "timeout_error")
				} else {
					s.log.Error("failed to connect to worker after retries", "error", err)
					writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
				}
				return
			}
			s.log.Warn("stream interrupted", "worker", worker.ID, "error", err)
			if !sawDone {
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		case <-ctx.Done():
			return
		}
	}
}
