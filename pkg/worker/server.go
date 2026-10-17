package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// DefaultHealthCheckInterval is the default interval for health check loops.
const DefaultHealthCheckInterval = 30 * time.Second

// Server is the HTTP server for the worker.
type Server struct {
	log           *slog.Logger
	server        *http.Server
	addr          string
	models        []protocol.ModelInfo
	chatHistory   []ChatRequest
	capabilities  *capabilities.Aggregator
	config        security.Config
	backend       Backend // Backend adapter for model inference
	healthTracker *ModelHealthTracker

	// concurrencyEstimator tracks backend 429s/latency to infer per-worker cap.
	// Nil when no backend is configured.
	concurrencyEstimator *ConcurrencyEstimator

	// modelsMu protects concurrent access to the models field. The refresh
	// goroutine (SetModels/GetModels) and HTTP handlers (handleCapabilities,
	// metrics, modelsList, loadModelByName, loadModelHandler) may run
	// concurrently, so every read and write of s.models must hold this lock.
	modelsMu sync.Mutex

	// allowedModels is a whitelist of models this worker is WILLING to serve.
	// When non-empty, the router only schedules requests for models in this
	// list on this worker. Empty means "serve all".
	allowedModels []string
	// excludedModels is a blacklist of models this worker is NOT WILLING to
	// serve. Applies only when allowedModels is empty.
	excludedModels []string
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
	tracker := NewModelHealthTracker(cfg.EnableHealthChecks)
	return &Server{
		log:           log,
		addr:          addr,
		models:        []protocol.ModelInfo{},
		chatHistory:   []ChatRequest{},
		capabilities:  agg,
		config:        cfg,
		backend:       nil,
		healthTracker: tracker,
	}
}

// SetBackend sets the backend adapter for this server.
func (s *Server) SetBackend(backend Backend, model string) {
	s.backend = NewBackendAdapter(backend, model)

	// Initialize concurrency estimator, seeded with the backend's thread
	// count when available (llama-cpp). Backends without a thread concept
	// leave threads = 0, so the estimator falls back to defaultInitCap = 4
	// (backward compatible, issue #139).
	initCap := defaultInitCap
	if metrics, err := backend.GetMetrics(); err == nil && metrics.Threads > 0 {
		initCap = metrics.Threads
	}
	if e := NewConcurrencyEstimatorWithInit("/tmp/infermesh-concurrency-cache.json", initCap); e != nil {
		s.concurrencyEstimator = e
	}

	// Try to dynamically discover models from the backend
	if discoveredModels, err := backend.ListModels(); err == nil && len(discoveredModels) > 0 {
		s.log.Info("dynamically discovered models from backend",
			"backend", backend.Name(),
			"count", len(discoveredModels),
		)
		// Merge discovered models with existing models
		s.modelsMu.Lock()
		s.models = mergeModels(s.models, discoveredModels)
		s.modelsMu.Unlock()
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
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	s.models = models
}

// GetModels returns a copy of the models known to this worker.
func (s *Server) GetModels() []protocol.ModelInfo {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	out := make([]protocol.ModelInfo, len(s.models))
	copy(out, s.models)
	return out
}

// SetModelFilters configures the worker's model-serving opt-out filters.
// allowed is a whitelist: when non-empty the worker only serves models in
// this list. excluded is a blacklist: when allowed is empty the worker refuses
// models in this list. Both are stored as-is (nil when empty) so the JSON
// encoder omits them when unset.
func (s *Server) SetModelFilters(allowed, excluded []string) {
	s.allowedModels = allowed
	s.excludedModels = excluded
}

// ApplyModelFilters copies the server's model filters onto the given
// Capabilities struct. It is called after any reassignment of
// caps.AllowedModels/caps.ExcludedModels so the flags survive refresh closures
// that rebuild the Capabilities value (e.g. reassigning caps.Models).
func (s *Server) ApplyModelFilters(caps *protocol.Capabilities) {
	caps.AllowedModels = s.allowedModels
	caps.ExcludedModels = s.excludedModels
}

// Start runs the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/capabilities", s.handleCapabilities)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/completions", s.completions)
	mux.HandleFunc("/v1/models/load", s.loadModelHandler)
	mux.HandleFunc("/v1/models", s.modelsList)

	var handler http.Handler = mux

	// Per-endpoint API key check on inference endpoints (dev and prod).
	// Mirrors the router's wrapInferenceEndpoints: the middleware is a
	// no-op when APIKey is empty, so we can apply it unconditionally.
	if s.config.APIKey != "" {
		handler = s.wrapInferenceEndpoints(handler, s.config.APIKey)
	}

	if !security.IsDevMode(s.config) {
		// Prod mode: validate the TLS configuration before starting so we
		// fail fast with a clear error instead of crashing inside
		// ListenAndServeTLS with empty cert paths.
		caCertPath := filepath.Join(s.config.CertDir, "ca.crt")
		if err := security.ValidateTLSConfig(s.config.MTLSCert, s.config.MTLSKey, caCertPath); err != nil {
			return fmt.Errorf("invalid TLS configuration: %w", err)
		}

		// Load the CA cert pool for client-certificate verification.
		caCertPool := security.LoadCACertPool(caCertPath)

		// Wrap the handler with the mTLS middleware so the worker enforces
		// client-certificate verification and CN restriction at the HTTP
		// layer as well as the TLS layer. mTLS wraps the API-key-wrapped
		// handler so the ordering is: mTLS (innermost) -> API key (outermost).
		mtlsMW := security.NewMTLSMiddleware(caCertPool, true, security.SplitCNs(s.config.TrustedCNs)...)
		handler = mtlsMW(handler)

		// Load the worker's certificate for serving HTTPS.
		cert, err := security.LoadTLSCertFromFile(s.config.MTLSCert, s.config.MTLSKey)
		if err != nil {
			return fmt.Errorf("failed to load worker TLS certificate: %w", err)
		}

		s.server = &http.Server{
			Addr:    s.addr,
			Handler: handler,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    caCertPool,
			},
		}

		s.log.Info("worker server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.config), "tls", "enabled")

		// Bind explicitly on tcp4 BEFORE serving. http.ListenAndServe uses
		// net.Listen("tcp", addr), which on macOS creates a dual-stack IPv6
		// socket with IPV6_V6ONLY=0 under SO_REUSEADDR. When another process
		// (e.g. LM Studio) already holds the IPv4 half of that port, the bind
		// "succeeds" but the worker ends up serving IPv6-only: it advertises
		// its IPv4 address to the router, the router's CapabilityCache fetches
		// http://<ip>:<port>/capabilities, and the request lands on the other
		// process, which returns 404. The worker then looks model-less.
		//
		// Binding tcp4 first fails loudly if the port is taken, instead of
		// silently serving a half-dead endpoint.
		listener, err := net.Listen("tcp4", s.addr)
		if err != nil {
			return fmt.Errorf("worker failed to bind %s: %w (port held by another process?)", s.addr, err)
		}

		go func() {
			<-ctx.Done()
			_ = s.server.Shutdown(context.Background())
		}()

		if s.concurrencyEstimator != nil {
			go s.concurrencyEstimator.Start(ctx)
		}

		return s.server.ServeTLS(listener, "", "")
	}

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: handler,
	}

	s.log.Info("worker server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.config))

	// Bind explicitly on tcp4 BEFORE serving (see prod branch above for
	// the dual-stack / port-collision rationale).
	listener, err := net.Listen("tcp4", s.addr)
	if err != nil {
		return fmt.Errorf("worker failed to bind %s: %w (port held by another process?)", s.addr, err)
	}

	go func() {
		<-ctx.Done()
		_ = s.server.Shutdown(context.Background())
	}()

	if s.concurrencyEstimator != nil {
		go s.concurrencyEstimator.Start(ctx)
	}

	return s.server.Serve(listener)
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string {
	if s.server != nil {
		return s.server.Addr
	}
	return s.addr
}

// wrapInferenceEndpoints wraps the given handler so that inference endpoints
// (/v1/chat/completions, /v1/completions) require a valid API key while
// other endpoints pass through unchanged. When apiKey is empty the
// middleware is a no-op pass-through (dev mode).
func (s *Server) wrapInferenceEndpoints(next http.Handler, apiKey string) http.Handler {
	apiKeyMW := security.NewAPIKeyMiddleware(apiKey, apiKey != "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions", "/v1/completions":
			apiKeyMW(next).ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
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
	s.modelsMu.Lock()
	models := make([]protocol.ModelInfo, len(s.models))
	copy(models, s.models)
	s.modelsMu.Unlock()
	worker.Capabilities.Models = append(worker.Capabilities.Models, models...)

	// Surface the worker's model opt-out filters so the router's
	// CapabilityCache hydrates them and the scheduler can honour them.
	s.ApplyModelFilters(&worker.Capabilities)

	if s.concurrencyEstimator != nil {
		worker.Capabilities.ConcurrencyCap = s.concurrencyEstimator.Cap()
	}
	if s.backend != nil {
		if metrics, err := s.backend.GetMetrics(); err == nil && metrics.Threads > 0 {
			worker.Capabilities.Threads = metrics.Threads
		}
	}

	if err := json.NewEncoder(w).Encode(worker); err != nil {
		s.log.Error("failed to encode capabilities", "error", err)
		http.Error(w, "failed to encode capabilities", http.StatusInternalServerError)
	}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.modelsMu.Lock()
	count := len(s.models)
	s.modelsMu.Unlock()
	metrics := "# HELP workers_total Total number of registered workers\n"
	metrics += "# TYPE workers_total gauge\n"
	metrics += "workers_total 1\n"
	metrics += "# HELP models_total Total number of available models\n"
	metrics += "# TYPE models_total gauge\n"
	metrics += fmt.Sprintf("models_total %d\n", count)
	_, _ = w.Write([]byte(metrics))
}

// isBackendTimeout reports whether err is a context deadline exceeded or a
// timeout error, indicating the backend took too long to respond. This covers
// both absolute deadlines (http.Client.Timeout) and idle timeouts (reset on
// activity) emitted by the streaming backends.
func isBackendTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "idle timeout") ||
		strings.Contains(msg, "TLS handshake timeout") ||
		strings.Contains(msg, "i/o timeout")
}

// writeStreamBackendError handles a backend error that arrived during a
// streaming request. If no chunk has been flushed yet (started == false) the
// response is still unwritten, so we emit a proper JSON error body with the
// appropriate HTTP status instead of opening an SSE stream with a malformed
// frame. If a chunk has already been flushed (started == true) we terminate
// the stream cleanly with data: [DONE] and do NOT emit the bogus error frame.
func (s *Server) writeStreamBackendError(w http.ResponseWriter, err error, started bool) {
	if !started {
		status := http.StatusBadGateway
		if isBackendTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	s.log.Error("backend error mid-stream", "error", err)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ensureSSEHeaders sets the text/event-stream headers on the first write of a
// streaming response. It is idempotent: calling it again after headers have
// already been flushed is a no-op, so it is safe to call from both the
// first-chunk path and the clean-close path.
func ensureSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// chatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Parse request
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse chat request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Store in history for debugging (capped at maxChatHistory to prevent unbounded growth)
	const maxChatHistory = 1000
	s.chatHistory = append(s.chatHistory, req)
	if len(s.chatHistory) > maxChatHistory {
		s.chatHistory = s.chatHistory[len(s.chatHistory)-maxChatHistory:]
	}

	// Check if backend is configured
	if s.backend == nil {
		s.log.Error("backend not configured")
		http.Error(w, "backend not configured", http.StatusInternalServerError)
		return
	}

	// Check circuit breaker state
	if !s.backend.IsHealthy() {
		if s.concurrencyEstimator != nil {
			s.concurrencyEstimator.RecordCircuitOpen()
		}
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Non-streaming: return the full response as application/json.
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		start := time.Now()
		chatResp, err := s.backend.CompleteChat(ctx, req.Model, req)
		if s.concurrencyEstimator != nil {
			if err != nil {
				var bhErr *BackendHTTPError
				if errors.As(err, &bhErr) {
					s.concurrencyEstimator.RecordRequest(start, bhErr.StatusCode)
				} else {
					s.concurrencyEstimator.RecordRequest(start, 0)
				}
			} else {
				s.concurrencyEstimator.RecordRequest(start, http.StatusOK)
			}
		}
		if err != nil {
			s.log.Error("backend error", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(chatResp); err != nil {
			s.log.Error("failed to encode response", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		return
	}

	// Streaming: forward ChatChunks as OpenAI-compatible SSE frames.
	// Headers are set lazily on the first flushed chunk so that a backend
	// error arriving before any chunk can still produce a proper JSON error
	// response (see writeStreamBackendError).
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	chatCh, errCh := s.backend.StreamChat(ctx, req.Model, req)
	started := false
	for {
		// Prioritise an already-buffered backend error: on early failure the
		// real backends close BOTH channels, so a plain select could randomly
		// pick the closed chunk channel and mask the error.
		var pendingErr error
		select {
		case err, ok := <-errCh:
			if ok && err != nil {
				pendingErr = err
			}
			// ok == false: errCh closed without error — keep waiting for chunks.
		default:
		}
		if pendingErr != nil {
			s.writeStreamBackendError(w, pendingErr, started)
			return
		}

		select {
		case chunk, ok := <-chatCh:
			if !ok {
				// The chunk channel closed. The real backends close errCh
				// before chatCh and buffer the error before either close, so
				// check errCh one last time to catch an error that raced with
				// the close.
				select {
				case err, ok := <-errCh:
					if ok && err != nil {
						s.writeStreamBackendError(w, err, started)
						return
					}
				default:
				}
				if !started {
					ensureSSEHeaders(w)
				}
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if !started {
				ensureSSEHeaders(w)
				started = true
			}
			jsonChunk, err := json.Marshal(chunk)
			if err != nil {
				s.log.Error("failed to marshal chunk", "error", err)
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(jsonChunk))
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// completions handles the /v1/completions HTTP endpoint.
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
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
		if s.concurrencyEstimator != nil {
			s.concurrencyEstimator.RecordCircuitOpen()
		}
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Non-streaming: return the full response as application/json.
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		start := time.Now()
		compResp, err := s.backend.CompleteCompletions(ctx, req.Model, req)
		if s.concurrencyEstimator != nil {
			if err != nil {
				var bhErr *BackendHTTPError
				if errors.As(err, &bhErr) {
					s.concurrencyEstimator.RecordRequest(start, bhErr.StatusCode)
				} else {
					s.concurrencyEstimator.RecordRequest(start, 0)
				}
			} else {
				s.concurrencyEstimator.RecordRequest(start, http.StatusOK)
			}
		}
		if err != nil {
			s.log.Error("backend error", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(compResp); err != nil {
			s.log.Error("failed to encode response", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		return
	}

	// Streaming: forward CompletionChunks as OpenAI-compatible SSE frames.
	// Headers are set lazily on the first flushed chunk so that a backend
	// error arriving before any chunk can still produce a proper JSON error
	// response (see writeStreamBackendError).
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	compCh, errCh := s.backend.StreamCompletions(ctx, req.Model, req)
	started := false
	for {
		// Prioritise an already-buffered backend error: on early failure the
		// real backends close BOTH channels, so a plain select could randomly
		// pick the closed chunk channel and mask the error.
		var pendingErr error
		select {
		case err, ok := <-errCh:
			if ok && err != nil {
				pendingErr = err
			}
			// ok == false: errCh closed without error — keep waiting for chunks.
		default:
		}
		if pendingErr != nil {
			s.writeStreamBackendError(w, pendingErr, started)
			return
		}

		select {
		case chunk, ok := <-compCh:
			if !ok {
				// The chunk channel closed. The real backends close errCh
				// before compCh and buffer the error before either close, so
				// check errCh one last time to catch an error that raced with
				// the close.
				select {
				case err, ok := <-errCh:
					if ok && err != nil {
						s.writeStreamBackendError(w, err, started)
						return
					}
				default:
				}
				if !started {
					ensureSSEHeaders(w)
				}
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if !started {
				ensureSSEHeaders(w)
				started = true
			}
			jsonChunk, err := json.Marshal(chunk)
			if err != nil {
				s.log.Error("failed to marshal chunk", "error", err)
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(jsonChunk))
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// modelsList handles the /v1/models HTTP endpoint.
func (s *Server) modelsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.modelsMu.Lock()
	data := make([]protocol.ModelInfo, len(s.models))
	copy(data, s.models)
	s.modelsMu.Unlock()
	// make([]T, n) is never nil, so no nil-guard is needed here; an empty
	// s.models yields an empty (non-nil) slice, which JSON encodes as [].
	response := ModelsResponse{Object: "list", Data: data}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		http.Error(w, "failed to encode models", http.StatusInternalServerError)
	}
}

// loadModelByName marks a declared model as loaded, mirroring the HTTP
// /v1/models/load handler. It is used by the WebSocket model_load_request
// dispatch; the next heartbeat carries the updated models so the router's
// capability cache sees the load.
func (s *Server) loadModelByName(name string) (bool, string) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	for i, m := range s.models {
		if m.Name == name {
			s.models[i].Loaded = true
			return true, ""
		}
	}
	return false, "model not found"
}

// loadModelHandler handles the /v1/models/load HTTP endpoint.
func (s *Server) loadModelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	found := false
	s.modelsMu.Lock()
	for i, m := range s.models {
		if m.Name == req.Model {
			found = true
			// Update loaded flag – simple in-memory state change.
			s.models[i].Loaded = true
			break
		}
	}
	s.modelsMu.Unlock()

	if !found {
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{"status": "loaded"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// StartHealthCheckLoop periodically checks the backend health and records the result.
func (s *Server) StartHealthCheckLoop(ctx context.Context, interval time.Duration) {
	if s.healthTracker == nil || !s.healthTracker.GetEnabled() {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.backend == nil {
				continue
			}

			if err := s.backend.HealthCheck(); err != nil {
				// Basic health checks intentionally pass zeroed metrics; detailed
				// operational metrics are captured by the backend's GetMetrics()
				// and are not part of the health check path.
				s.healthTracker.RecordHealthCheck(Unhealthy, ModelMetrics{})
				s.log.Warn("backend health check failed", "error", err)
			} else {
				// Basic health checks intentionally pass zeroed metrics; detailed
				// operational metrics are captured by the backend's GetMetrics()
				// and are not part of the health check path.
				s.healthTracker.RecordHealthCheck(Healthy, ModelMetrics{})
				s.log.Info("backend health check passed")
			}
			state := s.backend.GetCircuitState()
			if state == CircuitOpen && s.concurrencyEstimator != nil {
				s.concurrencyEstimator.RecordCircuitOpen()
			} else if state == CircuitClosed && s.concurrencyEstimator != nil {
				s.concurrencyEstimator.RecordCircuitClosed()
			}
		}
	}
}
