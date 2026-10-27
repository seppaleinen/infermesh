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
	chatHistory   []protocol.ChatRequest
	capabilities  *capabilities.Aggregator
	config        security.Config
	backend       Backend // Backend adapter for model inference
	healthTracker *ModelHealthTracker

	// concurrencyEstimator tracks backend 429s/latency to infer per-worker cap.
	// Nil when no backend is configured.
	concurrencyEstimator *ConcurrencyEstimator

	// inferenceStats records completed inference calls per model over a
	// rolling window and feeds the /capabilities payload so the router's
	// scheduler can rank workers by throughput (issue #134). Nil when no
	// backend is configured; handleCapabilities and the inference handlers
	// guard against nil.
	inferenceStats *InferenceTracker

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

// Wire-protocol types (ChatRequest, ChatMessage, Choice, ChatChunk,
// CompletionChunk, ModelsResponse, Usage, ChatResponse, CompletionResponse)
// live in pkg/protocol/api.go — see docs/adr/0004-code-quality-ceilings.md.
// The worker package references them as protocol.* throughout.

// NewServer creates a new worker HTTP server.
func NewServer(log *slog.Logger, addr string, cfg security.Config) *Server {
	agg := capabilities.NewAggregator(capabilities.Defaults(), log)
	tracker := NewModelHealthTracker(cfg.EnableHealthChecks)
	return &Server{
		log:            log,
		addr:           addr,
		models:         []protocol.ModelInfo{},
		chatHistory:    []protocol.ChatRequest{},
		capabilities:   agg,
		config:         cfg,
		backend:        nil,
		healthTracker:  tracker,
		inferenceStats: NewInferenceTracker(0),
	}
}

// SetBackend stores the backend directly. The old passthrough wrapper was
// removed as dead code (issue #159): its stored model field was never read
// because every call site passes the model from the request anyway.
func (s *Server) SetBackend(backend Backend, model string) {
	s.backend = backend

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

// SetInferenceWindow replaces the inference-stats tracker with one using the
// given rolling window. Startup-only: call before Start, matching the other
// config setters. A value <= 0 falls back to DefaultInferenceWindow (10 min)
// via NewInferenceTracker.
func (s *Server) SetInferenceWindow(d time.Duration) {
	s.inferenceStats = NewInferenceTracker(d)
}

// recordInference records a completed inference call for the given
// model into the inference tracker. It is a no-op when no backend is
// configured (inferenceStats is nil), so callers can invoke it
// unconditionally after a successful response.
func (s *Server) recordInference(model string, usage *protocol.Usage, ttft, total time.Duration) {
	if s.inferenceStats == nil || usage == nil {
		return
	}
	s.inferenceStats.Record(model, usage.PromptTokens, usage.CompletionTokens, ttft, total)
}

// recordStreamingInference records a completed streaming inference.
// When the backend's final frame carried a Usage, use its token counts;
// otherwise fall back to chunk count as a completion-token proxy (one
// content chunk ≈ one token) with zero prompt tokens, so the tracker
// still records a throughput sample for backends that omit usage.
func (s *Server) recordStreamingInference(model string, usage *protocol.Usage, chunkCount int, firstChunkAt time.Time) {
	if s.inferenceStats == nil {
		return
	}
	ttft := time.Since(firstChunkAt)
	if usage != nil {
		s.inferenceStats.Record(model, usage.PromptTokens, usage.CompletionTokens, ttft, ttft)
		return
	}
	s.inferenceStats.Record(model, 0, chunkCount, ttft, ttft)
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

	// Surface the worker's per-model inference throughput snapshot so the
	// router's capability cache can rank workers by tokens/sec (issue #134).
	// Empty when no inference has been recorded yet; the JSON encoder omits
	// the field when nil/empty, keeping the payload backward compatible.
	if s.inferenceStats != nil {
		worker.Capabilities.InferenceStats = s.inferenceStats.Snapshots()
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

// chatCompletions, completions, modelsList, loadModelByName, loadModelHandler
// and the shared streamSSE/dispatchNonStreaming helpers live in handlers.go.

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
