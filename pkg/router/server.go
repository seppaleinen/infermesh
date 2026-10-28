package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/internal/registry"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
	"github.com/seppaleinen/infermesh/pkg/security"
	"log/slog"
)

// autoModelAlias is the logical model name that triggers auto routing.
// Requests with this model name are classified by prompt complexity and
// rewritten to a tiered concrete model before worker selection.
const autoModelAlias = "auto"

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
	// Protected by scorerWeightsMu; replaced atomically in the same critical
	// section as scorerWeights.
	scorer          scheduler.ScoringAlgorithm
	scorerWeights   schedulerWeights
	scorerWeightsMu sync.RWMutex

	// loadSnapshot returns per-worker queue-load data for scoring. Defaults to
	// reading the hub; tests may override it with deterministic values.
	loadSnapshot func() map[string]WorkerQueueStats

	// busyLoadSnapshot returns the per-worker in-flight snapshot used by the
	// busy-status overlay in handleWorkersList. Defaults to reading the hub;
	// tests may override it with deterministic values.
	busyLoadSnapshot func() map[string]WorkerQueueStats

	// proxyTimeout bounds how long proxyStream waits for a worker response
	// before emitting a 504. Defaults to 120s; tests override it to drive the
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

	// modelLoadTimeout bounds how long the router waits for a worker's
	// /v1/models/load HTTP POST before considering the load attempt failed.
	// Defaults to 120s; tests may override it to drive the 504 path without
	// sleeping. See SetModelLoadTimeout.
	modelLoadTimeout time.Duration

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
	// Protected by autoRoutingMu.
	autoRouting   *AutoRoutingConfig
	autoRoutingMu sync.RWMutex

	// modelAliases maps user-facing model aliases to canonical model names.
	// When nil or empty, alias resolution is disabled and requests are
	// routed with the literal model name the client sent (backward
	// compatible). When active, any request model that is an alias key is
	// rewritten to its Canonical before worker selection.
	modelAliases   *ModelAliasConfig
	modelAliasesMu sync.RWMutex

	// connections is the count of currently active HTTP connections to the
	// router. It is the number of open TCP connections, NOT a distinct-client
	// count: a single client using HTTP keep-alive holds one connection while
	// issuing many requests, and a client behind a connection pool or proxy
	// may hold several. Incremented on http.StateNew, decremented on
	// http.StateClosed. Kept alive across keep-alive (StateIdle) and hijacked
	// (StateHijacked) connections so the count reflects live sockets, not
	// in-flight requests.
	connections atomic.Int64

	// tracker records every dispatched inference request (model + worker +
	// startedAt + status) for the GET /meta/requests/inflight visibility
	// endpoint (issue #156). Nil-safe: every call site checks for nil so a
	// minimal test server that never calls NewServer's full constructor is
	// unaffected.
	tracker *RequestTracker
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
		tracker:      NewRequestTracker(0, 0),
		proxyTimeout: 120 * time.Second,
		scorer:       scorer,
		scorerWeights: schedulerWeights{
			QuantMatch:       scorer.QuantMatchWeight,
			VRAMFree:         scorer.VRAMFreeWeight,
			GPUUtil:          scorer.GPUUtilWeight,
			QueueDepth:       scorer.QueueDepthWeight,
			Latency:          scorer.LatencyWeight,
			LoadedModelBonus: scorer.LoadedModelBonusWeight,
			ColdLoadPenalty:  scorer.ColdLoadPenaltyWeight,
			MaxQueueDepth:    scorer.MaxQueueDepth(),
			// ThroughputWeight defaults to 0.0 (disabled) so the built-in
			// scorer is byte-for-byte identical to before this feature.
		},
	}
	srv.loadSnapshot = srv.hubLoadSnapshot
	srv.busyLoadSnapshot = srv.hubBusySnapshot
	// Default retry/failover configuration.
	srv.maxAttempts = 3
	srv.retryBudget = 60 * time.Second
	srv.proxyTimeout = 120 * time.Second
	srv.modelLoadTimeout = 120 * time.Second
	// clientFactory defaults to nil; clientFor() provides the default
	// implementation when it is nil.  Tests may override it to inject a mock.
	return srv
}

// SetRelayURL configures the relay WebSocket URL for outbound-only mode.
func (s *Server) SetRelayURL(url string) {
	s.hub.SetRelayURL(url)
}

// SetAPIKey sets the API key for inference endpoint authentication.
// Overrides any key set at construction time.
func (s *Server) SetAPIKey(key string) {
	s.hub.SetAPIKey(key)
}

// SetMaxInFlight overrides the per-worker concurrent-call cap. A value <= 0
// falls back to defaultMaxInFlight.
func (s *Server) SetMaxInFlight(n int) {
	s.hub.SetMaxInFlight(n)
}

// SetWorkerMaxInFlight overrides the max-in-flight cap for a specific worker.
func (s *Server) SetWorkerMaxInFlight(workerID string, cap int) {
	s.hub.SetWorkerMaxInFlight(workerID, cap)
}

// SetMaxConnections overrides the total worker WebSocket connection cap. A
// value <= 0 falls back to defaultMaxConnections.
func (s *Server) SetMaxConnections(n int) {
	s.hub.SetMaxConnections(n)
}

// SetProxyTimeout overrides the per-dispatch timeout applied by proxyStream.
// A value <= 0 falls back to 120s. Used by tests to drive the 504 path
// without sleeping.
func (s *Server) SetProxyTimeout(d time.Duration) {
	if d <= 0 {
		d = 120 * time.Second
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

// SetModelLoadTimeout overrides the HTTP client timeout used by loadModelOnWorker
// when the router POSTs /v1/models/load to a worker. A value <= 0 falls back to
// 120s. Used by tests to drive the timeout path without sleeping.
func (s *Server) SetModelLoadTimeout(d time.Duration) {
	if d <= 0 {
		d = 120 * time.Second
	}
	s.modelLoadTimeout = d
}

// SetAutoRouting configures the model="auto" alias. When cfg is nil or
// has no tier models configured, the alias is disabled and "auto" is
// treated as a literal model name (backward compatible).
//
// The alias intercepts requests with model="auto" before worker
// selection, classifies prompt complexity, and rewrites the model to a
// tiered concrete model name.
func (s *Server) SetAutoRouting(cfg *AutoRoutingConfig) {
	s.autoRoutingMu.Lock()
	defer s.autoRoutingMu.Unlock()
	s.autoRouting = cfg
}

// autoRoutingActiveLocked is the caller-holds-RLock form of the auto-routing
// check. It is used inside resolveAutoModel* where the caller already holds
// autoRoutingMu to avoid a re-entrant lock. Delegates to the package-level
// autoRoutingActive helper so the check has a single implementation.
func (s *Server) autoRoutingActiveLocked() bool {
	return autoRoutingActive(s.autoRouting)
}

// resolveAutoModel rewrites a request's model field when it equals the
// "auto" alias and auto routing is enabled. It returns the concrete
// model name to dispatch on, or an error when the classified tier has
// no configured model. Non-"auto" models are returned unchanged.
//
// This is called before marshal & dispatch so the rest of the pipeline
// sees a concrete model name and requires no changes.
func (s *Server) resolveAutoModelChat(req *protocol.ChatRequest) (string, error) {
	s.autoRoutingMu.RLock()
	defer s.autoRoutingMu.RUnlock()

	if req.Model != autoModelAlias || !s.autoRoutingActiveLocked() {
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
func (s *Server) resolveAutoModelCompletion(req *protocol.CompletionRequest) (string, error) {
	s.autoRoutingMu.RLock()
	defer s.autoRoutingMu.RUnlock()

	if req.Model != autoModelAlias || !s.autoRoutingActiveLocked() {
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
	s.modelAliasesMu.Lock()
	defer s.modelAliasesMu.Unlock()
	s.modelAliases = cfg
}

// modelAliasesActive reports whether alias resolution is enabled and at
// least one alias has a non-empty Canonical.
func (s *Server) modelAliasesActive() bool {
	s.modelAliasesMu.RLock()
	defer s.modelAliasesMu.RUnlock()
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
	s.modelAliasesMu.RLock()
	defer s.modelAliasesMu.RUnlock()
	if !modelAliasesActive(s.modelAliases) {
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
	QuantMatch       float64
	VRAMFree         float64
	GPUUtil          float64
	QueueDepth       float64
	Latency          float64
	LoadedModelBonus float64
	ColdLoadPenalty  float64
	MaxQueueDepth    int
	// ThroughputWeight is the weight for the worker's advertised tokens/sec
	// in the weighted scoring formula (issue #134). Zero (default) keeps the
	// throughput component out of the weighted sum, so the default scorer
	// is byte-for-byte identical to before this feature.
	ThroughputWeight float64
}

// SetScorerWeights updates the weighted scorer used by selectWorker. A zero
// value for any weight keeps that weight at its current value, so an all-zero
// call is a no-op and the built-in defaults survive; maxQueueDepth <= 0 keeps
// the current threshold (defaultMaxQueueDepth when unset).
func (s *Server) SetScorerWeights(quantMatch, vramFree, gpuUtil, queueDepth, latency, loadedModelBonus, coldLoadPenalty float64, maxQueueDepth int) {
	s.scorerWeightsMu.Lock()
	defer s.scorerWeightsMu.Unlock()

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
	if loadedModelBonus > 0 {
		w.LoadedModelBonus = loadedModelBonus
	}
	if coldLoadPenalty > 0 {
		w.ColdLoadPenalty = coldLoadPenalty
	}
	if maxQueueDepth > 0 {
		w.MaxQueueDepth = maxQueueDepth
	}
	s.scorerWeights = w
	s.scorer = scheduler.NewWeightedScorerWithConfig(
		w.QuantMatch, w.VRAMFree, w.GPUUtil, w.QueueDepth, w.Latency,
		w.LoadedModelBonus, w.ColdLoadPenalty, w.ThroughputWeight,
		scheduler.DefaultUnavailableTTL(), w.MaxQueueDepth,
	)
}

// ScorerWeights returns the current scorer weight configuration.
func (s *Server) ScorerWeights() schedulerWeights {
	s.scorerWeightsMu.RLock()
	defer s.scorerWeightsMu.RUnlock()
	return s.scorerWeights
}

// scorerWeightsEnabled reports whether the loaded-model bonus or cold-load
// penalty is active. When either is > 0, the scorer accepts unloaded workers
// and ranks them with the bonus/penalty spread. When both are 0 (default),
// the scorer rejects unloaded workers (backward compatible).
func (s *Server) scorerWeightsEnabled() bool {
	s.scorerWeightsMu.RLock()
	defer s.scorerWeightsMu.RUnlock()
	return s.scorerWeights.LoadedModelBonus > 0 || s.scorerWeights.ColdLoadPenalty > 0
}

// SetScorerThroughputWeight sets the weight for the worker's advertised
// tokens/sec in the weighted scoring formula (issue #134). A value <= 0
// keeps the current value, which defaults to 0.0 (throughput disabled,
// backward compatible). It is exposed separately from SetScorerWeights so
// the CLI can set it without re-specifying the other weights.
func (s *Server) SetScorerThroughputWeight(weight float64) {
	s.scorerWeightsMu.Lock()
	defer s.scorerWeightsMu.Unlock()

	w := s.scorerWeights
	if weight > 0 {
		w.ThroughputWeight = weight
	}
	s.scorerWeights = w
	s.scorer = scheduler.NewWeightedScorerWithConfig(
		w.QuantMatch, w.VRAMFree, w.GPUUtil, w.QueueDepth, w.Latency,
		w.LoadedModelBonus, w.ColdLoadPenalty, w.ThroughputWeight,
		scheduler.DefaultUnavailableTTL(), w.MaxQueueDepth,
	)
}

// currentScorer returns the scoring algorithm in use, read under
// scorerWeightsMu so it is consistent with ScorerWeights(). The returned
// interface is immutable after construction (NewWeightedScorer* never mutates
// its weights), so callers may retain it safely.
func (s *Server) currentScorer() scheduler.ScoringAlgorithm {
	s.scorerWeightsMu.RLock()
	defer s.scorerWeightsMu.RUnlock()
	return s.scorer
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
	mux.HandleFunc("/v1/inference-stats", s.handleInferenceStats)
	mux.HandleFunc("/v1/dev/register", s.handleDevRegister)
	mux.HandleFunc("/meta/models/popular", s.handlePopularModels)
	mux.HandleFunc("/meta/aliases", s.handleSetAliases)
	mux.HandleFunc("/meta/auto-routing", s.handleAutoRouting)
	mux.HandleFunc("/meta/connections/count", s.handleConnectionCount)
	mux.HandleFunc("/meta/requests/inflight", s.handleInflightRequests)
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

	// Start the request tracker's periodic prune loop so the finished-request
	// ring buffer stays bounded.
	if s.tracker != nil {
		s.tracker.Start(ctx)
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
		// middleware (applied after this block) handles external clients.
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

		// TLS config is needed for the server; build it now.
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{*cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    caCertPool,
		}

		// Build the server (TLS or plain) after handler is fully wrapped.
		s.server = &http.Server{
			Addr:      s.addr,
			Handler:   handler,
			TLSConfig: tlsConfig,
		}
	} else {
		// Dev mode: no mTLS.
		s.server = &http.Server{
			Addr:    s.addr,
			Handler: handler,
		}
	}

	// Per-endpoint API key check on inference endpoints (dev and prod).
	// Hoisted after the mode branch so the guard appears once (issue #82).
	// In prod, mTLS already wraps mux (innermost), so API key wraps
	// outermost to preserve original prod ordering: mTLS -> API key.
	// In dev, API key wraps mux directly.
	if s.cfg.APIKey != "" {
		handler = s.wrapInferenceEndpoints(handler, s.cfg.APIKey)
	}

	// The server's handler must point to the final wrapped handler.
	s.server.Handler = handler

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

// hubLoadSnapshot returns a per-worker queue-load snapshot from the hub, taken
