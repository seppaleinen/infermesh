package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

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

// Handle represents a running worker: HTTP server, registration loop (or
// mDNS announcer), and cancellation. Use Stop to shut it down.
type Handle struct {
	log       *slog.Logger
	cancel    context.CancelFunc
	announcer discovery.Announcer
	done      chan struct{}
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

// RunWorker starts a worker (capability detection, backend, HTTP server, and
// registration) from cfg and returns a Handle used to shut it down. The
// passed ctx bounds the worker's lifetime; Stop cancels the derived context.
func RunWorker(ctx context.Context, cfg RunConfig) (*Handle, error) {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("role", "worker")

	wctx, cancel := context.WithCancel(ctx)
	h := &Handle{
		log:    log,
		cancel: cancel,
		done:   make(chan struct{}),
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
	// Ensure unique instance name for worker ID
	// (math/rand is auto-seeded since Go 1.20, no explicit Seed needed)
	if dcfg.InstanceName == "" {
		instanceID := rand.Intn(10000)
		dcfg.InstanceName = fmt.Sprintf("worker-%d", instanceID)
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
			baseInfo := info
			go WSRegisterLoop(wctx, WSConfig{RouterURL: cfg.RelayURL}, func() protocol.WorkerInfo {
				current := baseInfo
				current.Capabilities = caps
				models := srv.GetModels()
				if cfg.DevMode {
					for i := range models {
						if models[i].Name != "" {
							models[i].Loaded = true
						}
					}
				}
				current.Capabilities.Models = models
				srv.ApplyModelFilters(&current.Capabilities)
				return current
			}, srv, log)
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
		baseInfo := prodInfo
		caCertPath := filepath.Join(cfg.CertDir, "ca.crt")
		go RegisterLoopWithRefreshMTLS(wctx, cfg.ProdRouter, func() protocol.WorkerInfo {
			current := baseInfo
			current.Capabilities = caps
			current.Capabilities.Models = srv.GetModels()
			srv.ApplyModelFilters(&current.Capabilities)
			return current
		}, DefaultRegisterInterval, cfg.MTLSCert, cfg.MTLSKey, caCertPath, log)
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
		baseInfo := regInfo
		go RegisterLoopWithRefresh(wctx, cfg.RouterBase, func() protocol.WorkerInfo {
			current := baseInfo
			current.Capabilities = caps
			models := srv.GetModels()
			if cfg.DevMode {
				for i := range models {
					// Dev backends (LM Studio etc.) serve whatever they list;
					// treat catalogue entries as routable.
					if models[i].Name != "" {
						models[i].Loaded = true
					}
				}
			}
			current.Capabilities.Models = models
			srv.ApplyModelFilters(&current.Capabilities)
			return current
		}, DefaultRegisterInterval, log)
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
