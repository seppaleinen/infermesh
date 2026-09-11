package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/security"
	"github.com/seppaleinen/infermesh/pkg/worker"
)

// RouterFlags holds the parsed flags for the router subcommand, including the
// worker passthrough flags used when --worker is set.
type RouterFlags struct {
	DevMode            bool
	ProdMode           bool
	MTLSCert           string
	MTLSKey            string
	CertDir            string
	APIKey             string
	Addr               string
	Worker             bool
	Port               int
	Backend            string
	ModelPath          string
	EnableHealthChecks bool
}

// parseRouterFlags parses args into RouterFlags. It uses ContinueOnError so
// the result is unit-testable; callers must handle the returned error.
func parseRouterFlags(args []string) (RouterFlags, error) {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	var f RouterFlags
	registerRouterFlags(fs, &f)
	return f, fs.Parse(args)
}

// registerRouterFlags registers the router subcommand flags on fs, binding
// values to f. Shared by parseRouterFlags and routerFlagUsage.
func registerRouterFlags(fs *flag.FlagSet, f *RouterFlags) {
	fs.BoolVar(&f.DevMode, "dev-mode", false, "run in dev mode (no auth, loopback only)")
	fs.BoolVar(&f.ProdMode, "prod-mode", false, "run in production mode (mTLS required)")
	fs.StringVar(&f.MTLSCert, "mtls-cert", "", "path to mTLS certificate file")
	fs.StringVar(&f.MTLSKey, "mtls-key", "", "path to mTLS key file")
	fs.StringVar(&f.CertDir, "cert-dir", "", "path to certificate directory (for CA)")
	fs.StringVar(&f.APIKey, "api-key", "", "API key for authentication (production mode)")
	fs.StringVar(&f.Addr, "addr", ":8080", "listen address (host:port) for the router HTTP server")
	fs.BoolVar(&f.Worker, "worker", false, "run an in-process worker registered against this router (dev mode only)")
	// Worker passthrough flags (combined mode).
	fs.IntVar(&f.Port, "port", 8081, "worker HTTP port (used with --worker)")
	fs.StringVar(&f.Backend, "backend", "llama-cpp", "worker backend adapter (llama-cpp, ollama, vllm, lmstudio)")
	fs.StringVar(&f.ModelPath, "model-path", "", "worker model file path (used with --worker)")
	fs.BoolVar(&f.EnableHealthChecks, "enable-health-checks", true, "enable periodic backend health checks (used with --worker)")
}

// listenPort extracts the numeric port from a listen address.
func listenPort(addr string) (int, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port in listen address %q", addr)
	}
	return p, nil
}

// waitForRouterReady polls the router's /v1/workers endpoint until it responds
// or the timeout elapses. It is a sanity check only: the register loop
// retries every 10s and self-heals even if this times out.
func waitForRouterReady(base string, timeout time.Duration) {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/v1/workers")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
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

	// Fail-fast validation for combined mode (exit 2 with a clear message).
	if f.Worker {
		routerPort, err := listenPort(f.Addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "infermesh: %v\n", err)
			return 2
		}
		if err := worker.ValidateCombinedFlags(f.ProdMode, f.Worker, routerPort, f.Port, f.Backend); err != nil {
			fmt.Fprintf(os.Stderr, "infermesh: %v\n", err)
			return 2
		}
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
		DevMode:  isDevMode,
		MTLSCert: f.MTLSCert,
		MTLSKey:  f.MTLSKey,
		CertDir:  f.CertDir,
		APIKey:   f.APIKey,
	}

	// Start router HTTP server
	srv := router.NewServer(reg, log, f.Addr, secCfg)
	go func() {
		if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("router server error", "error", err)
		}
	}()

	// Optional in-process worker (dev mode only; validated above).
	var wh *worker.Handle
	if f.Worker {
		base, err := worker.RouterBaseFromListenAddr(f.Addr)
		if err != nil {
			log.Error("failed to derive worker registration URL", "error", err)
			return 2
		}
		// Sanity check: wait for the router to be reachable before starting
		// the worker (the register loop self-heals with a 10s retry anyway).
		waitForRouterReady(base, 5*time.Second)

		wh, err = worker.RunWorker(ctx, worker.RunConfig{
			ModelPath:          f.ModelPath,
			Backend:            f.Backend,
			Port:               f.Port,
			DevMode:            isDevMode,
			RouterBase:         base,
			EnableHealthChecks: f.EnableHealthChecks,
		})
		if err != nil {
			log.Error("failed to start in-process worker", "error", err)
			return 1
		}
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	// Shutdown ordering: stop the worker first so its last heartbeat and
	// state reach the router before the listener closes; then cancel the
	// shared context (closes the router listener), then stop registry.
	log.Info("shutting down")
	if wh != nil {
		wh.Stop()
	}
	cancel()
	if listener != nil {
		_ = listener.Stop()
	}
	_ = reg.Stop()
	return 0
}
