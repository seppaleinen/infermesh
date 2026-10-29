package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// ValidateRunConfig checks the RunConfig for valid combinations of settings.
// It catches common misconfigurations that would cause silent registration
// failures or security issues:
//   - Relay URL in prod mode (relay is dev-only)
//   - RouterBase set with non-loopback IP in prod mode
//   - Conflicting settings (relay URL + router base with both non-empty → relay takes precedence)
func ValidateRunConfig(cfg RunConfig) error {
	// Relay URL is dev-mode only.
	if !cfg.DevMode && cfg.RelayURL != "" {
		return fmt.Errorf("relay URL is only supported in dev mode (got --relay-url=%q in prod mode)", cfg.RelayURL)
	}
	// In prod mode, RouterBase must be loopback (the router's prod loopback
	// check requires worker.IP == 127.0.0.1). A non-loopback RouterBase in
	// prod mode would fail the router's registration check.
	if !cfg.DevMode && cfg.RouterBase != "" {
		if host, err := extractHost(cfg.RouterBase); err == nil && !isLoopback(host) {
			return fmt.Errorf("prod mode router base must be loopback (got %q); use --prod-router for mTLS registration", cfg.RouterBase)
		}
	}
	// Conflicting settings are allowed; relay takes precedence at runtime.
	return nil
}

// extractHost parses a URL and returns its host component (host:port stripped).
func extractHost(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host in %q", rawURL)
	}
	return host, nil
}

// isLoopback reports whether host is a loopback address (127.0.0.1, ::1, or "localhost").
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// RunConfig holds all configuration needed to run a worker.
type RunConfig struct {
	// ModelPath is the declared model file path (optional; models can be
	// discovered dynamically from the backend or declared via SetModels).
	ModelPath string
	// Backend selects the backend adapter (llama-cpp, ollama, vllm, lmstudio, custom).
	Backend string
	// BackendURL overrides the backend adapter's base URL (e.g. for an
	// OpenAI-compatible mock or a non-default llama-cpp endpoint).
	BackendURL string
	// Port is the HTTP server port.
	Port int
	// DevMode enables dev mode (no auth, loopback registration).
	DevMode bool
	// MTLSCert, MTLSKey, and CertDir are mTLS settings for prod mode.
	MTLSCert string
	MTLSKey  string
	CertDir  string
	// TrustedCNs is a comma-separated list of allowed client certificate CNs
	// for mTLS verification (empty = any CA-signed cert).
	TrustedCNs string
	// RouterBase is the router base URL for dev-mode HTTP registration
	// (e.g. http://127.0.0.1:8080). Empty means mDNS discovery.
	RouterBase string
	// ProdRouter is the router base URL for prod-mode mTLS registration
	// (e.g. https://127.0.0.1:8080). When set and DevMode is false the
	// worker registers over mTLS instead of mDNS. The router's prod
	// loopback check requires the advertised IP to be 127.0.0.1.
	ProdRouter string
	// RelayURL is the relay WebSocket URL for outbound-only connectivity.
	// When set, the worker uses WebSocket registration via the relay instead
	// of HTTP registration or mDNS. Only supported in dev mode.
	RelayURL string
	// EnableHealthChecks toggles periodic backend health checks.
	EnableHealthChecks bool
	// MaxTokens is the default maximum number of tokens to generate when the
	// incoming request doesn't specify max_tokens. Defaults to 4096.
	MaxTokens int
	// ModelRefreshInterval is the cadence for re-querying the backend's
	// ListModels() so models that start after the worker (e.g. LM Studio,
	// Ollama, vLLM) surface without a restart. 0 uses the default (10s).
	// The user is expected to raise this to 5min once the pool is stable.
	ModelRefreshInterval time.Duration
	// HealthCheckInterval is the cadence for backend health checks.
	// 0 uses the default (30s).
	HealthCheckInterval time.Duration
	// InferenceWindow is the rolling window for per-model inference
	// throughput stats (tokens/sec) advertised in /capabilities so the
	// router's scheduler can rank workers by speed. Values <= 0 fall back
	// to DefaultInferenceWindow (10 minutes).
	InferenceWindow time.Duration
	// AllowedModels is a comma-separated whitelist of models this worker is
	// WILLING to serve. When non-empty the router only schedules requests for
	// models in this list on this worker. Empty means "serve all".
	AllowedModels string
	// ExcludedModels is a comma-separated blacklist of models this worker is
	// NOT WILLING to serve. Applies only when AllowedModels is empty.
	ExcludedModels string
	// APIKey is the API key for authenticating inference requests (dev and prod mode).
	// When non-empty, /v1/chat/completions and /v1/completions require a valid
	// X-API-Key header. Mirrors the router's --api-key behavior (issue #81).
	APIKey string
	// RelayAPIKey is the key the worker presents to the router during relay
	// registration. It is distinct from APIKey: the router's --api-key gates
	// client inference requests, while the relay registration key gates which
	// workers the router accepts over the broker. In relay mode the worker is
	// on a remote host, so the router cannot verify it by peer IP — the key
	// IS the identity check. Empty means "present nothing" (the router only
	// rejects relay registrations when it has configured its own key).
	RelayAPIKey string
	// BackendTimeout is the HTTP client timeout for backend requests
	// (default 30s). Inference calls that exceed this duration are
	// aborted by the worker's HTTP client to the backend.
	BackendTimeout time.Duration
	// Threads is the llama-cpp thread count (default 4). Other backends
	// ignore it (issue #139).
	Threads int
}

// ReloadableConfig holds configuration values that can be re-read from
// environment variables on SIGHUP. Empty values mean "keep the current
// effective value".
type ReloadableConfig struct {
	RouterBase     string
	RelayURL       string
	RelayAPIKey    string
	APIKey         string
	AllowedModels  string
	ExcludedModels string
}

// newReloadableConfig builds the initial ReloadableConfig snapshot from a
// RunConfig. It is the single source for the three registration paths in
// RunWorker (relay, mTLS, HTTP), which previously each re-typed the same
// six-field literal.
func newReloadableConfig(cfg RunConfig) ReloadableConfig {
	return ReloadableConfig{
		RouterBase:     cfg.RouterBase,
		RelayURL:       cfg.RelayURL,
		RelayAPIKey:    cfg.RelayAPIKey,
		APIKey:         cfg.APIKey,
		AllowedModels:  cfg.AllowedModels,
		ExcludedModels: cfg.ExcludedModels,
	}
}

// Handle represents a running worker: HTTP server, registration loop (or
// mDNS announcer), and cancellation. Use Stop to shut it down.
type Handle struct {
	log         *slog.Logger
	cancel      context.CancelFunc
	announcer   discovery.Announcer
	done        chan struct{}
	regMu       sync.Mutex
	regCancel   context.CancelFunc
	regConfig   ReloadableConfig
	regInfoBase protocol.WorkerInfo
	regDevMode  bool
	regCaps     protocol.Capabilities
	srv         *Server
	// mTLS registration parameters, captured at startup and replayed by
	// ReloadConfig when the worker registered over mTLS. ReloadConfig must
	// re-launch RegisterLoopWithRefreshMTLS with these stored values because
	// launchRegistration only handles the HTTP/WS paths. SIGHUP cannot change
	// the certs (out of scope), so they are static.
	regUseMTLS    bool
	regProdRouter string
	regMTLSCert   string
	regMTLSKey    string
	regCertDir    string
	// rootCtx is the worker's main context (wctx). Registration loops
	// restarted by ReloadConfig derive from it so that Stop() (which cancels
	// wctx) still reaches them. Deriving from context.Background() instead
	// would orphan the post-reload loop: Stop() would log "shutting down"
	// but the new heartbeat goroutine would keep running.
	rootCtx context.Context
}

// Stop shuts the worker down: stops the announcer (if any), cancels the
// runtime context, and waits (bounded) for the HTTP server goroutine to exit.
func (h *Handle) Stop() {
	h.log.Info("shutting down")
	if h.announcer != nil {
		_ = h.announcer.Stop()
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.log.Warn("worker server did not exit in time; proceeding with shutdown")
	}
}

// launchRegistration decides which registration path to take based on the
// current regConfig and starts the registration loop. It returns a cancel
// function that stops the loop. Thread-safe: uses regMu to protect
// registration state.
//
// The caller must hold h.regMu before calling this function. On reload,
// the caller cancels the previous loop first, then calls this to start a
// new one with updated settings. In-flight inference requests are NOT
// disrupted because only the registration loop is cancelled.
//
// The new loop derives its context from rootCtx (the worker's main
// context), NOT from context.Background(): if it did, Stop() would cancel
// wctx but the post-reload heartbeat goroutine would keep running, orphaned.
func (h *Handle) launchRegistration(regConfig ReloadableConfig) context.CancelFunc {
	// Stop any currently running registration loop
	if h.regCancel != nil {
		h.regCancel()
		h.regCancel = nil
	}

	h.regConfig = regConfig

	// Select registration path based on config priority:
	// 1. Relay WebSocket (dev mode only, takes precedence)
	// 2. HTTP registration (dev mode or prod loopback fallback)
	// 3. mDNS discovery (default - no loop needed)
	if regConfig.RelayURL != "" {
		// WebSocket registration via relay (dev mode only).
		// Re-probe reachability so a reload to a dead broker degrades to
		// HTTP instead of spinning forever, matching startup behavior.
		if regConfig.RouterBase != "" && !relayReachable(h.rootCtx, regConfig.RelayURL) {
			h.log.Warn("relay unreachable after reload; falling back to HTTP registration",
				"relay", regConfig.RelayURL, "router", regConfig.RouterBase)
			return h.startHTTPRegistration(regConfig)
		}
		regCtx, regCancel := context.WithCancel(h.rootCtx)
		h.regCancel = regCancel
		go WSRegisterLoop(regCtx, WSConfig{RouterURL: regConfig.RelayURL}, func() protocol.WorkerInfo {
			return h.buildRegistrationInfo(regConfig)
		}, h.srv, h.log)
		return regCancel
	}
	if regConfig.RouterBase != "" {
		// HTTP registration
		return h.startHTTPRegistration(regConfig)
	}
	// mDNS discovery - no registration loop needed
	return func() {}
}

// startHTTPRegistration starts the HTTP registration loop for the given
// config. Extracted so the HTTP path is shared by launchRegistration and the
// relay→HTTP fallback instead of being duplicated inline.
func (h *Handle) startHTTPRegistration(regConfig ReloadableConfig) context.CancelFunc {
	regCtx, regCancel := context.WithCancel(h.rootCtx)
	h.regCancel = regCancel
	go RegisterLoopWithRefresh(regCtx, regConfig.RouterBase, func() protocol.WorkerInfo {
		return h.buildRegistrationInfo(regConfig)
	}, DefaultRegisterInterval, h.log)
	return regCancel
}

// launchMTLSRegistration (re)starts the prod-mode mTLS registration loop with
// the stored static cert parameters and the current ReloadableConfig. It is
// called by ReloadConfig for workers that registered over mTLS, because
// launchRegistration only handles the HTTP/WS paths. SIGHUP cannot change
// mTLS certs (out of scope), so the cert parameters are static and stored on
// the Handle at startup.
func (h *Handle) launchMTLSRegistration(regConfig ReloadableConfig) context.CancelFunc {
	if h.regCancel != nil {
		h.regCancel()
		h.regCancel = nil
	}
	h.regConfig = regConfig
	caCertPath := filepath.Join(h.regCertDir, "ca.crt")
	regCtx, regCancel := context.WithCancel(h.rootCtx)
	h.regCancel = regCancel
	go RegisterLoopWithRefreshMTLS(regCtx, h.regProdRouter, func() protocol.WorkerInfo {
		return h.buildRegistrationInfo(regConfig)
	}, DefaultRegisterInterval, h.regMTLSCert, h.regMTLSKey, caCertPath, h.log)
	return regCancel
}

// buildRegistrationInfo constructs a WorkerInfo for the current registration
// path. It reads fresh models from the server and applies model filters.
func (h *Handle) buildRegistrationInfo(regConfig ReloadableConfig) protocol.WorkerInfo {
	info := h.regInfoBase
	info.Capabilities = h.regCaps
	models := h.srv.GetModels()
	if h.regDevMode {
		for i := range models {
			if models[i].Name != "" {
				models[i].Loaded = true
			}
		}
	}
	info.Capabilities.Models = models
	h.srv.ApplyModelFilters(&info.Capabilities)
	info.APIKey = regConfig.RelayAPIKey
	// Relay path uses WebSocket transport; HTTP path uses default (empty/HTTP).
	if regConfig.RelayURL != "" {
		info.Transport = protocol.TransportWS
	}
	return info
}

// ReloadConfig re-reads configuration from environment variables and swaps
// the registration loop with the new values. Empty env var values preserve
// the current effective value. Thread-safe: can be called concurrently with
// in-flight requests. Only the registration heartbeat loop is restarted;
// in-flight inference requests are NOT disrupted.
//
// Lock ordering (to prevent deadlock): filtersMu → apiKeyMu → regMu.
// This function acquires them in that order: SetModelFilters (filtersMu),
// SetAPIKey (apiKeyMu), then regMu.Lock(). No other path reverses this order.
func (h *Handle) ReloadConfig() error {
	// Snapshot the current config under regMu so effectiveReloadConfig's
	// read of h.regConfig is race-free (it runs before the lock is taken).
	h.regMu.Lock()
	current := h.regConfig
	h.regMu.Unlock()

	// Re-read env vars, keeping current values if env var is empty
	newConfig := h.effectiveReloadConfig(current)

	// Validate the new config (mode is static, so use the stored DevMode)
	if err := ValidateRunConfig(RunConfig{
		DevMode:     h.regDevMode,
		RouterBase:  newConfig.RouterBase,
		RelayURL:    newConfig.RelayURL,
		RelayAPIKey: newConfig.RelayAPIKey,
		APIKey:      newConfig.APIKey,
	}); err != nil {
		h.log.Error("failed to validate reloaded config", "error", err)
		return err
	}

	// Update Server model filters and API key so new heartbeats carry them
	// and subsequent inference requests enforce the new key.
	if h.srv != nil {
		h.srv.SetModelFilters(splitModelList(newConfig.AllowedModels), splitModelList(newConfig.ExcludedModels))
		h.srv.SetAPIKey(newConfig.APIKey)
	}

	// Swap the registration loop: stop the old one, start a new one
	// with the updated settings. In-flight inference is untouched.
	h.regMu.Lock()
	defer h.regMu.Unlock()

	// mTLS workers re-launch RegisterLoopWithRefreshMTLS with the stored
	// static certs; launchRegistration only handles HTTP/WS paths.
	if h.regUseMTLS {
		h.launchMTLSRegistration(newConfig)
	} else {
		h.launchRegistration(newConfig)
	}

	h.log.Info("registration config reloaded via SIGHUP",
		"router", newConfig.RouterBase,
		"relay", newConfig.RelayURL,
		"api_key_set", newConfig.APIKey != "",
		"allowed_models", newConfig.AllowedModels,
		"excluded_models", newConfig.ExcludedModels)
	return nil
}

// effectiveReloadConfig reads environment variables and returns a
// ReloadableConfig, preserving current values when env vars are empty.
// The `current` argument is the caller's snapshot of h.regConfig (taken
// under regMu) so this function never reads h.regConfig directly — it is
// safe to call without holding the lock.
func (h *Handle) effectiveReloadConfig(current ReloadableConfig) ReloadableConfig {
	cfg := ReloadableConfig{}

	// Read env vars, preserving current values when empty
	if newVal := os.Getenv("INFERMESH_ROUTER_URL"); newVal != "" {
		cfg.RouterBase = newVal
	} else {
		cfg.RouterBase = current.RouterBase
	}
	if newVal := os.Getenv("INFERMESH_RELAY_URL"); newVal != "" {
		cfg.RelayURL = newVal
	} else {
		cfg.RelayURL = current.RelayURL
	}
	if newVal := os.Getenv("INFERMESH_RELAY_API_KEY"); newVal != "" {
		cfg.RelayAPIKey = newVal
	} else {
		cfg.RelayAPIKey = current.RelayAPIKey
	}
	if newVal := os.Getenv("INFERMESH_API_KEY"); newVal != "" {
		cfg.APIKey = newVal
	} else {
		cfg.APIKey = current.APIKey
	}
	if newVal := os.Getenv("INFERMESH_ALLOWED_MODELS"); newVal != "" {
		cfg.AllowedModels = newVal
	} else {
		cfg.AllowedModels = current.AllowedModels
	}
	if newVal := os.Getenv("INFERMESH_EXCLUDED_MODELS"); newVal != "" {
		cfg.ExcludedModels = newVal
	} else {
		cfg.ExcludedModels = current.ExcludedModels
	}

	return cfg
}

// RunWorker starts a worker (capability detection, backend, HTTP server, and
// registration) from cfg and returns a Handle used to shut it down. The
// passed ctx bounds the worker's lifetime; Stop cancels the derived context.
func RunWorker(ctx context.Context, cfg RunConfig) (*Handle, error) {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("role", "worker")

	wctx, cancel := context.WithCancel(ctx)
	h := &Handle{
		log:     log,
		cancel:  cancel,
		done:    make(chan struct{}),
		rootCtx: wctx,
	}
	fail := func(err error) (*Handle, error) {
		cancel()
		return nil, err
	}

	log.Info("worker configuration",
		"model_path", cfg.ModelPath,
		"backend", cfg.Backend,
	)

	// Configure discovery
	var dcfg discovery.Config
	if cfg.DevMode {
		dcfg = discovery.DevDefaults()
	} else {
		dcfg = discovery.Defaults()
	}
	// Ensure unique instance name for worker ID.
	// Use stable hostname-based ID so restarts re-register the same ID
	// and overwrite the old registry entry immediately (instead of leaving
	// a stale entry until TTL cleanup). Multiple workers on the same host
	// are differentiated by port.
	if dcfg.InstanceName == "" {
		hostname := Hostname()
		dcfg.InstanceName = fmt.Sprintf("worker-%s-%d", hostname, cfg.Port)
	}

	// Security configuration
	secCfg := security.Config{
		DevMode:            cfg.DevMode,
		MTLSCert:           cfg.MTLSCert,
		MTLSKey:            cfg.MTLSKey,
		CertDir:            cfg.CertDir,
		TrustedCNs:         cfg.TrustedCNs,
		EnableHealthChecks: cfg.EnableHealthChecks,
		APIKey:             cfg.APIKey,
	}

	// Configure capabilities
	capsCfg := capabilities.Defaults()

	// Pre-detect capabilities for initial announcement
	agg := capabilities.NewAggregator(capsCfg, log)
	caps, err := agg.Detect()
	if err != nil {
		log.Warn("initial capability detection failed, using fallback", "error", err)
	}

	// Start periodic capability refresh
	go agg.StartRefresh(wctx)

	// Create worker HTTP server
	addr := ":" + strconv.Itoa(cfg.Port)
	srv := NewServer(log, addr, secCfg)

	// Configure the inference-stats rolling window before Start (startup-only
	// setter; values <= 0 fall back to DefaultInferenceWindow).
	srv.SetInferenceWindow(cfg.InferenceWindow)

	// Set up backend adapter based on --backend flag
	backendImpl, err := NewBackendFromName(cfg.Backend, cfg.ModelPath, cfg.BackendURL, cfg.BackendTimeout, cfg.MaxTokens, cfg.Threads)
	if err != nil {
		log.Warn("failed to create backend", "backend", cfg.Backend, "error", err)
	} else {
		srv.SetBackend(backendImpl, "")
		log.Info("backend configured", "backend", cfg.Backend)

		// Try to dynamically discover and register models from backend
		if models, err := backendImpl.ListModels(); err == nil && len(models) > 0 {
			srv.SetModels(models)
			log.Info("dynamically discovered models from backend",
				"backend", cfg.Backend,
				"count", len(models))
		} else if err != nil {
			log.Warn("failed to auto-discover models from backend", "error", err)
			// Auto-discovery failed. For backends that auto-discover models from
			// the runtime (ollama, lmstudio, vllm), starting the worker without a
			// model-path is OK — the next capability refresh cycle will retry
			// discovery. If a model-path is provided, fall back to manual model
			// registration. Otherwise, start the worker with no models; they will
			// be discovered on the next refresh cycle.
			if cfg.ModelPath != "" {
				// Fall back to manual model registration if model path provided
				srv.SetModels([]protocol.ModelInfo{
					{
						Name:    filepath.Base(cfg.ModelPath),
						Backend: cfg.Backend,
						Loaded:  cfg.DevMode,
					},
				})
				log.Info("registered model from model-path",
					"model", filepath.Base(cfg.ModelPath))
			} else {
				// Auto-discovery failed but ModelPath is empty; start the worker
				// anyway. Models will be discovered on the next capability refresh
				// cycle (aggregator Detect() re-reads model config, or restart the
				// worker after starting the backend).
				log.Warn("worker started with no models — auto-discovery failed; " +
					"start the backend (LM Studio, Ollama, vLLM) and restart the " +
					"worker, or use --model-path to manually specify a model")
			}
		}

		// Start the health check loop
		go srv.StartHealthCheckLoop(wctx, effectiveHealthCheckInterval(cfg.HealthCheckInterval))

		// Periodically re-query the backend for its model catalogue. The
		// initial discovery above is one-shot: a backend (LM Studio, Ollama,
		// vLLM) that starts after the worker would otherwise stay invisible
		// until the worker is restarted. This goroutine closes that gap by
		// re-discovering on every tick and merging into srv.models.
		go refreshModels(wctx, srv, backendImpl, effectiveModelRefreshInterval(cfg.ModelRefreshInterval))
	}

	// Start worker server
	go func() {
		defer close(h.done)
		if err := srv.Start(wctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker server error", "error", err)
		}
	}()

	// Configure model opt-out filters from the comma-separated CLI values.
	allowed := splitModelList(cfg.AllowedModels)
	excluded := splitModelList(cfg.ExcludedModels)
	srv.SetModelFilters(allowed, excluded)

	// Build WorkerInfo for registration
	info := protocol.WorkerInfo{
		ID:           dcfg.InstanceName,
		Hostname:     Hostname(),
		IP:           LocalIP(),
		Port:         cfg.Port,
		Capabilities: caps,
		Status:       protocol.StatusAvailable,
		Version:      "v1",
	}
	// Carry the filters on the base announcement so the mDNS announcer path
	// (and any path that uses `info` directly) advertises them.
	info.Capabilities.AllowedModels = allowed
	info.Capabilities.ExcludedModels = excluded
	// Relay registrations are identity-checked by the router's API key (the
	// worker is on a remote host, so there is no peer IP to verify). Carry
	// the relay key on the WorkerInfo so the register payload carries it.
	info.APIKey = cfg.RelayAPIKey

	// Store the base info on the Handle so ReloadConfig can rebuild it.
	h.srv = srv
	h.regInfoBase = info
	h.regDevMode = cfg.DevMode
	h.regCaps = caps
	// mTLS registration parameters are static (SIGHUP cannot change certs,
	// out of scope for issue #188) but must be replayed by ReloadConfig,
	// which re-dispatches via launchMTLSRegistration for mTLS workers.
	h.regUseMTLS = cfg.ProdRouter != "" && !cfg.DevMode
	h.regProdRouter = cfg.ProdRouter
	h.regMTLSCert = cfg.MTLSCert
	h.regMTLSKey = cfg.MTLSKey
	h.regCertDir = cfg.CertDir
	h.rootCtx = wctx

	// Relay WebSocket registration (dev mode only) - takes precedence over RouterBase.
	//
	// When the relay is unreachable but a direct router URL is also configured
	// (the common Docker Compose case, where both INFERMESH_RELAY_URL and
	// INFERMESH_ROUTER_URL are set), the worker degrades gracefully to HTTP
	// registration instead of spinning forever on a dead broker. On a true
	// outbound-only host only the relay URL is set, so the check is skipped
	// and the worker stays relay-only.
	if cfg.RelayURL != "" {
		if !cfg.DevMode {
			log.Warn("--relay-url is only supported in dev mode; ignoring", "relay", cfg.RelayURL)
		} else if relayReachable(wctx, cfg.RelayURL) || cfg.RouterBase == "" {
			log.Info("registering with relay via websocket", "relay", cfg.RelayURL)
			info.Transport = protocol.TransportWS
			h.regInfoBase = info
			h.regConfig = newReloadableConfig(cfg)
			h.launchRegistration(h.regConfig)
			return h, nil
		} else {
			log.Warn("relay unreachable; falling back to HTTP registration", "relay", cfg.RelayURL, "router", cfg.RouterBase)
		}
	}

	// Prod-mode mTLS registration (bypasses mDNS). The router's /v1/dev/register
	// endpoint is wrapped by the mTLS middleware in prod mode, so the worker
	// must present its client cert over HTTPS. The router's prod loopback
	// check (server.go handleDevRegister) requires worker.IP == 127.0.0.1.
	if cfg.ProdRouter != "" && !cfg.DevMode {
		log.Info("prod-mode: registering with router via mTLS", "router", cfg.ProdRouter)
		prodInfo := info
		prodInfo.IP = "127.0.0.1" // satisfies router's prod loopback check
		h.regInfoBase = prodInfo
		h.regConfig = newReloadableConfig(cfg)
		// mTLS path uses RegisterLoopWithRefreshMTLS directly (launchRegistration
		// only handles HTTP/WS paths). ReloadConfig re-dispatches via
		// launchMTLSRegistration with the stored static certs.
		h.launchMTLSRegistration(h.regConfig)
		return h, nil
	}

	// HTTP registration (dev mode OR prod mode loopback fallback).
	// In prod mode loopback fallback, --router is used instead of --prod-router
	// when no mTLS secrets are provisioned. The router's prod loopback check
	// still requires worker.IP == 127.0.0.1.
	if cfg.RouterBase != "" {
		if cfg.DevMode {
			log.Info("dev-mode: registering with router via HTTP", "router", cfg.RouterBase)
		} else {
			log.Info("prod-mode loopback fallback: registering with router via HTTP", "router", cfg.RouterBase)
		}
		// Send fresh capabilities/models on every heartbeat: the startup
		// snapshot (info) predates dynamic backend discovery (SetModels),
		// so without refresh the router sees zero models and selection
		// fails with no_workers even though the worker logs success.
		regInfo := info
		if !cfg.DevMode {
			// Prod mode loopback fallback: router's handleDevRegister requires
			// worker.IP == "127.0.0.1" in prod mode.
			regInfo.IP = "127.0.0.1"
		}
		h.regInfoBase = regInfo
		h.regConfig = newReloadableConfig(cfg)
		h.launchRegistration(h.regConfig)
		return h, nil
	}

	// mDNS discovery path (original behavior)
	announcer, err := discovery.NewAnnouncer(discovery.BackendMDNS, dcfg, log)
	if err != nil {
		return fail(fmt.Errorf("failed to create announcer: %w", err))
	}
	h.announcer = announcer

	if err := announcer.Announce(wctx, info); err != nil {
		return fail(fmt.Errorf("failed to announce: %w", err))
	}
	if err := announcer.Start(wctx); err != nil {
		return fail(fmt.Errorf("failed to start announcer: %w", err))
	}

	return h, nil
}
