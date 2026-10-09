package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// RouterFlags holds the parsed flags for the router binary.
type RouterFlags struct {
	DevMode        bool
	ProdMode       bool
	MTLSCert       string
	MTLSKey        string
	CertDir        string
	APIKey         string
	TrustedCNs     string
	Addr           string
	RelayURL       string
	MaxInFlight    int
	MaxConnections int

	// Retry/failover settings for non-streaming requests.
	MaxAttempts int
	RetryBudget time.Duration

	// Weighted scorer settings. Zero values keep the built-in defaults (no-op).
	ScorerQuantMatch    float64
	ScorerVRAMFree      float64
	ScorerGPUUtil       float64
	ScorerQueueDepth    float64
	ScorerLatency       float64
	ScorerMaxQueueDepth int

	// Auto routing: model="auto" alias maps prompt complexity to tiered
	// model names. Empty values disable the alias (backward compatible).
	AutoTierSimple  string
	AutoTierMedium  string
	AutoTierComplex string

	// Auto routing classification thresholds (tokens estimated via the
	// ~4 chars/token heuristic). Zero values keep the documented defaults.
	AutoSimpleMaxTokens   int
	AutoMediumMaxTokens   int
	AutoReasoningKeywords string

	// Model aliases: comma-separated "alias=canonical" pairs. Empty
	// disables alias resolution (backward compatible). Canonical names are
	// kept verbatim, so a canonical may contain "=" — only the first "=" in
	// each pair is treated as the separator.
	ModelAlias string
}

// parseRouterFlags parses args into RouterFlags. It uses ContinueOnError so
// the result is unit-testable; callers must handle the returned error.
func parseRouterFlags(args []string) (RouterFlags, error) {
	fs := flag.NewFlagSet("infermesh-router", flag.ContinueOnError)
	var f RouterFlags
	registerRouterFlags(fs, &f)
	return f, fs.Parse(args)
}

// buildAutoRoutingConfig constructs an AutoRoutingConfig from the parsed
// flags. Returns nil when no tier model is configured, so the alias stays
// disabled by default (backward compatible).
func (f *RouterFlags) buildAutoRoutingConfig() *router.AutoRoutingConfig {
	if f.AutoTierSimple == "" && f.AutoTierMedium == "" && f.AutoTierComplex == "" {
		return nil
	}

	cfg := router.DefaultAutoRoutingConfig()
	cfg.Models = map[router.ComplexityTier]string{
		router.TierSimple:  f.AutoTierSimple,
		router.TierMedium:  f.AutoTierMedium,
		router.TierComplex: f.AutoTierComplex,
	}
	if f.AutoSimpleMaxTokens > 0 {
		cfg.SimpleMaxTokens = f.AutoSimpleMaxTokens
	}
	if f.AutoMediumMaxTokens > 0 {
		cfg.MediumMaxTokens = f.AutoMediumMaxTokens
	}
	if f.AutoReasoningKeywords != "" {
		for _, kw := range splitCSV(f.AutoReasoningKeywords) {
			if kw != "" {
				cfg.ReasoningKeywords = append(cfg.ReasoningKeywords, kw)
			}
		}
	}
	return &cfg
}

// buildModelAliasConfig constructs a ModelAliasConfig from the parsed
// --model-alias flag. Returns nil when no alias is configured, so alias
// resolution stays disabled by default (backward compatible).
func (f *RouterFlags) buildModelAliasConfig() *router.ModelAliasConfig {
	return router.ParseModelAliases(f.ModelAlias)
}

// splitCSV splits a comma-separated string into trimmed, non-empty tokens.
// Mirrors splitModelList in pkg/worker/run.go — keep in sync.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// registerRouterFlags registers the router flags on fs, binding values to f.
// Shared by parseRouterFlags and routerFlagUsage.
func registerRouterFlags(fs *flag.FlagSet, f *RouterFlags) {
	fs.BoolVar(&f.DevMode, "dev-mode", false, "run in dev mode (no auth, loopback only)")
	fs.BoolVar(&f.ProdMode, "prod-mode", false, "run in production mode (mTLS required)")
	fs.StringVar(&f.MTLSCert, "mtls-cert", "", "path to mTLS certificate file")
	fs.StringVar(&f.MTLSKey, "mtls-key", "", "path to mTLS key file")
	fs.StringVar(&f.CertDir, "cert-dir", "", "path to certificate directory (for CA)")
	fs.StringVar(&f.APIKey, "api-key", "", "API key for authenticating inference requests (dev and prod mode)")
	fs.StringVar(&f.TrustedCNs, "trusted-cn", "", "comma-separated list of trusted client certificate CNs for mTLS (empty = any)")
	fs.StringVar(&f.Addr, "addr", ":8080", "listen address (host:port) for the router HTTP server")
	fs.StringVar(&f.RelayURL, "relay-url", "", "relay URL for outbound-only WebSocket connectivity (dev mode only)")
	fs.IntVar(&f.MaxInFlight, "max-in-flight", 0, "max concurrent in-flight calls per worker (0 = default of 4); rejects excess with HTTP 429")
	fs.IntVar(&f.MaxConnections, "max-connections", 0, "max concurrent worker WebSocket connections (0 = default of 256); rejects excess with a protocol error frame")
	fs.IntVar(&f.MaxAttempts, "max-attempts", 3, "max dispatch attempts per non-streaming request (1 disables failover)")
	fs.DurationVar(&f.RetryBudget, "retry-budget", 60*time.Second, "global deadline across all failover attempts for a non-streaming request")
	fs.Float64Var(&f.ScorerQuantMatch, "scorer-quant-match", 0, "weight for quantization match in weighted scoring (0 = keep default of 0.40)")
	fs.Float64Var(&f.ScorerVRAMFree, "scorer-vram-free", 0, "weight for free VRAM ratio in weighted scoring (0 = keep default of 0.25)")
	fs.Float64Var(&f.ScorerGPUUtil, "scorer-gpu-util", 0, "weight for GPU utilization in weighted scoring (0 = keep default of 0.15)")
	fs.Float64Var(&f.ScorerQueueDepth, "scorer-queue-depth", 0, "weight for queue depth in weighted scoring (0 = keep default of 0.10)")
	fs.Float64Var(&f.ScorerLatency, "scorer-latency", 0, "weight for latency in weighted scoring (0 = keep default of 0.10)")
	fs.IntVar(&f.ScorerMaxQueueDepth, "scorer-max-queue-depth", 0, "queue depth treated as fully loaded when normalizing queue depth score (0 = keep default of 10)")

	// Auto routing: model="auto" alias. Empty tier models disable the alias.
	fs.StringVar(&f.AutoTierSimple, "auto-tier-simple", "", "model name to route simple prompts to when model=\"auto\" (empty = alias disabled)")
	fs.StringVar(&f.AutoTierMedium, "auto-tier-medium", "", "model name to route medium-complexity prompts to when model=\"auto\"")
	fs.StringVar(&f.AutoTierComplex, "auto-tier-complex", "", "model name to route complex prompts to when model=\"auto\"")
	fs.IntVar(&f.AutoSimpleMaxTokens, "auto-simple-max-tokens", 0, "estimated-token threshold below which prompts are classified simple (0 = default 200)")
	fs.IntVar(&f.AutoMediumMaxTokens, "auto-medium-max-tokens", 0, "estimated-token threshold below which prompts are classified medium (0 = default 600)")
	fs.StringVar(&f.AutoReasoningKeywords, "auto-reasoning-keywords", "", "comma-separated substrings whose presence in a prompt bumps it one complexity tier (reasoning signal)")

	// Model aliases: comma-separated "alias=canonical" pairs. An empty value
	// disables alias resolution (backward compatible). Use this to expose
	// short, stable names that map to the canonical model the pool actually
	// serves — e.g. "gpt-4o=qwen2.5-coder-7b-instruct-mlx@4bit".
	fs.StringVar(&f.ModelAlias, "model-alias", "", "comma-separated alias=canonical model pairs (empty = aliasing disabled)")
}

func main() {
	os.Exit(runRouter(os.Args[1:]))
}

// runRouter runs the router process and returns the process exit code.
func runRouter(args []string) int {
	f, err := parseRouterFlags(args)
	if err != nil {
		// The flag package already printed the error and usage to stderr.
		return 2
	}

	// Determine dev mode: --prod-mode disables dev mode, --dev-mode enables it
	// If neither specified, defaults to dev mode (backward compatible)
	isDevMode := true
	if f.ProdMode {
		isDevMode = false
	} else if f.DevMode {
		isDevMode = true
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("role", "router")

	// Configure discovery and registry
	var cfg discovery.Config
	if isDevMode {
		cfg = discovery.DevDefaults()
	} else {
		cfg = discovery.Defaults()
	}

	regCfg := registry.Defaults()

	// Create registry
	reg, err := registry.New(regCfg, log)
	if err != nil {
		log.Error("failed to create registry", "error", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.Start(ctx); err != nil {
		log.Error("failed to start registry", "error", err)
		return 1
	}

	// Create discovery listener (prod-mode only).
	// In dev-mode workers register via HTTP POST /v1/dev/register, so the
	// mDNS listener is skipped entirely. This avoids the hashicorp/mdns
	// udp6 [ff02::fb]:5353 bind spam on hosts without IPv6 multicast
	// (docker, CI, minimal VMs) where every 5s probe logs
	// "[ERR] mdns: Failed to bind to udp6 port" with no functional benefit.
	var listener discovery.Listener
	if !isDevMode {
		listener, err = discovery.NewListener(discovery.BackendMDNS, cfg, log)
		if err != nil {
			log.Error("failed to create listener", "error", err)
			return 1
		}
		if err := listener.Start(ctx); err != nil {
			log.Error("failed to start listener", "error", err)
			return 1
		}

		// Bridge: discovery events → registry
		go func() {
			for event := range listener.Events() {
				if err := reg.HandleEvent(event); err != nil {
					log.Warn("failed to handle discovery event", "error", err)
				}
			}
		}()
	} else {
		log.Info("dev-mode: mDNS discovery disabled, using HTTP registration (/v1/dev/register)")
	}

	// Subscribe to registry events for logging
	sub := reg.Subscribe()
	go func() {
		for event := range sub {
			log.Info("registry event", "type", event.Type, "worker", event.Worker.ID)
		}
	}()

	// Security configuration
	secCfg := security.Config{
		DevMode:    isDevMode,
		MTLSCert:   f.MTLSCert,
		MTLSKey:    f.MTLSKey,
		CertDir:    f.CertDir,
		APIKey:     f.APIKey,
		TrustedCNs: f.TrustedCNs,
	}

	// Validate TLS configuration in production mode before starting the server
	// so we fail fast with a clear error instead of crashing inside ListenAndServe.
	if !isDevMode {
		caCertPath := filepath.Join(f.CertDir, "ca.crt")
		if err := security.ValidateTLSConfig(f.MTLSCert, f.MTLSKey, caCertPath); err != nil {
			log.Error("invalid TLS configuration", "error", err)
			return 1
		}
	}

	// Start router HTTP server
	srv := router.NewServer(reg, log, f.Addr, secCfg)
	if f.MaxInFlight > 0 {
		srv.SetMaxInFlight(f.MaxInFlight)
	}
	if f.MaxConnections > 0 {
		srv.SetMaxConnections(f.MaxConnections)
	}
	// Zero values are no-ops that keep the built-in scorer defaults.
	srv.SetScorerWeights(f.ScorerQuantMatch, f.ScorerVRAMFree, f.ScorerGPUUtil, f.ScorerQueueDepth, f.ScorerLatency, f.ScorerMaxQueueDepth)

	// Configure retry/failover parameters.
	srv.SetMaxAttempts(f.MaxAttempts)
	srv.SetRetryBudget(f.RetryBudget)

	// Configure the model="auto" alias (disabled by default).
	if autoCfg := f.buildAutoRoutingConfig(); autoCfg != nil {
		srv.SetAutoRouting(autoCfg)
	}

	// Configure user-facing model aliases (disabled by default).
	if aliasCfg := f.buildModelAliasConfig(); aliasCfg != nil {
		srv.SetModelAliases(aliasCfg)
	}

	if f.RelayURL != "" {
		srv.SetRelayURL(f.RelayURL)
		// Establish the relay connection in the background with retry. The
		// HTTP server starts serving immediately; only the relay link
		// retries (exponential backoff) if the initial dial fails or the
		// connection later drops.
		go srv.RunRelay(ctx, f.RelayURL)
	}
	go func() {
		if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("router server error", "error", err)
		}
	}()

	// Log mode
	if f.RelayURL != "" {
		log.Info("router listening",
			"addr", f.Addr,
			"mode", "relay-only",
			"note", "/v1/connect not mounted (workers connect via relay)",
			"relay_url", f.RelayURL,
			"dev_mode", isDevMode)
	} else {
		log.Info("router listening", "addr", f.Addr, "dev_mode", isDevMode)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("shutting down")
	cancel()
	if listener != nil {
		_ = listener.Stop()
	}
	_ = reg.Stop()
	return 0
}
