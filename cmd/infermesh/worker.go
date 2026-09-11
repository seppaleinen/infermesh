package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/seppaleinen/infermesh/pkg/worker"
)

// WorkerFlags holds the parsed flags for the worker subcommand.
type WorkerFlags struct {
	Capabilities     bool
	DevMode          bool
	ProdMode         bool
	MTLSCert         string
	MTLSKey          string
	Port             int
	CertDir          string
	ModelPath        string
	Backend          string
	Router           string
	EnableHealthChecks bool
}

// parseWorkerFlags parses args into WorkerFlags. It uses ContinueOnError so
// the result is unit-testable; callers must handle the returned error.
func parseWorkerFlags(args []string) (WorkerFlags, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	var f WorkerFlags
	registerWorkerFlags(fs, &f)
	return f, fs.Parse(args)
}

// registerWorkerFlags registers the worker subcommand flags on fs, binding
// values to f. Shared by parseWorkerFlags and workerFlagUsage.
func registerWorkerFlags(fs *flag.FlagSet, f *WorkerFlags) {
	fs.BoolVar(&f.Capabilities, "capabilities", false, "print detected capabilities and exit")
	fs.BoolVar(&f.DevMode, "dev-mode", false, "run in dev mode (no auth, loopback only)")
	fs.BoolVar(&f.ProdMode, "prod-mode", false, "run in production mode (mTLS required)")
	fs.StringVar(&f.MTLSCert, "mtls-cert", "", "path to mTLS certificate file")
	fs.StringVar(&f.MTLSKey, "mtls-key", "", "path to mTLS key file")
	fs.IntVar(&f.Port, "port", 8081, "listen port for the worker HTTP server")
	fs.StringVar(&f.CertDir, "cert-dir", "", "path to certificate directory")
	fs.StringVar(&f.ModelPath, "model-path", "", "path to the model file")
	fs.StringVar(&f.Backend, "backend", "llama-cpp", "backend adapter (llama-cpp, ollama, lmstudio, vllm, custom)")
	fs.StringVar(&f.Router, "router", "", "router base URL for HTTP registration (e.g. http://127.0.0.1:8080, dev-mode only)")
	fs.BoolVar(&f.EnableHealthChecks, "enable-health-checks", true, "enable periodic backend health checks")
}

// workerFlagUsage prints the worker subcommand's flag help to w.
func workerFlagUsage(w io.Writer) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	var f WorkerFlags
	registerWorkerFlags(fs, &f)
	fs.SetOutput(w)
	fs.Usage()
}

// runWorker runs the worker subcommand and returns the process exit code.
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
	// If neither specified, defaults to dev mode (backward compatible)
	isDevMode := true
	if f.ProdMode {
		isDevMode = false
	} else if f.DevMode {
		isDevMode = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start worker (RunWorker selects the backend adapter internally).
	h, err := worker.RunWorker(ctx, worker.RunConfig{
		ModelPath:          f.ModelPath,
		Backend:            f.Backend,
		Port:               f.Port,
		DevMode:            isDevMode,
		MTLSCert:           f.MTLSCert,
		MTLSKey:            f.MTLSKey,
		CertDir:            f.CertDir,
		RouterBase:         f.Router,
		EnableHealthChecks: f.EnableHealthChecks,
	})
	if err != nil {
		if errors.Is(err, worker.ErrMissingModelPath) {
			// Match the original CLI output: error line on stdout, then usage.
			slog.New(slog.NewTextHandler(os.Stdout, nil)).Error(err.Error())
			workerFlagUsage(os.Stderr)
			return 1
		}
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
