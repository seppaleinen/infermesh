package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
	"github.com/seppaleinen/infermesh/pkg/worker"
)

func createBackend(backendName, modelPath string) (worker.Backend, error) {
	switch backendName {
	case "llama-cpp":
		return worker.NewLlamaCppBackend("http://localhost:8080", 2048, 4, 1), nil
	case "ollama":
		return worker.NewOllamaBackend("http://localhost:11434", modelPath), nil
	case "lmstudio":
		return worker.NewLMStudioBackend("http://127.0.0.1:1234", modelPath), nil
	case "vllm":
		return worker.NewVLLMBackend("localhost:8000", modelPath), nil
	case "custom":
		return worker.NewCustomBackend("http://localhost:8000", "", "custom"), nil
	default:
		return worker.NewLlamaCppBackend("http://localhost:8080", 2048, 4, 1), nil
	}
}

func main() {
	showCaps := flag.Bool("capabilities", false, "print worker capabilities and exit")
	devMode := flag.Bool("dev-mode", false, "run in dev mode (no auth, loopback only)")
	prodMode := flag.Bool("prod-mode", false, "run in production mode (mTLS required)")
	mtlsCert := flag.String("mtls-cert", "", "path to mTLS certificate file")
	mtlsKey := flag.String("mtls-key", "", "path to mTLS key file")
	port := flag.Int("port", 8081, "HTTP server port")
	certDir := flag.String("cert-dir", "", "path to certificate directory (for CA)")
	modelPath := flag.String("model-path", "", "path to model file (optional, auto-discovery used if not provided)")
	backend := flag.String("backend", "llama-cpp", "backend adapter (llama-cpp, ollama, vllm, lmstudio)")
	routerAddr := flag.String("router", "", "router base URL for dev-mode HTTP registration (e.g. http://127.0.0.1:8080); skips mDNS")
	enableHealthChecks := flag.Bool("enable-health-checks", true, "enable periodic backend health checks")
	flag.Parse()

	if *showCaps {
		os.Exit(capabilitiesCmd())
	}

	// Determine dev mode: --prod-mode disables dev mode, --dev-mode enables it
	// If neither specified, defaults to dev mode (backward compatible)
	isDevMode := true
	if *prodMode {
		isDevMode = false
	} else if *devMode {
		isDevMode = true
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	log.Info("worker configuration",
		"model_path", *modelPath,
		"backend", *backend,
	)

	// In production mode, a model path is required so the worker knows what it serves.
	if !isDevMode && *modelPath == "" {
		log.Error("--model-path is required in production mode")
		flag.Usage()
		os.Exit(1)
	}

	// Configure discovery
	var cfg discovery.Config
	if isDevMode {
		cfg = discovery.DevDefaults()
	} else {
		cfg = discovery.Defaults()
	}
	// Ensure unique instance name for worker ID
	if cfg.InstanceName == "" {
		rand.Seed(time.Now().UnixNano())
		instanceID := rand.Intn(10000)
		cfg.InstanceName = fmt.Sprintf("worker-%d", instanceID)
	}

	// Security configuration
	secCfg := security.Config{
		DevMode:            isDevMode,
		MTLSCert:           *mtlsCert,
		MTLSKey:            *mtlsKey,
		CertDir:            *certDir,
		EnableHealthChecks: *enableHealthChecks,
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agg.StartRefresh(ctx)

	// Create worker HTTP server
	addr := ":" + strconv.Itoa(*port)
	srv := worker.NewServer(log, addr, secCfg)

	// Set up backend adapter based on --backend flag
	backendImpl, err := createBackend(*backend, *modelPath)
	if err != nil {
		log.Warn("failed to create backend", "backend", *backend, "error", err)
	} else {
		srv.SetBackend(backendImpl, "")
		log.Info("backend configured", "backend", *backend)

		// Try to dynamically discover and register models from backend
		if models, err := backendImpl.ListModels(); err == nil && len(models) > 0 {
			srv.SetModels(models)
			log.Info("dynamically discovered models from backend",
				"backend", *backend,
				"count", len(models))
		} else if err != nil {
			log.Warn("failed to auto-discover models from backend, using model-path", "error", err)
			// Fall back to manual model registration if model path provided
			if *modelPath != "" {
				srv.SetModels([]protocol.ModelInfo{
					{
						Name:    filepath.Base(*modelPath),
						Backend: *backend,
						// In dev mode the backend is mocked (no real load lifecycle), so
						// treat the declared model as loaded to allow worker selection.
						Loaded: isDevMode,
					},
				})
				log.Info("registered model from model-path",
					"model", filepath.Base(*modelPath))
			}
		}

		// Start the health check loop (uses DefaultHealthCheckInterval)
		go srv.StartHealthCheckLoop(ctx, worker.DefaultHealthCheckInterval)
	}

	// Start worker server
	go func() {
		if err := srv.Start(ctx); err != nil {
			log.Error("worker server error", "error", err)
		}
	}()

	// Build WorkerInfo for registration
	info := protocol.WorkerInfo{
		ID:           cfg.InstanceName,
		Hostname:     getHostname(),
		IP:           getLocalIP(),
		Port:         *port,
		Capabilities: caps,
		Status:       protocol.StatusAvailable,
		Version:      "v1",
	}

	// Dev-mode HTTP registration (bypasses mDNS)
	if *routerAddr != "" {
		if isDevMode {
			// Override IP to loopback for dev HTTP registration
			info.IP = "127.0.0.1"
			log.Info("dev-mode: registering with router via HTTP", "router", *routerAddr)
			// Send fresh capabilities/models on every heartbeat: the startup
			// snapshot (info) predates dynamic backend discovery (SetModels),
			// so without refresh the router sees zero models and selection
			// fails with no_workers even though the worker logs success.
			baseInfo := info
			go worker.RegisterLoopWithRefresh(ctx, *routerAddr, func() protocol.WorkerInfo {
				current := baseInfo
				current.Capabilities = caps
				models := srv.GetModels()
				if isDevMode {
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
			}, worker.DefaultRegisterInterval, log)
			// Skip mDNS — wait for shutdown
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			<-sigCh
			log.Info("shutting down")
			cancel()
			return
		}
		// Prod mode with --router: warn and fall through to mDNS
		log.Warn("--router flag is ignored in production mode; using mDNS discovery", "router", *routerAddr)
	}

	// mDNS discovery path (original behavior)
	announcer, err := discovery.NewAnnouncer(discovery.BackendMDNS, cfg, log)
	if err != nil {
		log.Error("failed to create announcer", "error", err)
		os.Exit(1)
	}

	if err := announcer.Announce(ctx, info); err != nil {
		log.Error("failed to announce", "error", err)
		os.Exit(1)
	}
	if err := announcer.Start(ctx); err != nil {
		log.Error("failed to start announcer", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("shutting down")
	announcer.Stop()
	cancel()
}

func getLocalIP() string {
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

func getHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "infermesh-worker"
	}
	return name
}
