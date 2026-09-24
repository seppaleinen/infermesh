package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	Addr           string
	RelayURL       string
	MaxInFlight    int
	MaxConnections int
}

// parseRouterFlags parses args into RouterFlags. It uses ContinueOnError so
// the result is unit-testable; callers must handle the returned error.
func parseRouterFlags(args []string) (RouterFlags, error) {
	fs := flag.NewFlagSet("infermesh-router", flag.ContinueOnError)
	var f RouterFlags
	registerRouterFlags(fs, &f)
	return f, fs.Parse(args)
}

// registerRouterFlags registers the router flags on fs, binding values to f.
// Shared by parseRouterFlags and routerFlagUsage.
func registerRouterFlags(fs *flag.FlagSet, f *RouterFlags) {
	fs.BoolVar(&f.DevMode, "dev-mode", false, "run in dev mode (no auth, loopback only)")
	fs.BoolVar(&f.ProdMode, "prod-mode", false, "run in production mode (mTLS required)")
	fs.StringVar(&f.MTLSCert, "mtls-cert", "", "path to mTLS certificate file")
	fs.StringVar(&f.MTLSKey, "mtls-key", "", "path to mTLS key file")
	fs.StringVar(&f.CertDir, "cert-dir", "", "path to certificate directory (for CA)")
	fs.StringVar(&f.APIKey, "api-key", "", "API key for authentication (production mode)")
	fs.StringVar(&f.Addr, "addr", ":8080", "listen address (host:port) for the router HTTP server")
	fs.StringVar(&f.RelayURL, "relay-url", "", "relay URL for outbound-only WebSocket connectivity (dev mode only)")
	fs.IntVar(&f.MaxInFlight, "max-in-flight", 0, "max concurrent in-flight calls per worker (0 = default of 4); rejects excess with HTTP 429")
	fs.IntVar(&f.MaxConnections, "max-connections", 0, "max concurrent worker WebSocket connections (0 = default of 256); rejects excess with a protocol error frame")
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
		DevMode:  isDevMode,
		MTLSCert: f.MTLSCert,
		MTLSKey:  f.MTLSKey,
		CertDir:  f.CertDir,
		APIKey:   f.APIKey,
	}

	// Start router HTTP server
	srv := router.NewServer(reg, log, f.Addr, secCfg)
	if f.MaxInFlight > 0 {
		srv.SetMaxInFlight(f.MaxInFlight)
	}
	if f.MaxConnections > 0 {
		srv.SetMaxConnections(f.MaxConnections)
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
