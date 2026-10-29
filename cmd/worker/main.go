package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/internal/routeraddr"
	"github.com/seppaleinen/infermesh/pkg/worker"
)

// WorkerFlags holds the parsed flags for the worker binary.
type WorkerFlags struct {
	Capabilities       bool
	DevMode            bool
	ProdMode           bool
	MTLSCert           string
	MTLSKey            string
	Port               int
	CertDir            string
	ModelPath          string
	Backend            string
	BackendURL         string
	Router             string
	ProdRouter         string
	RelayURL           string
	TrustedCNs         string
	EnableHealthChecks bool
	AllowedModels      string
	ExcludedModels     string
	// ModelRefreshInterval is the cadence for re-querying the backend's
	// model catalogue (0 uses the default 10s).
	ModelRefreshInterval time.Duration
	// HealthCheckInterval is the cadence for backend health checks (0 uses the default 30s).
	HealthCheckInterval time.Duration
	// InferenceWindow is the rolling window for per-model inference
	// throughput stats (tokens/sec) reported in /capabilities. Default 6h;
	// values <= 0 fall back to the built-in 10-minute window.
	InferenceWindow time.Duration
	APIKey          string
	// BackendTimeout is the HTTP client timeout for backend requests
	// (default 30s). Inference calls that exceed this duration are
	// aborted by the worker's HTTP client to the backend.
	BackendTimeout time.Duration
	// MaxTokens is the default max_tokens to send to the backend when the
	// incoming request omits it. Default 4096.
	MaxTokens int
	// Threads is the llama-cpp thread count (default 4). Other backends
	// ignore it (issue #139).
	Threads int
	// RelayAPIKey is the key presented to the router during relay registration.
	// Distinct from APIKey: the router's --api-key gates client inference
	// requests, while the relay registration key gates which workers the router
	// accepts over the broker. In relay mode the worker is on a remote host,
	// so the router cannot verify it by peer IP — the key IS the identity
	// check. Empty means "present nothing" (the router only rejects relay
	// registrations when it has configured its own key).
	RelayAPIKey string
}

// parseWorkerFlags parses args into WorkerFlags. It uses ContinueOnError so
// the result is unit-testable; callers must handle the returned error.
func parseWorkerFlags(args []string) (WorkerFlags, error) {
	fs := flag.NewFlagSet("infermesh-worker", flag.ContinueOnError)
	var f WorkerFlags
	registerWorkerFlags(fs, &f)
	return f, fs.Parse(args)
}

// registerWorkerFlags registers the worker flags on fs, binding values to f.
func registerWorkerFlags(fs *flag.FlagSet, f *WorkerFlags) {
	fs.BoolVar(&f.Capabilities, "capabilities", false, "print detected capabilities and exit")
	fs.BoolVar(&f.DevMode, "dev-mode", false, "run in dev mode (no auth, loopback only)")
	fs.BoolVar(&f.ProdMode, "prod-mode", false, "run in production mode (mTLS required)")
	fs.StringVar(&f.MTLSCert, "mtls-cert", "", "path to mTLS certificate file")
	fs.StringVar(&f.MTLSKey, "mtls-key", "", "path to mTLS key file")
	fs.IntVar(&f.Port, "port", 8081, "listen port for the worker HTTP server")
	fs.StringVar(&f.CertDir, "cert-dir", "", "path to certificate directory")
	fs.StringVar(&f.ModelPath, "model-path", "", "path to the model file (optional; models can be discovered dynamically)")
	fs.StringVar(&f.Backend, "backend", "llama-cpp", "backend adapter (llama-cpp, ollama, lmstudio, vllm, custom)")
	fs.StringVar(&f.BackendURL, "backend-url", "", "backend adapter base URL (overrides the adapter's default endpoint)")
	fs.StringVar(&f.Router, "router", "", "router base URL for HTTP registration (e.g. http://127.0.0.1:8080, dev-mode only)")
	fs.StringVar(&f.ProdRouter, "prod-router", "", "router base URL for mTLS registration in production mode (e.g. https://127.0.0.1:8080); bypasses mDNS")
	fs.StringVar(&f.RelayURL, "relay-url", "", "relay URL for outbound-only WebSocket connectivity (dev mode only)")
	fs.StringVar(&f.TrustedCNs, "trusted-cn", "", "comma-separated list of trusted client certificate CNs for mTLS (empty = any)")
	fs.BoolVar(&f.EnableHealthChecks, "enable-health-checks", true, "enable periodic backend health checks")
	fs.StringVar(&f.AllowedModels, "allowed-models", "", "comma-separated list of models this worker is WILLING to serve (whitelist); empty = all")
	fs.StringVar(&f.ExcludedModels, "excluded-models", "", "comma-separated list of models this worker is NOT WILLING to serve (blacklist); applies only when --allowed-models is empty")
	fs.DurationVar(&f.ModelRefreshInterval, "model-refresh-interval", 0, "cadence for re-querying the backend's model catalogue; 0 uses the default 10s")
	fs.DurationVar(&f.HealthCheckInterval, "health-check-interval", 0, "cadence for backend health checks; 0 uses the default 30s")
	// InferenceWindow: tokens/sec is a slow-moving machine characteristic —
	// a long window smooths bursty usage while still adapting within a work
	// session. Values <= 0 fall back to the built-in 10-minute window.
	fs.DurationVar(&f.InferenceWindow, "inference-window", 6*time.Hour, "rolling window for per-model inference throughput stats (tokens/sec) reported in /capabilities; default 6h. tokens/sec is a slow-moving machine characteristic — 6h smooths bursty usage while adapting within a work session. Values <= 0 fall back to the built-in 10-minute window.")
	fs.StringVar(&f.APIKey, "api-key", "", "API key for authenticating inference requests (dev and prod mode)")
	// Idle timeout for streaming backends: abort if no SSE line arrives for this
	// long. The timer resets on every line received, so active streaming is
	// not bounded by this value. First-byte wait is also bounded by it.
	// Non-streaming calls still use this as an absolute deadline.
	fs.DurationVar(&f.BackendTimeout, "backend-timeout", 120*time.Second, "idle timeout for backend streaming: abort if no SSE line arrives for this long (does not count against active streaming; first-byte wait also bounded by this value)")
	// Default max_tokens to send to the backend when the incoming request
	// omits it. 4096 is a sensible default for most models.
	fs.IntVar(&f.MaxTokens, "max-tokens", 4096, "default max_tokens to send to the backend when the incoming request omits it")
	fs.IntVar(&f.Threads, "threads", 4, "number of threads for the llama-cpp backend (default 4); ignored by other backends")
	fs.StringVar(&f.RelayAPIKey, "relay-api-key", "", "key presented to the router during relay registration; distinct from --api-key, which gates client inference requests")
}

func main() {
	os.Exit(runWorker(os.Args[1:]))
}

// runWorker runs the worker process and returns the process exit code.
// It is the thin CLI wrapper around pkg/worker.RunWorker; all runtime logic
// lives in pkg/worker.
func runWorker(args []string) int {
	f, err := parseWorkerFlags(args)
	if err != nil {
		// The flag package already printed the error and usage to stderr.
		return 2
	}

	// --capabilities: print detected capabilities and exit (before any
	// model/backend validation, matching the old cmd/worker behavior).
	if f.Capabilities {
		return worker.PrintCapabilities(os.Stdout)
	}

	// Determine dev mode: --prod-mode disables dev mode, --dev-mode enables it
	// If neither specified, defaults to dev mode (backward compatible).
	// Resolved once per process via the shared routeraddr.Mode (issue #103 R2).
	mode := routeraddr.ResolveMode(f.DevMode, f.ProdMode)

	// Dev-mode default: when --router is not provided, use the router's
	// default address (http://127.0.0.1:8080) so the worker registers via
	// HTTP instead of silently falling back to mDNS. This matches the
	// router's default --addr :8080 and avoids the silent registration
	// failure when the router runs on a non-default port.
	//
	// INFERMESH_ROUTER_URL overrides the default without a flag, so container
	// orchestrators (Docker Compose, Kubernetes) can point the worker at the
	// router's service name + port while keeping the loopback default for
	// bare-metal dev. The flag still wins when set.
	if mode.IsDev() && f.Router == "" {
		if env := os.Getenv("INFERMESH_ROUTER_URL"); env != "" {
			f.Router = env
		} else {
			f.Router = "http://127.0.0.1:8080"
		}
	}

	// --relay-url wins; INFERMESH_RELAY_URL is the env-var fallback so the
	// relay profile can inject the broker URL without editing WORKER_ARGS.
	// When both the relay and a direct router URL are set (the Compose relay
	// profile), the worker probes the relay first and falls back to HTTP
	// registration if the broker is unreachable — see relayReachable in
	// pkg/worker/run.go.
	if f.RelayURL == "" {
		if env := os.Getenv("INFERMESH_RELAY_URL"); env != "" {
			f.RelayURL = env
		}
	}
	// --relay-api-key wins; INFERMESH_RELAY_API_KEY is the env-var fallback so
	// the relay profile can inject the registration key without editing
	// WORKER_ARGS.
	if f.RelayAPIKey == "" {
		if env := os.Getenv("INFERMESH_RELAY_API_KEY"); env != "" {
			f.RelayAPIKey = env
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start worker (RunWorker selects the backend adapter internally).
	h, err := worker.RunWorker(ctx, worker.RunConfig{
		ModelPath:            f.ModelPath,
		Backend:              f.Backend,
		BackendURL:           f.BackendURL,
		Port:                 f.Port,
		DevMode:              mode.IsDev(),
		MTLSCert:             f.MTLSCert,
		MTLSKey:              f.MTLSKey,
		CertDir:              f.CertDir,
		TrustedCNs:           f.TrustedCNs,
		RouterBase:           f.Router,
		ProdRouter:           f.ProdRouter,
		RelayURL:             f.RelayURL,
		EnableHealthChecks:   f.EnableHealthChecks,
		MaxTokens:            f.MaxTokens,
		AllowedModels:        f.AllowedModels,
		ExcludedModels:       f.ExcludedModels,
		ModelRefreshInterval: f.ModelRefreshInterval,
		HealthCheckInterval:  f.HealthCheckInterval,
		InferenceWindow:      f.InferenceWindow,
		APIKey:               f.APIKey,
		BackendTimeout:       f.BackendTimeout,
		Threads:              f.Threads,
		RelayAPIKey:          f.RelayAPIKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		return 1
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	// h.Stop() logs "shutting down" via its own (role=worker) logger.
	h.Stop()
	return 0
}
