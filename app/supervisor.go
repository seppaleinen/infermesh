package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/routeraddr"
)

// default binary names — match the Makefile output layout
// (bin/infermesh-router, bin/infermesh-worker). Relative paths are resolved
// against the app's working directory, so when the app is launched from the
// repo root (the supported dev flow) the binaries are found automatically.
const (
	defaultRouterBinary = "bin/infermesh-router"
	defaultWorkerBinary = "bin/infermesh-worker"

	// healthCheckTimeout bounds every outbound health probe so a dead host
	// surfaces as an error instead of hanging the UI thread.
	healthCheckTimeout = 2 * time.Second
)

// supervisorServiceName is the Wails service name surfaced to the frontend.
const supervisorServiceName = "Supervisor"

const (
	roleRouter = "router"
	roleWorker = "worker"
)

// ProcessState is the lifecycle state of a supervised child process.
type ProcessState string

const (
	// StateStopped means the process is not running (never started, exited on
	// its own, or was stopped by the user).
	StateStopped ProcessState = "stopped"
	// StateRunning means the child is up and listening.
	StateRunning ProcessState = "running"
	// StateError means the process failed to start or exited abnormally.
	StateError ProcessState = "error"
)

// ProcessStatus is the UI projection of one supervised child process.
type ProcessStatus struct {
	State      ProcessState `json:"state"`
	Running    bool         `json:"running"`
	Healthy    bool         `json:"healthy"`
	Registered bool         `json:"registered"`
	PID        int          `json:"pid"`
	StartedAt  time.Time    `json:"started_at"`
	LogPath    string       `json:"log_path,omitempty"`
	Error      string       `json:"error,omitempty"`
}

// SupervisorStatus is the full status returned by Supervisor.Status().
type SupervisorStatus struct {
	Router    ProcessStatus `json:"router"`
	Worker    ProcessStatus `json:"worker"`
	RouterURL string        `json:"router_url,omitempty"`
}

// ProcessSpec describes where to find each binary; persisted via Settings.
// These fields are intentionally absent from the YAML schema and are kept as
// defaults here; a future settings-UI step can surface editable fields.
const (
	defaultRouterBinaryPath = "bin/infermesh-router"
	defaultWorkerBinaryPath = "bin/infermesh-worker"
)

var (
	errRouterAlreadyRunning = errors.New("router is already running")
	errWorkerAlreadyRunning = errors.New("worker is already running")
	errNoSavedSettings      = errors.New("no settings found")
	errBinaryNotFound       = errors.New("binary not found")
)

// Supervisor is a Wails service that launches and manages the headless
// router/worker binaries on behalf of the desktop app (issue #44).
//
// Wails-bound methods: StartRouter / StopRouter / StartWorker / StopWorker
// / Status / RestartRouter / RestartWorker.
//
// Secrets: the supervisor never holds secret values directly. When building
// child process args it resolves secrets from the keyring (router/apikey for
// --api-key, mtls/cert + mtls/key for --mtls-cert/--mtls-key) and never logs
// them. Dev mode (the default) requires no secrets.
type Supervisor struct {
	kr           Keyring
	settingsPath string
	binaryDir    string
	logsDir      string
	cmdBuilder   cmdBuilder // seam for tests; defaults to realCmdBuilder
	routerProbe  func(ctx context.Context, st Settings) ([]WorkerView, error)
	workerProbe  func(ctx context.Context, st Settings) (bool, error)

	// clientCertPaths holds the minted desktop client cert/key PEM paths so
	// RouterClient can build a TLS transport in prod mode. Set when the
	// router is started in prod mode; cleared on StopRouter.
	clientCertPaths [2]string // [0]=cert, [1]=key

	// caCertDir holds the temporary directory containing ca.crt for the router
	// to verify client certificates. Set when the router is started in prod
	// mode; cleared on StopRouter.
	caCertDir string

	// routerClient is the desktop RouterClient whose transport is updated
	// when the router starts/stops in prod mode. The Supervisor owns the
	// transport; RouterClient has zero knowledge of the Supervisor (issue
	// #103 R1).
	routerClient *RouterClient

	mu             sync.Mutex
	router         *managedProcess
	worker         *managedProcess
	workerStarting bool
	routerErr      error
	workerErr      error
	routerPID      int
	workerPID      int
	tempFiles      []string
}

// NewSupervisor creates a Wails service for process supervision.
//
//	kr:        keyring used to resolve secrets (router/apikey, mtls/*).
//	settingsPath: where Settings YAML lives (read on each Start so a saved
//	    change takes effect immediately).
//	binaryDir: base directory for resolving relative binary paths; defaults to
//	    the current working directory when empty.
//	logsDir:   directory for stdout/stderr capture; defaults to ~/.config/
//	    infermesh/logs.
func NewSupervisorService(kr Keyring, settingsPath, binaryDir, logsDir string) *Supervisor {
	if binaryDir == "" {
		binaryDir = "."
	}
	if logsDir == "" {
		logsDir = appLogsDir()
	}
	s := &Supervisor{
		kr:           kr,
		settingsPath: settingsPath,
		binaryDir:    binaryDir,
		logsDir:      logsDir,
		cmdBuilder:   realCmdBuilder,
	}
	s.routerProbe = s.defaultRouterProbe()
	s.workerProbe = s.defaultWorkerProbe()
	return s
}

// ServiceName implements application.Service for Wails binding discovery.
func (*Supervisor) ServiceName() string { return supervisorServiceName }

// SetRouterClient wires the desktop RouterClient into the Supervisor so the
// transport can be updated when the router starts/stops in prod mode. The
// client itself has zero knowledge of the Supervisor (issue #103 R1).
func (s *Supervisor) SetRouterClient(rc *RouterClient) {
	s.routerClient = rc
}

// resolveSecret reads a secret from the keyring by ref. A missing ref or
// missing value is not an error (the caller decides whether the secret is
// required). Keyring errors other than "not found" are surfaced. The value is
// never logged.
func (s *Supervisor) resolveSecret(ref string) (string, error) {
	if s.kr == nil {
		return "", nil
	}
	v, err := s.kr.Get(ref)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve secret %q: %w", ref, err)
	}
	return v, nil
}

// loadSettings reads the current Settings from disk and merges defaults so
// the supervisor always honours the latest saved configuration.
func (s *Supervisor) loadSettings() (Settings, error) {
	loaded, err := LoadFrom(s.settingsPath)
	if err != nil {
		return Settings{}, err
	}
	return mergeDefaults(loaded), nil
}

// binaryPath resolves a configured binary path. Relative paths become
// binaryDir/<path>; absolute paths pass through unchanged; an empty value
// uses the supplied default.
func (s *Supervisor) binaryPath(configured, defaultName string) string {
	p := strings.TrimSpace(configured)
	if p == "" {
		p = defaultName
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.binaryDir, p)
}

// routerURLForWorker normalises the router listen address into the base URL
// the worker needs for dev-mode HTTP registration. Delegates to
// pkg/routeraddr.Normalize (issue #103 R2).
func routerURLForWorker(routerAddr string) string {
	return routeraddr.Normalize(routerAddr)
}

// routerBaseURL normalises an address for HTTP health probes. Delegates to
// pkg/routeraddr.Normalize (issue #103 R2).
func routerBaseURL(addr string) string {
	return routeraddr.Normalize(addr)
}

// modeFromSettings resolves the dev/prod tri-state from Settings into a
// single routeraddr.Mode (issue #103 R2). nil DevMode means "old file /
// no opinion" → dev mode (the default).
func modeFromSettings(s Settings) routeraddr.Mode {
	if s.DevMode != nil && !*s.DevMode {
		return routeraddr.ModeProd
	}
	return routeraddr.ModeDev
}
