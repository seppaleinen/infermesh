package router

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
	"github.com/seppaleinen/infermesh/pkg/security"
	"log/slog"
)

// PopularModelInfo represents a model with popularity signals for the
// /meta/models/popular endpoint.
type PopularModelInfo struct {
	Model             string `json:"model"`
	WorkerCount       int    `json:"worker_count"`
	LoadedWorkerCount int    `json:"loaded_worker_count"`
	CallCount         int    `json:"call_count"`
}

// PopularModelsResponse is the response from the /meta/models/popular endpoint.
type PopularModelsResponse struct {
	Models []PopularModelInfo `json:"models"`
}

// autoModelAlias is the logical model name that triggers auto routing.
// Requests with this model name are classified by prompt complexity and
// rewritten to a tiered concrete model before worker selection.
const autoModelAlias = "auto"

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

// writeRateLimitError writes an OpenAI-shaped 429 rate-limit response with a
// Retry-After header. It is written BEFORE SSE headers are flushed so the
// client sees a clean JSON error rather than a half-open SSE stream.
func writeRateLimitError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)

	errorResp := ErrorResponse{
		Error: ErrorDetail{
			Message: "rate limit exceeded: max in-flight calls reached for this worker",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	}

	if err := json.NewEncoder(w).Encode(errorResp); err != nil {
		slog.Error("failed to encode rate limit error response", "error", err)
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
	log     *slog.Logger
	reg     registry.Registry
	addr    string
	cache   *CapabilityCache
	cfg     security.Config
	hub     *WSHub
	server  *http.Server
	counter *CallCounter

	// scorer is the deterministic weighted scoring algorithm used by
	// selectWorker. Never nil after NewServer; tests may replace it.
	scorer        scheduler.ScoringAlgorithm
	scorerWeights schedulerWeights

	// loadSnapshot returns per-worker queue-load data for scoring. Defaults to
	// reading the hub; tests may override it with deterministic values.
	loadSnapshot func() map[string]WorkerQueueStats

	// proxyTimeout bounds how long proxyStream waits for a worker response
	// before emitting a 504. Defaults to 30s; tests override it to drive the
	// 504 path without sleeping. See SetProxyTimeout.
	proxyTimeout time.Duration

	// maxAttempts bounds how many worker attempts dispatchWithFailover makes
	// for a single non-streaming client request (1 = no failover). Defaults to
	// 3; overridden by SetMaxAttempts / the --max-attempts CLI flag.
	maxAttempts int

	// retryBudget is the global deadline applied across all failover attempts
	// for a single non-streaming client request. Defaults to 60s; overridden
	// by SetRetryBudget / the --retry-budget CLI flag.
	retryBudget time.Duration

	// clientFactory returns the WorkerClient used to talk to a worker. Defaults
	// to clientFor; tests override it to inject deterministic errors into the
	// failover loop without spinning up real worker servers.
	clientFactory func(protocol.WorkerInfo) WorkerClient

	// routerClientCert is the router's own client certificate, presented to
	// workers over mTLS in prod mode. Nil in dev mode.
	routerClientCert *tls.Certificate
	// workerCAPool is the CA pool used to verify worker certificates. Nil in
	// dev mode.
	workerCAPool *x509.CertPool

	// autoRouting configures the model="auto" alias. When nil, "auto" is
	// treated as a literal model name (backward compatible). When set,
	// requests with model="auto" are classified by prompt complexity and
	// rewritten to a tiered concrete model before worker selection.
	autoRouting *AutoRoutingConfig

	// modelAliases maps user-facing model aliases to canonical model names.
	// When nil or empty, alias resolution is disabled and requests are
	// routed with the literal model name the client sent (backward
	// compatible). When active, any request model that is an alias key is
	// rewritten to its Canonical before worker selection.
	modelAliases *ModelAliasConfig

	// connections is the count of currently active HTTP connections to the
	// router. It is the number of open TCP connections, NOT a distinct-client
	// count: a single client using HTTP keep-alive holds one connection while
	// issuing many requests, and a client behind a connection pool or proxy
	// may hold several. Incremented on http.StateNew, decremented on
	// http.StateClosed. Kept alive across keep-alive (StateIdle) and hijacked
	// (StateHijacked) connections so the count reflects live sockets, not
	// in-flight requests.
	connections atomic.Int64
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
	scorer := scheduler.NewWeightedScorer()
	srv := &Server{
		log:          log,
		reg:          reg,
		addr:         addr,
		cache:        cache,
		cfg:          cfg,
		hub:          hub,
		counter:      NewCallCounter(10 * time.Minute),
		proxyTimeout: 30 * time.Second,
		scorer:       scorer,
		scorerWeights: schedulerWeights{
			QuantMatch:    scorer.QuantMatchWeight,
			VRAMFree:      scorer.VRAMFreeWeight,
			GPUUtil:       scorer.GPUUtilWeight,
			QueueDepth:    scorer.QueueDepthWeight,
			Latency:       scorer.LatencyWeight,
			MaxQueueDepth: scorer.MaxQueueDepth(),
		},
	}
	srv.loadSnapshot = srv.hubLoadSnapshot
	// Default retry/failover configuration.
	srv.maxAttempts = 3
	srv.retryBudget = 60 * time.Second
	srv.clientFactory = srv.clientFor
	return srv
}

// SetRelayURL configures the relay WebSocket URL for outbound-only mode.
func (s *Server) SetRelayURL(url string) {
	s.hub.SetRelayURL(url)
}

// SetMaxInFlight overrides the per-worker concurrent-call cap. A value <= 0
// falls back to defaultMaxInFlight.
func (s *Server) SetMaxInFlight(n int) {
	s.hub.SetMaxInFlight(n)
}

// SetMaxConnections overrides the total worker WebSocket connection cap. A
// value <= 0 falls back to defaultMaxConnections.
func (s *Server) SetMaxConnections(n int) {
	s.hub.SetMaxConnections(n)
}

// SetProxyTimeout overrides the per-dispatch timeout applied by proxyStream.
// A value <= 0 falls back to 30s. Used by tests to drive the 504 path
// without sleeping.
func (s *Server) SetProxyTimeout(d time.Duration) {
	if d <= 0 {
		d = 30 * time.Second
	}
	s.proxyTimeout = d
}

// SetMaxAttempts overrides the maximum number of worker attempts
// dispatchWithFailover makes for a single non-streaming client request.
// A value <= 1 disables failover (single attempt, current behavior).
// A value > 1 enables failover with up to maxAttempts total attempts.
// Values <= 0 fall back to the default (3).
func (s *Server) SetMaxAttempts(n int) {
	if n <= 0 {
		n = 3
	}
	s.maxAttempts = n
}

// SetRetryBudget overrides the global deadline applied across all failover
// attempts for a single non-streaming request. A value <= 0 falls back to 60s.
func (s *Server) SetRetryBudget(d time.Duration) {
	if d <= 0 {
		d = 60 * time.Second
	}
	s.retryBudget = d
}

// SetAutoRouting configures the model="auto" alias. When cfg is nil or
// has no tier models configured, the alias is disabled and "auto" is
// treated as a literal model name (backward compatible).
//
// The alias intercepts requests with model="auto" before worker
// selection, classifies prompt complexity, and rewrites the model to a
// tiered concrete model name.
func (s *Server) SetAutoRouting(cfg *AutoRoutingConfig) {
	s.autoRouting = cfg
}

// autoRoutingActive reports whether the model="auto" alias is enabled
// and has at least one tier model configured.
func (s *Server) autoRoutingActive() bool {
	if s.autoRouting == nil || s.autoRouting.Models == nil {
		return false
	}
	for _, m := range s.autoRouting.Models {
		if m != "" {
			return true
		}
	}
	return false
}

// resolveAutoModel rewrites a request's model field when it equals the
// "auto" alias and auto routing is enabled. It returns the concrete
// model name to dispatch on, or an error when the classified tier has
// no configured model. Non-"auto" models are returned unchanged.
//
// This is called before marshal & dispatch so the rest of the pipeline
// sees a concrete model name and requires no changes.
func (s *Server) resolveAutoModelChat(req *ChatRequest) (string, error) {
	if req.Model != autoModelAlias || !s.autoRoutingActive() {
		return req.Model, nil
	}
	tier := s.autoRouting.Classify(*req)
	model, ok := s.autoRouting.ResolveModel(tier)
	if !ok {
		return "", fmt.Errorf("no model configured for complexity tier %s", tier)
	}
	s.log.Debug("auto-routed chat request", "tier", tier, "model", model, "tokens", s.autoRouting.estimateChatTokens(*req))
	return model, nil
}

// resolveAutoModelCompletion is the completion-requests counterpart of
// resolveAutoModelChat.
func (s *Server) resolveAutoModelCompletion(req *CompletionRequest) (string, error) {
	if req.Model != autoModelAlias || !s.autoRoutingActive() {
		return req.Model, nil
	}
	tier := s.autoRouting.ClassifyCompletion(*req)
	model, ok := s.autoRouting.ResolveModel(tier)
	if !ok {
		return "", fmt.Errorf("no model configured for complexity tier %s", tier)
	}
	s.log.Debug("auto-routed completion request", "tier", tier, "model", model, "tokens", estimateTokens(req.Prompt))
	return model, nil
}

// SetModelAliases configures user-facing model aliases. When cfg is nil or
// has no aliases with a non-empty Canonical, alias resolution is disabled
// and requests are routed with the literal model name the client sent
// (backward compatible).
//
// The alias intercepts requests before worker selection: any request model
// that is an alias key is rewritten to its Canonical so the rest of the
// pipeline sees the concrete model name and requires no changes.
func (s *Server) SetModelAliases(cfg *ModelAliasConfig) {
	s.modelAliases = cfg
}

// modelAliasesActive reports whether alias resolution is enabled and at
// least one alias has a non-empty Canonical.
func (s *Server) modelAliasesActive() bool {
	return modelAliasesActive(s.modelAliases)
}

// resolveAliasModel rewrites a request's model field when it matches a
// configured alias key. It returns the Canonical name when active and the
// model is a key with a non-empty Canonical; otherwise it returns the model
// unchanged. It never errors — an unresolvable alias is simply passed
// through so the scheduler surfaces its usual no_workers error.
//
// This is called after the model="auto" resolution and before marshal &
// dispatch so the rest of the pipeline sees the canonical model name.
func (s *Server) resolveAliasModel(model string) string {
	if !s.modelAliasesActive() {
		return model
	}
	target, ok := s.modelAliases.Aliases[model]
	if !ok || target.Canonical == "" {
		return model
	}
	s.log.Debug("resolved model alias", "alias", model, "canonical", target.Canonical)
	return target.Canonical
}

// schedulerWeights holds the configurable weighted-scorer settings exposed via
// CLI flags. Zero values mean "keep current" when passed to SetScorerWeights.
type schedulerWeights struct {
	QuantMatch    float64
	VRAMFree      float64
	GPUUtil       float64
	QueueDepth    float64
	Latency       float64
	MaxQueueDepth int
}

// SetScorerWeights updates the weighted scorer used by selectWorker. A zero
// value for any weight keeps that weight at its current value, so an all-zero
// call is a no-op and the built-in defaults survive; maxQueueDepth <= 0 keeps
// the current threshold (defaultMaxQueueDepth when unset).
func (s *Server) SetScorerWeights(quantMatch, vramFree, gpuUtil, queueDepth, latency float64, maxQueueDepth int) {
	w := s.scorerWeights
	if quantMatch > 0 {
		w.QuantMatch = quantMatch
	}
	if vramFree > 0 {
		w.VRAMFree = vramFree
	}
	if gpuUtil > 0 {
		w.GPUUtil = gpuUtil
	}
	if queueDepth > 0 {
		w.QueueDepth = queueDepth
	}
	if latency > 0 {
		w.Latency = latency
	}
	if maxQueueDepth > 0 {
		w.MaxQueueDepth = maxQueueDepth
	}
	s.scorerWeights = w
	s.scorer = scheduler.NewWeightedScorerWithConfig(
		w.QuantMatch, w.VRAMFree, w.GPUUtil, w.QueueDepth, w.Latency,
		scheduler.DefaultUnavailableTTL(), w.MaxQueueDepth,
	)
}

// ScorerWeights returns the current scorer weight configuration.
func (s *Server) ScorerWeights() schedulerWeights {
	return s.scorerWeights
}

// RunRelay maintains the outbound relay connection, dialing immediately and
// re-dialing with exponential backoff after any disconnect until ctx is
// cancelled. It blocks; call it in a goroutine. The router's HTTP server
// keeps serving while only the relay link retries.
func (s *Server) RunRelay(ctx context.Context, url string) {
	s.hub.runRelay(ctx, url)
}

// Start runs the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/workers", s.handleWorkersList)
	mux.HandleFunc("/v1/dev/register", s.handleDevRegister)
	mux.HandleFunc("/meta/models/popular", s.handlePopularModels)
	mux.HandleFunc("/meta/connections/count", s.handleConnectionCount)
	mux.HandleFunc("/v1/queue/stats", s.handleQueueStats)

	// Outbound WebSocket worker connectivity: workers dial ws://router:8080/v1/connect
	// Only mount when NOT in relay mode — in relay mode, Handler() returns nil
	// (workers connect via the relay instead of directly to the router).
	if h := s.hub.Handler(); h != nil {
		mux.Handle("/v1/connect", h)
	}

	// Start the capability cache event loop so registry events populate
	// the cache that /v1/models, /v1/workers and worker selection read from.
	// Without this, dev HTTP registrations land in the registry but the
	// cache stays empty → empty workers list, null models, no_workers.
	if s.cache != nil && s.reg != nil {
		s.cache.Start(ctx)
	}

	// Start the call counter's periodic prune loop so the rolling 10m
	// window stays bounded.
	if s.counter != nil {
		s.counter.Start(ctx)
	}

	var handler http.Handler = mux

	if !security.IsDevMode(s.cfg) {
		// Prod mode: validate the TLS configuration before starting so we
		// fail fast with a clear error instead of crashing inside
		// ListenAndServe with an invalid TLS config.
		caCertPath := filepath.Join(s.cfg.CertDir, "ca.crt")
		if err := security.ValidateTLSConfig(s.cfg.MTLSCert, s.cfg.MTLSKey, caCertPath); err != nil {
			return fmt.Errorf("invalid TLS configuration: %w", err)
		}

		// Load the CA cert pool for client-certificate verification.
		caCertPool := security.LoadCACertPool(caCertPath)

		// Wrap the handler with the mTLS middleware so the router enforces
		// client-certificate verification and CN restriction at the HTTP
		// layer. This replaces the raw tls.Config that was never enforced.
		// The mTLS middleware handles worker-facing endpoints; the API key
		// middleware (wrapped outside this block) handles external clients.
		mtlsMW := security.NewMTLSMiddleware(caCertPool, true, security.SplitCNs(s.cfg.TrustedCNs)...)
		handler = mtlsMW(mux)

		// Load the router's certificate for serving HTTPS.
		cert, err := security.LoadTLSCertFromFile(s.cfg.MTLSCert, s.cfg.MTLSKey)
		if err != nil {
			return fmt.Errorf("failed to load router TLS certificate: %w", err)
		}

		// Cache the cert + CA pool so the router→worker dial-back client can
		// present the router's cert and verify worker certs over mTLS.
		s.routerClientCert = cert
		s.workerCAPool = caCertPool

		s.server = &http.Server{
			Addr:    s.addr,
			Handler: handler,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    caCertPool,
			},
		}
	} else {
		// Dev mode: no mTLS.
		s.server = &http.Server{
			Addr:    s.addr,
			Handler: handler,
		}
	}

	// Per-endpoint API key check on inference endpoints (dev and prod).
	// Hoisted out of the mode branch so the guard appears once (issue #82).
	// In prod, mTLS is already wrapped around mux (innermost), so API key
	// wraps outermost to preserve original prod ordering: mTLS -> API key.
	if s.cfg.APIKey != "" {
		handler = s.wrapInferenceEndpoints(handler, s.cfg.APIKey)
	}

	// Attach the ConnState hook so the active-connection counter tracks
	// live sockets. StateNew increments, StateClosed decrements. Keep-alive
	// (StateIdle) and hijacked (StateHijacked) connections are NOT
	// decremented: they remain open sockets and must continue to count.
	s.server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			s.connections.Add(1)
		case http.StateClosed:
			s.connections.Add(-1)
		}
	}

	s.log.Info("router server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.cfg))

	go func() {
		<-ctx.Done()
		_ = s.server.Shutdown(context.Background())
	}()

	// Prod mode: enforce the TLS configuration. ListenAndServeTLS activates
	// the http.Server.TLSConfig (RequireAndVerifyClientCert + CA pool) so
	// the mTLS middleware actually sees r.TLS. Without this the server
	// served plain HTTP and every request was rejected with 403.
	if !security.IsDevMode(s.cfg) {
		return s.server.ListenAndServeTLS(s.cfg.MTLSCert, s.cfg.MTLSKey)
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

// wrapInferenceEndpoints wraps the given handler so that inference endpoints
// (/v1/chat/completions, /v1/completions) require a valid API key while
// other endpoints pass through unchanged.
func (s *Server) wrapInferenceEndpoints(next http.Handler, apiKey string) http.Handler {
	// required mirrors the worker's wrapInferenceEndpoints: a key is only
	// enforced when one is actually configured. Without this, passing an
	// empty apiKey would enforce a key against "" and reject every request.
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

// handleChatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

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

	// Resolve the model="auto" alias (if enabled) to a concrete tiered
	// model before marshal & dispatch. Non-auto models are returned
	// unchanged, preserving existing routing behavior.
	if model, err := s.resolveAutoModelChat(&req); err != nil {
		s.log.Error("auto routing failed", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, err.Error(), "server_error", "auto_routing_error")
		return
	} else {
		req.Model = model
	}

	// Resolve any user-facing model alias to its canonical name after the
	// "auto" alias so aliases can target tiered models too.
	req.Model = s.resolveAliasModel(req.Model)

	// Marshal the request body once for dispatch.
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal chat request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}

	if req.Stream {
		// Streaming: fail fast, no failover (cannot undo partial SSE output).
		worker, err := s.selectWorker(req.Model)
		if err != nil {
			s.log.Error("failed to select worker", "error", err)
			writeErrorResponse(w, http.StatusServiceUnavailable, "no workers", "server_error", "no_workers")
			return
		}
		if s.counter != nil {
			s.counter.Record(req.Model)
		}
		if s.hub != nil && s.hub.isRateLimited(worker.ID) {
			s.hub.recordRejection(worker.ID, 429)
			writeRateLimitError(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		s.proxyChatStream(w, r, worker, req)
		return
	}

	// Non-streaming: dispatch with failover to a different worker on
	// transport/5xx/429 errors. The response is buffered until the first
	// successful attempt so nothing is written to the client until then.
	resp, err := s.dispatchWithFailover(r.Context(), req.Model, body, "chat", s.maxAttempts, s.retryBudget)
	if err != nil {
		s.writeFailoverError(w, req.Model, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleCompletions handles the /v1/completions HTTP endpoint.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

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

	// Resolve the model="auto" alias (if enabled) to a concrete tiered
	// model before marshal & dispatch.
	if model, err := s.resolveAutoModelCompletion(&req); err != nil {
		s.log.Error("auto routing failed", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, err.Error(), "server_error", "auto_routing_error")
		return
	} else {
		req.Model = model
	}

	// Resolve any user-facing model alias to its canonical name after the
	// "auto" alias so aliases can target tiered models too.
	req.Model = s.resolveAliasModel(req.Model)

	// Marshal the request body once for dispatch.
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal completion request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}

	if req.Stream {
		// Streaming: fail fast, no failover (cannot undo partial SSE output).
		worker, err := s.selectWorker(req.Model)
		if err != nil {
			s.log.Error("failed to select worker", "error", err)
			writeErrorResponse(w, http.StatusServiceUnavailable, "no workers", "server_error", "no_workers")
			return
		}
		if s.counter != nil {
			s.counter.Record(req.Model)
		}
		if s.hub != nil && s.hub.isRateLimited(worker.ID) {
			s.hub.recordRejection(worker.ID, 429)
			writeRateLimitError(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		s.proxyCompletionStream(w, r, worker, req)
		return
	}

	// Non-streaming: dispatch with failover to a different worker on
	// transport/5xx/429 errors. The response is buffered until the first
	// successful attempt so nothing is written to the client until then.
	resp, err := s.dispatchWithFailover(r.Context(), req.Model, body, "completion", s.maxAttempts, s.retryBudget)
	if err != nil {
		s.writeFailoverError(w, req.Model, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
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

	// When auto routing is enabled, advertise the synthetic "auto" alias so
	// OpenAI-compatible clients discover it alongside concrete models.
	if s.autoRoutingActive() {
		models = append(models, protocol.ModelInfo{
			Name:      autoModelAlias,
			Backend:   "auto",
			MaxTokens: 0,
			Loaded:    true,
		})
	}

	// Advertise configured user-facing aliases so clients can request them
	// by their short name and have the router rewrite them to the canonical
	// model before worker selection. Only aliases with a non-empty Canonical
	// are advertised — a dangling alias key would advertise a model that
	// resolves to nothing.
	if s.modelAliasesActive() {
		for alias, target := range s.modelAliases.Aliases {
			if target.Canonical == "" {
				continue
			}
			models = append(models, protocol.ModelInfo{
				Name:      alias,
				Backend:   "alias",
				MaxTokens: 0,
				Loaded:    true,
			})
		}
	}

	response := ModelsResponse{Object: "list", Data: models}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "failed to encode models", "server_error", "encoding_error")
	}
}

// handlePopularModels handles the /meta/models/popular endpoint.
func (s *Server) handlePopularModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// 1. Aggregate worker_count from capability cache (model appears in
	//    Capabilities.Models regardless of Loaded).
	// 2. Merge call_count from s.counter.Snapshot().
	// 3. Sort by a total order (no ties) so repeated polls do not shuffle
	//    equal-ranked models: loaded_worker_count desc, worker_count desc,
	//    call_count desc, model name asc. SliceStable is belt-and-suspenders;
	//    the comparator is already a total order.
	// 4. Encode PopularModelsResponse.

	// Get capabilities from cache
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	counts := s.counter.Snapshot()

	// Map to accumulate counts per model
	// Key: model name, Value: struct with counts
	type modelStats struct {
		model       string
		workerCount int
		loadedCount int
		callCount   int64
	}
	statsMap := make(map[string]*modelStats)

	// Iterate through all workers in cache to get model availability and counts
	for _, worker := range s.cache.cache {
		for _, m := range worker.Capabilities.Models {
			if _, ok := statsMap[m.Name]; !ok {
				statsMap[m.Name] = &modelStats{
					model:     m.Name,
					callCount: counts[m.Name],
				}
			}
			statsMap[m.Name].workerCount++
			if m.Loaded {
				statsMap[m.Name].loadedCount++
			}
		}
	}

	// Convert map to slice
	popularModels := make([]PopularModelInfo, 0, len(statsMap))
	for _, stat := range statsMap {
		popularModels = append(popularModels, PopularModelInfo{
			Model:             stat.model,
			WorkerCount:       stat.workerCount,
			LoadedWorkerCount: stat.loadedCount,
			CallCount:         int(stat.callCount),
		})
	}

	// Sort by loaded first, then model name (alphabetical).
	// Stable: loaded_worker_count is the only thing that changes
	// the order, so a refresh updates contents without reordering.
	sort.SliceStable(popularModels, func(i, j int) bool {
		a, b := popularModels[i], popularModels[j]
		if a.LoadedWorkerCount != b.LoadedWorkerCount {
			return a.LoadedWorkerCount > b.LoadedWorkerCount
		}
		return a.Model < b.Model
	})

	if err := json.NewEncoder(w).Encode(PopularModelsResponse{Models: popularModels}); err != nil {
		s.log.Error("failed to encode popular models", "error", err)
	}
}

// handleConnectionCount handles the /meta/connections/count HTTP endpoint. It reports
// the number of currently active HTTP connections to the router — the count
// of open TCP sockets, NOT a distinct-client count. A single client issuing
// many requests over one keep-alive connection holds one connection; a
// client behind a connection pool or proxy may hold several.
func (s *Server) handleConnectionCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		Connections int `json:"connections"`
	}{Connections: int(s.connections.Load())}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode client count", "error", err)
	}
}

// handleQueueStats handles the /v1/queue/stats HTTP endpoint.
func (s *Server) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if s.hub == nil {
		writeErrorResponse(w, http.StatusInternalServerError, "queue stats not configured", "server_error", "internal_error")
		return
	}

	resp := s.hub.QueueStats()
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode queue stats", "error", err)
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

	// Dev mode: derive the routable IP from the peer address (authoritative),
	// ignoring any client-provided value.
	var peerIP string
	if security.IsDevMode(s.cfg) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			// Same-host peer: a localhost dial may land over IPv6 loopback (::1),
			// which has no IPv4 form. Normalize any loopback peer to the
			// canonical routable IPv4 loopback address.
			ip = net.IPv4(127, 0, 0, 1)
		}
		if ip == nil || ip.To4() == nil {
			writeErrorResponse(w, http.StatusBadRequest, "unable to determine routable worker IPv4 from remote address", "validation_error", "ip_validation_error")
			return
		}
		providedIP := worker.IP
		worker.IP = ip.String()
		peerIP = worker.IP // trusted peer identity for collision check
		if providedIP != "" && providedIP != worker.IP {
			s.log.Info("dev worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port, "provided_ip", providedIP)
		} else {
			s.log.Info("dev worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port)
		}
	} else {
		// Prod mode: keep the legacy loopback-only check, unchanged.
		if worker.IP != "127.0.0.1" && worker.IP != "localhost" {
			writeErrorResponse(w, http.StatusForbidden, "non-loopback IP not allowed", "authentication_error", "ip_validation_error")
			return
		}
		peerIP = worker.IP // in prod mode the body-provided IP is authoritative (loopback only)
	}

	// Collision check: reject registration if this ID is already claimed
	// by a worker from a different peer IP. This prevents worker ID spoofing
	// / registry poisoning: a malicious worker cannot overwrite another
	// worker's registry entry by registering with the victim's ID.
	if existing, ok := s.reg.Get(worker.ID); ok {
		if existing.IP != peerIP {
			s.log.Warn("rejected duplicate worker ID from different peer",
				"id", worker.ID, "existing_ip", existing.IP, "peer_ip", peerIP)
			writeErrorResponse(w, http.StatusConflict, "worker ID already registered by another peer",
				"authentication_error", "duplicate_id")
			return
		}
		// Same peer IP: allow re-registration (legitimate reconnect/restart)
		s.log.Info("worker re-registered from same peer", "id", worker.ID, "peer_ip", peerIP)
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

// buildCandidates builds the list of candidate workers that have the
// requested model loaded, attempting auto-load when no loaded candidate
// is found. The returned slice is deterministically ordered by worker ID
// (lowest ID first) and is safe to use outside the cache lock.
// Caller must hold no lock; the function acquires the cache RLock internally.
func (s *Server) buildCandidates(model string) []protocol.WorkerInfo {
	if s.cache == nil {
		return nil
	}
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var candidates []protocol.WorkerInfo
	for _, worker := range s.cache.cache {
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

	// Deterministic ordering so equal scores resolve to the lowest worker ID.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates
}

// selectWorkerExcluding selects a worker for the given model, excluding any
// workers whose IDs are in the provided set. This is used by the failover
// loop to avoid retrying the same worker. The exclusion set is respected
// during both the primary candidate selection and the fallback paths.
func (s *Server) selectWorkerExcluding(model string, excluded map[string]struct{}) (protocol.WorkerInfo, error) {
	if s.cache == nil {
		return protocol.WorkerInfo{}, fmt.Errorf("capability cache not configured")
	}

	candidates := s.buildCandidates(model)

	// Filter out excluded workers before scoring.
	if len(excluded) > 0 {
		filtered := make([]protocol.WorkerInfo, 0, len(candidates))
		for _, w := range candidates {
			if _, ok := excluded[w.ID]; !ok {
				filtered = append(filtered, w)
			}
		}
		candidates = filtered
	}

	// Apply model opt-out filters (AllowedModels/ExcludedModels) to the
	// loaded-model candidates. Workers that opted out of the requested model
	// must not be considered, even if they have the model loaded.
	if s.cache != nil {
		s.cache.mu.RLock()
		// Build scheduler view for FilterByAllowedModels
		schedCands := make([]scheduler.WorkerInfo, 0, len(candidates))
		for _, w := range candidates {
			schedCands = append(schedCands, s.toSchedulerWorker(w, WorkerQueueStats{}, false))
		}
		s.cache.mu.RUnlock()

		schedCands = scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, schedCands)

		// Map back to protocol workers by ID (unique in cache)
		filteredProto := make([]protocol.WorkerInfo, 0, len(schedCands))
		for _, sc := range schedCands {
			for _, pc := range candidates {
				if pc.ID == sc.ID {
					filteredProto = append(filteredProto, pc)
					break
				}
			}
		}
		candidates = filteredProto
	}

	if len(candidates) > 0 {
		// Deterministic ordering before scoring so equal scores resolve to the
		// lowest worker ID.
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		if s.scorer != nil {
			sel, err := s.scoreCandidates(model, candidates, s.loadSnapshot())
			if err == nil && sel != nil {
				for _, w := range candidates {
					if w.ID == sel.ID {
						s.log.Debug("selected worker via weighted scorer", "model", model, "worker", w.ID, "score", sel.Score)
						return w, nil
					}
				}
			} else if err != nil {
				s.log.Warn("weighted scorer failed, falling back to deterministic selection", "model", model, "error", err)
			}
		}
		return candidates[0], nil
	}

	// Fallback: if no exact loaded-model match, route to any cached worker
	// that advertises the model (even if not marked loaded), then to any
	// available worker. Respect exclusion in both cases.
	//
	// The model opt-out filters (AllowedModels/ExcludedModels) are applied
	// here too. Without this, a worker that opted out of the requested model
	// would still be selected as a last-resort fallback, silently bypassing
	// the whitelist/blacklist (e.g. requesting model-z on a fleet where every
	// worker whitelists a different model would route to an unwilling worker
	// instead of returning no_workers).
	if s.cache != nil {
		s.cache.mu.RLock()
		defer s.cache.mu.RUnlock()

		// Build the scheduler's view of every non-excluded cached worker so
		// FilterByAllowedModels (which operates on scheduler.WorkerInfo) can
		// honour the opt-out flags. The protocol snapshot is kept alongside
		// the sched view so the returned value carries the full cached
		// worker; the two slices are kept in lockstep.
		allProto := make([]protocol.WorkerInfo, 0, len(s.cache.cache))
		allSched := make([]scheduler.WorkerInfo, 0, len(s.cache.cache))
		for _, worker := range s.cache.cache {
			if _, ok := excluded[worker.ID]; ok {
				continue
			}
			allProto = append(allProto, worker)
			allSched = append(allSched, s.toSchedulerWorker(worker, WorkerQueueStats{}, false))
		}

		// 1. Any worker advertising the model (loaded or not) that is willing
		//    to serve it. FilterByAllowedModels preserves input order, so the
		//    returned sched entries map 1:1 onto the proto entries.
		adProto := make([]protocol.WorkerInfo, 0, len(allProto))
		adSched := make([]scheduler.WorkerInfo, 0, len(allSched))
		for i := range allProto {
			for _, m := range allProto[i].Capabilities.Models {
				if m.Name == model {
					adProto = append(adProto, allProto[i])
					adSched = append(adSched, allSched[i])
					break
				}
			}
		}
		adSched = scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, adSched)
		if len(adSched) > 0 {
			// FilterByAllowedModels preserves input order, but the first
			// surviving sched entry may not be the first advertising proto
			// entry (an unwilling advertiser is skipped). Map back by ID,
			// which is unique within the cache.
			selectedID := adSched[0].ID
			s.log.Warn("routing to worker with model not marked loaded", "model", model, "worker", selectedID)
			for i := range adProto {
				if adProto[i].ID == selectedID {
					return adProto[i], nil
				}
			}
		}

		// 2. Last resort: any available worker willing to serve the model.
		//    Unfiltered workers (empty lists) remain eligible; workers that
		//    explicitly opted out are excluded, so a fully opted-out fleet
		//    returns ErrNoMatchingWorkers rather than forcing the request
		//    onto an unwilling worker.
		anySched := scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, allSched)
		if len(anySched) > 0 {
			sort.Slice(anySched, func(i, j int) bool { return anySched[i].ID < anySched[j].ID })
			s.log.Warn("no worker has requested model, falling back to available worker", "model", model, "worker", anySched[0].ID)
			// Find the proto snapshot matching the selected sched worker.
			for i := range allProto {
				if allProto[i].ID == anySched[0].ID {
					return allProto[i], nil
				}
			}
		}
	}

	return protocol.WorkerInfo{}, scheduler.ErrNoMatchingWorkers
}

// selectWorker selects a worker for the given model using the default
// (empty) exclusion set. This delegates to selectWorkerExcluding for
// implementation reuse.
func (s *Server) selectWorker(model string) (protocol.WorkerInfo, error) {
	return s.selectWorkerExcluding(model, nil)
}

// isRetryableError classifies an error as retryable (should trigger failover
// to the next worker) or non-retryable (fail fast). Retryable errors include
// transport failures, timeouts, 5xx/429 worker responses, and deadline
// exceeded; non-retryable includes 4xx (except 429), parse errors, and
// scheduler exhaustion sentinels.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Explicitly non-retryable first.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, scheduler.ErrNoMatchingWorkers) ||
		errors.Is(err, scheduler.ErrAllWorkersUnavailable) {
		return false
	}
	// Retryable sentinels.
	if errors.Is(err, errTimeout) {
		return true
	}
	if errors.Is(err, errConnClosed) {
		return true
	}
	if errors.Is(err, errRelayDisconnected) {
		return true
	}
	if errors.Is(err, errRateLimited) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Worker HTTP errors: 5xx and 429 are retryable; other 4xx are not.
	var herr *WorkerHTTPError
	if errors.As(err, &herr) {
		switch {
		case herr.StatusCode >= 500:
			return true
		case herr.StatusCode == http.StatusTooManyRequests:
			return true
		default:
			return false
		}
	}
	return false
}

// errAllAttemptsFailed is a sentinel error returned when all failover
// attempts have been exhausted. It wraps the last underlying error for
// diagnostic purposes.
var errAllAttemptsFailed = errors.New("all failover attempts exhausted")

// dispatchWithFailover dispatches a non-streaming inference request with
// automatic failover across workers. It returns the successful worker's
// response body or an error after exhausting all attempts or the retry
// budget. The response body is raw (unprocessed) and should be written
// with Content-Type: application/json by the caller.
func (s *Server) dispatchWithFailover(ctx context.Context, model string, body []byte, kind string, maxAttempts int, budget time.Duration) ([]byte, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	excluded := make(map[string]struct{})
	var lastErr error
	recorded := false

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Respect the retry budget.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errAllAttemptsFailed, ctx.Err())
		}

		worker, err := s.selectWorkerExcluding(model, excluded)
		if err != nil {
			lastErr = err
			if attempt == maxAttempts {
				break
			}
			continue
		}

		// Record this inference dispatch for the popularity counter exactly
		// once per client request (not per attempt).
		if !recorded && s.counter != nil {
			s.counter.Record(model)
			recorded = true
		}

		// Pre-dispatch rate-limit check: a worker at its MaxInFlight cap
		// cannot take the call right now; treat it as a retryable rejection
		// and try the next best worker instead of failing the request outright.
		if s.hub != nil && s.hub.isRateLimited(worker.ID) {
			s.hub.recordRejection(worker.ID, http.StatusTooManyRequests)
			excluded[worker.ID] = struct{}{}
			lastErr = errRateLimited
			continue
		}

		resp, err := s.clientFactory(worker).Complete(ctx, worker, kind, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if isRetryableError(err) {
			excluded[worker.ID] = struct{}{}
			continue
		}
		// Non-retryable error: fail fast.
		return nil, err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no worker available for model %s", model)
	}
	return nil, fmt.Errorf("%w: %w", errAllAttemptsFailed, lastErr)
}

// writeFailoverError maps a failover error to an appropriate HTTP response
// and writes it to the client. It is called after the failover loop has
// exhausted all attempts or hit a non-retryable error.
func (s *Server) writeFailoverError(w http.ResponseWriter, model string, err error) {
	if err == nil {
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return
	}
	// All attempts exhausted sentinel.
	if errors.Is(err, errAllAttemptsFailed) {
		writeErrorResponse(w, http.StatusServiceUnavailable, "all failover attempts exhausted", "server_error", "all_attempts_failed")
		return
	}
	// Retry budget exceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		writeErrorResponse(w, http.StatusGatewayTimeout, "request timed out after retries", "server_error", "timeout_error")
		return
	}
	// Worker HTTP errors: pass through their status code.
	var herr *WorkerHTTPError
	if errors.As(err, &herr) {
		writeErrorResponse(w, herr.StatusCode, "worker error", "server_error", "worker_error")
		return
	}
	// Transport / relay errors.
	if errors.Is(err, errTimeout) {
		writeErrorResponse(w, http.StatusGatewayTimeout, "request timed out", "server_error", "timeout_error")
		return
	}
	if errors.Is(err, errConnClosed) || errors.Is(err, errRelayDisconnected) {
		writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
		return
	}
	// Scheduler exhaustion / no workers.
	writeErrorResponse(w, http.StatusServiceUnavailable, "no workers available for model "+model, "server_error", "no_workers")
}

// hubLoadSnapshot returns a per-worker queue-load snapshot from the hub, taken
// once per selection so every candidate sees consistent load data. Returns an
// empty map when no hub is configured (e.g. minimal test servers).
func (s *Server) hubLoadSnapshot() map[string]WorkerQueueStats {
	load := make(map[string]WorkerQueueStats)
	if s.hub == nil {
		return load
	}
	for _, ws := range s.hub.QueueStats().Workers {
		load[ws.WorkerID] = ws
	}
	return load
}

// toSchedulerWorker converts a cached protocol worker into the scheduler's
// view, attaching queue depth and wait latency from the hub snapshot when
// available. Unknown signals are left negative so the scorer treats them as
// neutral (workers do not push live GPU utilization today).
func (s *Server) toSchedulerWorker(w protocol.WorkerInfo, load WorkerQueueStats, haveLoad bool) scheduler.WorkerInfo {
	sw := scheduler.WorkerInfo{
		ID:            w.ID,
		Addr:          fmt.Sprintf("%s:%d", w.IP, w.Port),
		GPUUtilPct:    -1, // unknown; scorer treats as neutral
		QueueDepth:    -1, // unknown until the load snapshot says otherwise
		AvgLatencyMS:  -1, // unknown until the load snapshot says otherwise
		LastHeartbeat: w.LastSeen,
	}
	if w.Capabilities.VRAM.TotalMB > 0 {
		sw.VRAMTotalMB = int(w.Capabilities.VRAM.TotalMB)
		sw.VRAMFreeMB = int(w.Capabilities.VRAM.FreeMB)
	}
	sw.Models = append(sw.Models, w.Capabilities.Models...)
	// Carry the worker's model opt-out filters so the scheduler can honour
	// them during selection.
	sw.AllowedModels = append(sw.AllowedModels, w.Capabilities.AllowedModels...)
	sw.ExcludedModels = append(sw.ExcludedModels, w.Capabilities.ExcludedModels...)
	if haveLoad {
		sw.QueueDepth = int(load.QueueDepth)
		if load.AvgWaitMs >= 0 {
			sw.AvgLatencyMS = load.AvgWaitMs
		}
	}
	return sw
}

// scoreCandidates runs the weighted scorer over candidates and returns the best
// worker. Candidates must already be deterministically ordered (by ID) so equal
// scores resolve to the lowest ID.
func (s *Server) scoreCandidates(model string, candidates []protocol.WorkerInfo, load map[string]WorkerQueueStats) (*scheduler.SelectedWorker, error) {
	workers := make([]scheduler.WorkerInfo, 0, len(candidates))
	for _, w := range candidates {
		l, ok := load[w.ID]
		workers = append(workers, s.toSchedulerWorker(w, l, ok))
	}
	return scheduler.SelectWorker(context.Background(), s.scorer, scheduler.ModelRequest{Model: model}, workers, s.scorer.GetUnavailableTTL())
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
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK
}

// clientFor returns a WorkerClient for the given worker, preferring the
// active WebSocket connection for WS workers and falling back to HTTP
// dial-back for legacy (mDNS/dev-HTTP) workers.
func (s *Server) clientFor(worker protocol.WorkerInfo) WorkerClient {
	if worker.Transport == protocol.TransportWS {
		if c := s.hub.Client(worker.ID); c != nil {
			return c
		}
		// No active connection yet (e.g. worker just registered, reader loop
		// hasn't started). Return a relayWorkerClient that sends via the hub's
		// relaySendCh; the writer loop will pick it up once the relay is up.
		return newRelayWorkerClient(s.hub, s.log)
	}
	s.log.Warn("worker advertises websocket transport but has no active connection; falling back to http", "worker", worker.ID)
	if s.routerClientCert != nil && s.workerCAPool != nil {
		return newHTTPWorkerClientWithCerts(s.log, s.routerClientCert, s.workerCAPool)
	}
	return newHTTPWorkerClient(s.log)
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
//
// The dispatch is bounded by the per-request context plus proxyTimeout. Real
// worker clients surface errTimeout on the errCh channel when the dispatch
// context expires, so the 504 branch below handles the timeout and bumps the
// per-worker 504 counter (issue #54). Matching on the sentinel rather than on
// ctx.Err() is deliberate: a client may surface the timeout before the
// server-side context has actually expired, in which case ctx.Err() would
// still be nil and the 504 branch would be skipped. The ctx.Done() case is
// intentionally absent for the same reason. Clients that never surface a
// timeout error must close chunkCh themselves to terminate the stream.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, client WorkerClient, worker protocol.WorkerInfo, kind string, body []byte) {
	// Apply context-based timeout (default 30s, overridable via SetProxyTimeout).
	timeout := s.proxyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
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
				} else if errors.Is(err, errTimeout) {
					s.log.Error("request timed out after retries", "worker", worker.ID, "timeout", timeout)
					client.RecordTimeout(worker.ID)
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
		}
	}
}
