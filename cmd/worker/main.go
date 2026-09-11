package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/seppaleinen/infermesh/pkg/worker"
)

// main is a thin wrapper over pkg/worker: it parses flags and delegates all
// runtime behavior to worker.RunWorker.
func main() {
	fs := flag.NewFlagSet("infermesh-worker", flag.ExitOnError)
	showCaps := fs.Bool("capabilities", false, "print worker capabilities and exit")
	devMode := fs.Bool("dev-mode", false, "run in dev mode (no auth, loopback only)")
	prodMode := fs.Bool("prod-mode", false, "run in production mode (mTLS required)")
	mtlsCert := fs.String("mtls-cert", "", "path to mTLS certificate file")
	mtlsKey := fs.String("mtls-key", "", "path to mTLS key file")
	port := fs.Int("port", 8081, "HTTP server port")
	certDir := fs.String("cert-dir", "", "path to certificate directory (for CA)")
	modelPath := fs.String("model-path", "", "path to model file (optional, auto-discovery used if not provided)")
	backend := fs.String("backend", "llama-cpp", "backend adapter (llama-cpp, ollama, vllm, lmstudio)")
	routerAddr := fs.String("router", "", "router base URL for dev-mode HTTP registration (e.g. http://127.0.0.1:8080); skips mDNS")
	enableHealthChecks := fs.Bool("enable-health-checks", true, "enable periodic backend health checks")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// Unreachable with flag.ExitOnError, which exits on parse errors.
		os.Exit(2)
	}

	if *showCaps {
		os.Exit(worker.PrintCapabilities(os.Stdout))
	}

	// Determine dev mode: --prod-mode disables dev mode, --dev-mode enables it
	// If neither specified, defaults to dev mode (backward compatible)
	isDevMode := true
	if *prodMode {
		isDevMode = false
	} else if *devMode {
		isDevMode = true
	}

	cfg := worker.RunConfig{
		ModelPath:          *modelPath,
		Backend:            *backend,
		Port:               *port,
		DevMode:            isDevMode,
		MTLSCert:           *mtlsCert,
		MTLSKey:            *mtlsKey,
		CertDir:            *certDir,
		RouterBase:         *routerAddr,
		EnableHealthChecks: *enableHealthChecks,
	}

	h, err := worker.RunWorker(context.Background(), cfg)
	if err != nil {
		if errors.Is(err, worker.ErrMissingModelPath) {
			// Match the original CLI output: error line on stdout, then usage.
			log := slog.New(slog.NewTextHandler(os.Stdout, nil))
			log.Error(err.Error())
			fs.Usage()
		} else {
			fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		}
		os.Exit(1)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	h.Stop()
}
