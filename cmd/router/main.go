package main

import (
	"context"
	"flag"
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

	// Create discovery listener
	listener, err := discovery.NewListener(discovery.BackendMDNS, cfg, log)
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
		MTLSCert:   *mtlsCert,
		MTLSKey:    *mtlsKey,
		CertDir:    *certDir,
	}

	// Start router HTTP server
	srv := router.NewServer(reg, log, ":8080", secCfg)
	go func() {
		if err := srv.Start(ctx); err != nil {
			log.Error("router server error", "error", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("shutting down")
	cancel()
	listener.Stop()
	reg.Stop()
}