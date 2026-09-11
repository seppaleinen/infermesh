package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
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

// ErrMissingModelPath is returned when a worker is configured without a model
// path in production mode.
var ErrMissingModelPath = errors.New("--model-path is required in production mode")

// RunConfig holds all configuration needed to run a worker.
type RunConfig struct {
	// ModelPath is the declared model file path (required in prod mode).
	ModelPath string
	// Backend selects the backend adapter (llama-cpp, ollama, vllm, lmstudio, custom).
	Backend string
	// Port is the HTTP server port.
	Port int
	// DevMode enables dev mode (no auth, loopback registration).
	DevMode bool
	// MTLSCert, MTLSKey, and CertDir are mTLS settings for prod mode.
	MTLSCert string
	MTLSKey  string
	CertDir  string
	// RouterBase is the router base URL for dev-mode HTTP registration
	// (e.g. http://127.0.0.1:8080). Empty means mDNS discovery.
	RouterBase string
	// EnableHealthChecks toggles periodic backend health checks.
	EnableHealthChecks bool
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

	// In production mode, a model path is required so the worker knows what it serves.
	if !cfg.DevMode && cfg.ModelPath == "" {
		return fail(ErrMissingModelPath)
	}

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
		EnableHealthChecks: cfg.EnableHealthChecks,
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

	// Set up backend adapter based on --backend flag
	backendImpl, err := NewBackendFromName(cfg.Backend, cfg.ModelPath)
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
			log.Warn("failed to auto-discover models from backend, using model-path", "error", err)
			// Fall back to manual model registration if model path provided
			if cfg.ModelPath != "" {
				srv.SetModels([]protocol.ModelInfo{
					{
						Name:    filepath.Base(cfg.ModelPath),
						Backend: cfg.Backend,
						// In dev mode the backend is mocked (no real load lifecycle), so
						// treat the declared model as loaded to allow worker selection.
						Loaded: cfg.DevMode,
					},
				})
				log.Info("registered model from model-path",
					"model", filepath.Base(cfg.ModelPath))
			}
		}

		// Start the health check loop (uses DefaultHealthCheckInterval)
		go srv.StartHealthCheckLoop(wctx, DefaultHealthCheckInterval)
	}

	// Start worker server
	go func() {
		defer close(h.done)
		if err := srv.Start(wctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker server error", "error", err)
		}
	}()

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

	// Dev-mode HTTP registration (bypasses mDNS)
	if cfg.RouterBase != "" {
		if cfg.DevMode {
			log.Info("dev-mode: registering with router via HTTP", "router", cfg.RouterBase)
			// Send fresh capabilities/models on every heartbeat: the startup
			// snapshot (info) predates dynamic backend discovery (SetModels),
			// so without refresh the router sees zero models and selection
			// fails with no_workers even though the worker logs success.
			baseInfo := info
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
				return current
			}, DefaultRegisterInterval, log)
			return h, nil
		}
		// Prod mode with --router: warn and fall through to mDNS
		log.Warn("--router flag is ignored in production mode; using mDNS discovery", "router", cfg.RouterBase)
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

// NewBackendFromName creates the backend adapter for the given backend name.
// Unknown names fall back to the llama-cpp backend (preserving the historical
// CLI behavior of the old createBackend helper).
func NewBackendFromName(backendName, modelPath string) (Backend, error) {
	switch backendName {
	case "llama-cpp":
		return NewLlamaCppBackend("http://localhost:8080", 2048, 4, 1), nil
	case "ollama":
		return NewOllamaBackend("http://localhost:11434", modelPath), nil
	case "lmstudio":
		return NewLMStudioBackend("http://127.0.0.1:1234", modelPath), nil
	case "vllm":
		return NewVLLMBackend("localhost:8000", modelPath), nil
	case "custom":
		return NewCustomBackend("http://localhost:8000", "", "custom"), nil
	default:
		return NewLlamaCppBackend("http://localhost:8080", 2048, 4, 1), nil
	}
}

// RouterBaseFromListenAddr derives the dev-mode HTTP registration base URL
// from a router listen address:
//
//	":8080", "0.0.0.0:8080", "localhost:8080" -> "http://127.0.0.1:8080"
//	"127.0.0.1:9000" -> "http://127.0.0.1:9000"
//	"myhost:8080" -> "http://myhost:8080"
//
// The scheme is always http (combined mode is dev-only).
func RouterBaseFromListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid port in listen address %q", addr)
	}
	if isLoopbackHost(host) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// isLoopbackHost reports whether a listen host is empty (any), the wildcard
// "0.0.0.0", or a loopback address.
func isLoopbackHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// LocalIP returns the first non-loopback IPv4 address of the host, or
// 127.0.0.1 when none can be determined.
func LocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

// Hostname returns the local hostname, falling back to "infermesh-worker"
// when it cannot be determined.
func Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "infermesh-worker"
	}
	return name
}

// PrintCapabilities prints the detected capabilities in a human-readable
// format to w. It returns 0 on success; on error the message is written to
// os.Stderr and 1 is returned.
func PrintCapabilities(w io.Writer) int {
	cfg := capabilities.Defaults()
	agg := capabilities.NewAggregator(cfg, nil)

	caps, err := agg.Detect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error detecting capabilities: %v\n", err)
		return 1
	}

	if err := writeCapabilities(w, caps); err != nil {
		return 1
	}
	return 0
}

// writeCapabilities renders the capability report to w, returning the first
// write error encountered.
func writeCapabilities(w io.Writer, caps protocol.Capabilities) error {
	write := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}

	if err := write("=== InferMesh Worker Capabilities ===\n"); err != nil {
		return err
	}
	if err := write("\n"); err != nil {
		return err
	}

	// GPU
	if err := write("GPU:\n"); err != nil {
		return err
	}
	if caps.GPU.Vendor != "" && caps.GPU.Model != "no GPU detected" {
		if err := write("  Vendor:     %s\n", caps.GPU.Vendor); err != nil {
			return err
		}
		if err := write("  Model:      %s\n", caps.GPU.Model); err != nil {
			return err
		}
		if err := write("  Compute Cores: %d\n", caps.GPU.ComputeCore); err != nil {
			return err
		}
		if err := write("  Total VRAM: %d MB\n", caps.GPU.TotalVRAM); err != nil {
			return err
		}
		if err := write("  Free VRAM:  %d MB\n", caps.GPU.FreeVRAM); err != nil {
			return err
		}
	} else {
		if err := write("  No GPU detected\n"); err != nil {
			return err
		}
	}
	if err := write("\n"); err != nil {
		return err
	}

	// VRAM
	if err := write("VRAM:\n"); err != nil {
		return err
	}
	if err := write("  Total: %d MB\n", caps.VRAM.TotalMB); err != nil {
		return err
	}
	if err := write("  Free: %d MB\n", caps.VRAM.FreeMB); err != nil {
		return err
	}
	if err := write("\n"); err != nil {
		return err
	}

	// System
	if err := write("System:\n"); err != nil {
		return err
	}
	if err := write("  Total Memory: %d MB\n", caps.System.TotalMB); err != nil {
		return err
	}
	if err := write("  Free Memory: %d MB\n", caps.System.FreeMB); err != nil {
		return err
	}
	if err := write("\n"); err != nil {
		return err
	}

	// Models
	if err := write("Models:\n"); err != nil {
		return err
	}
	if len(caps.Models) > 0 {
		for _, m := range caps.Models {
			if err := write("  %s (%d bytes, %s, backend: %s, loaded: %t)\n",
				m.Name, m.Size, m.Quantization, m.Backend, m.Loaded); err != nil {
				return err
			}
		}
	} else {
		if err := write("  No models declared\n"); err != nil {
			return err
		}
	}
	if err := write("\n"); err != nil {
		return err
	}

	// Engines
	if err := write("Engines:\n"); err != nil {
		return err
	}
	if len(caps.Engines) > 0 {
		for _, e := range caps.Engines {
			if err := write("  %s\n", e); err != nil {
				return err
			}
		}
	} else {
		if err := write("  No engines detected\n"); err != nil {
			return err
		}
	}
	if err := write("\n"); err != nil {
		return err
	}
	return nil
}

// ValidateCombinedFlags performs fail-fast validation for the combined
// router+worker mode, returning an error (for exit 2 with a clear message)
// when the flags cannot run in one process.
func ValidateCombinedFlags(prodMode, workerMode bool, routerPort, workerPort int, backend string) error {
	if prodMode && workerMode {
		return errors.New("--worker is only supported in dev mode; use 'router --dev-mode --worker'")
	}
	if !workerMode {
		return nil
	}
	if routerPort == workerPort {
		return fmt.Errorf("worker port %d collides with router port %d; choose a different --port", workerPort, routerPort)
	}
	if bp := backendPort(backend); bp == workerPort {
		return fmt.Errorf("worker port %d collides with the %s backend's hardcoded port %d; choose a different --port", workerPort, backend, bp)
	}
	return nil
}

// backendPort returns the hardcoded port of the named backend's HTTP server.
// Unknown backend names fall back to llama-cpp's port (matching
// NewBackendFromName).
func backendPort(backend string) int {
	switch backend {
	case "ollama":
		return 11434
	case "lmstudio":
		return 1234
	case "vllm", "custom":
		return 8000
	default: // llama-cpp and unknown names (llama-cpp fallback)
		return 8080
	}
}
