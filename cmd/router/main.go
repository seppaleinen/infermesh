package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/seppaleinen/infermesh/pkg/discovery"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func main() {
	devMode := flag.Bool("dev-mode", false, "run in dev mode (no auth, loopback only)")
	prodMode := flag.Bool("prod-mode", false, "run in production mode (mTLS required)")
	mtlsCert := flag.String("mtls-cert", "", "path to mTLS certificate file")
	mtlsKey := flag.String("mtls-key", "", "path to mTLS key file")
	certDir := flag.String("cert-dir", "", "path to certificate directory (for CA)")
	apiKey := flag.String("api-key", "", "API key for authentication (production mode)")
	addr := flag.String("addr", ":8080", "address to bind router (host:port)")
	flag.Parse()

	// Determine dev mode: --prod-mode disables dev mode, --dev-mode enables it
	// If neither specified, defaults to dev mode (backward compatible)
	isDevMode := true
	if *prodMode {
		isDevMode = false
	} else if *devMode {
		isDevMode = true
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

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
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.Start(ctx); err != nil {
		log.Error("failed to start registry", "error", err)
		os.Exit(1)
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
			os.Exit(1)
		}
		if err := listener.Start(ctx); err != nil {
			log.Error("failed to start listener", "error", err)
			os.Exit(1)
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
		MTLSCert: *mtlsCert,
		MTLSKey:  *mtlsKey,
		CertDir:  *certDir,
		APIKey:   *apiKey,
	}

	// Start router HTTP server
	srv := router.NewServer(reg, log, *addr, secCfg)
	go func() {
		if err := srv.Start(ctx); err != nil {
			log.Error("router server error", "error", err)
		}
	}()
	log.Info("router listening", "addr", *addr, "ws_connect", fmt.Sprintf("ws://%s/v1/connect", *addr), "dev_mode", isDevMode)

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
}
