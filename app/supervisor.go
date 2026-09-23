package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

// defaultRouterAddr is the fallback router listen address when Settings is
// empty. It mirrors RouterClient's defaultRouterURL minus the scheme.
const defaultRouterAddr = "127.0.0.1:8080"

// Secret refs and env-var names used when building child process args. The
// refs mirror knownSecretRefs in config_service.go so the YAML only ever holds
// references; values come from the keyring at process-launch time.
const (
	secretRefRouterAPIKey    = "router/apikey"
	secretRefWorkerCustomAuth = "worker/customauth"
	secretRefMTLSCert        = "mtls/cert"
	secretRefMTLSKey         = "mtls/key"
	envWorkerCustomAuth      = "INFERMESH_WORKER_CUSTOM_AUTH"
	roleRouter               = "router"
	roleWorker               = "worker"
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
	kr            Keyring
	settingsPath  string
	binaryDir     string
	logsDir       string
	cmdBuilder    cmdBuilder // seam for tests; defaults to realCmdBuilder
	routerProbe   func(ctx context.Context, st Settings) ([]WorkerView, error)
	workerProbe   func(ctx context.Context, st Settings) (bool, error)

	mu         sync.Mutex
	router     *managedProcess
	worker     *managedProcess
	routerErr  error
	workerErr  error
	routerPID  int
	workerPID  int
	tempFiles  []string
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
func NewSupervisor(kr Keyring, settingsPath, binaryDir, logsDir string) *Supervisor {
	if binaryDir == "" {
		binaryDir = "."
	}
	if logsDir == "" {
		logsDir = appLogsDir()
	}
	s := &Supervisor{
		kr:            kr,
		settingsPath:  settingsPath,
		binaryDir:     binaryDir,
		logsDir:       logsDir,
		cmdBuilder:    realCmdBuilder,
	}
	s.routerProbe = s.defaultRouterProbe()
	s.workerProbe = s.defaultWorkerProbe()
	return s
}

// ServiceName implements application.Service for Wails binding discovery.
func (*Supervisor) ServiceName() string { return supervisorServiceName }

// defaultRouterProbe returns a router-reachability check built from
// RouterClient. It doubles as the worker-registration list source: a nil error
// means the router answered /v1/workers with 200, and the returned slice is
// the pool. Tests inject a stub returning canned WorkerViews.
func (s *Supervisor) defaultRouterProbe() func(ctx context.Context, st Settings) ([]WorkerView, error) {
	return func(ctx context.Context, st Settings) ([]WorkerView, error) {
		rc := NewRouterClient(routerBaseURL(st.RouterAddr))
		return rc.GetWorkers(ctx)
	}
}

// defaultWorkerProbe returns a worker-HTTP health check against the worker's
// /health endpoint.
func (s *Supervisor) defaultWorkerProbe() func(ctx context.Context, st Settings) (bool, error) {
	return func(ctx context.Context, st Settings) (bool, error) {
		return workerHealthy(ctx, st.WorkerPort)
	}
}

// ---- secret / config helpers ------------------------------------------------

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
// the worker needs for dev-mode HTTP registration, e.g. ":8080" ->
// "http://127.0.0.1:8080".
func routerURLForWorker(routerAddr string) string {
	a := strings.TrimSpace(routerAddr)
	if a == "" {
		return "http://" + defaultRouterAddr
	}
	if strings.Contains(a, "://") {
		return strings.TrimRight(a, "/")
	}
	if strings.HasPrefix(a, ":") {
		a = "127.0.0.1" + a
	}
	return "http://" + strings.TrimRight(a, "/")
}

// routerBaseURL normalises an address for HTTP health probes; bare ":8080"
// becomes "http://127.0.0.1:8080", a bare "host:8080" becomes
// "http://host:8080".
func routerBaseURL(addr string) string {
	a := strings.TrimSpace(addr)
	if a == "" {
		return "http://" + defaultRouterAddr
	}
	if strings.Contains(a, "://") {
		return strings.TrimRight(a, "/")
	}
	if strings.HasPrefix(a, ":") {
		a = "127.0.0.1" + a
	}
	return "http://" + strings.TrimRight(a, "/")
}

// isDevMode reports whether the saved settings request dev mode (the default).
func isDevMode(s Settings) bool {
	return s.DevMode == nil || *s.DevMode
}

// ---- arg / env building -----------------------------------------------------

// buildRouterArgs constructs the CLI flags for the router binary from the
// saved Settings. No secrets are logged here.
func (s *Supervisor) buildRouterArgs(settings Settings) ([]string, error) {
	var args []string
	if isDevMode(settings) {
		args = append(args, "--dev-mode")
	} else {
		args = append(args, "--prod-mode")
	}
	if settings.RouterAddr != "" {
		args = append(args, "--addr", settings.RouterAddr)
	}
	if settings.RelayURL != "" {
		args = append(args, "--relay-url", settings.RelayURL)
	}
	if !isDevMode(settings) {
		apiKey, err := s.resolveSecret(secretRefRouterAPIKey)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			args = append(args, "--api-key", apiKey)
		}
		cert, key, err := s.writeMTLSFiles()
		if err != nil {
			return nil, err
		}
		if cert != "" {
			args = append(args, "--mtls-cert", cert)
		}
		if key != "" {
			args = append(args, "--mtls-key", key)
		}
	}
	return args, nil
}

// buildWorkerArgs constructs the CLI flags for the worker binary.
func (s *Supervisor) buildWorkerArgs(settings Settings) ([]string, error) {
	var args []string
	if isDevMode(settings) {
		args = append(args, "--dev-mode")
	} else {
		args = append(args, "--prod-mode")
	}
	if settings.WorkerBackend != "" {
		args = append(args, "--backend", settings.WorkerBackend)
	}
	if settings.WorkerModelPath != "" {
		args = append(args, "--model-path", settings.WorkerModelPath)
	}
	if settings.WorkerPort > 0 {
		args = append(args, "--port", strconv.Itoa(settings.WorkerPort))
	}
	if isDevMode(settings) {
		// dev-mode HTTP registration against the local router (skips mDNS).
		args = append(args, "--router", routerURLForWorker(settings.RouterAddr))
	}
	if settings.RelayURL != "" {
		args = append(args, "--relay-url", settings.RelayURL)
	}
	if settings.WorkerEnableHealthChecks != nil {
		args = append(args, "--enable-health-checks", strconv.FormatBool(*settings.WorkerEnableHealthChecks))
	}
	if !isDevMode(settings) {
		cert, key, err := s.writeMTLSFiles()
		if err != nil {
			return nil, err
		}
		if cert != "" {
			args = append(args, "--mtls-cert", cert)
		}
		if key != "" {
			args = append(args, "--mtls-key", key)
		}
	}
	return args, nil
}

// buildEnv returns the child process environment. We inherit the app's env
// and, when the worker has a configured custom-auth secret, surface it under
// a documented env var (the worker's `custom` backend can consume it). The
// value is resolved from the keyring and is never logged.
func (s *Supervisor) buildEnv(settings Settings) ([]string, error) {
	env := os.Environ()
	if auth, err := s.resolveSecret(secretRefWorkerCustomAuth); err != nil {
		return nil, err
	} else if auth != "" {
		env = append(env, envWorkerCustomAuth+"="+auth)
	}
	return env, nil
}

// writeMTLSFiles resolves the mtls/cert and mtls/key secrets from the keyring
// into temp files (mode 0600) and records them for cleanup. Returns empty
// paths when the secrets are absent. The file contents are never logged.
func (s *Supervisor) writeMTLSFiles() (certPath, keyPath string, err error) {
	cert, err := s.resolveSecret(secretRefMTLSCert)
	if err != nil {
		return "", "", err
	}
	key, err := s.resolveSecret(secretRefMTLSKey)
	if err != nil {
		return "", "", err
	}
	if cert == "" && key == "" {
		return "", "", nil
	}
	if err := os.MkdirAll(s.logsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("ensure log dir: %w", err)
	}
	if cert != "" {
		if certPath, err = writeTempFile(s.logsDir, "infermesh-mtls-cert-*.pem", cert); err != nil {
			return "", "", err
		}
		s.mu.Lock()
		s.tempFiles = append(s.tempFiles, certPath)
		s.mu.Unlock()
	}
	if key != "" {
		if keyPath, err = writeTempFile(s.logsDir, "infermesh-mtls-key-*.pem", key); err != nil {
			return "", "", err
		}
		s.mu.Lock()
		s.tempFiles = append(s.tempFiles, keyPath)
		s.mu.Unlock()
	}
	return certPath, keyPath, nil
}

// cleanupTempFiles removes any temp files created for mTLS secrets.
func (s *Supervisor) cleanupTempFiles() {
	s.mu.Lock()
	files := s.tempFiles
	s.tempFiles = nil
	s.mu.Unlock()
	for _, f := range files {
		_ = os.Remove(f)
	}
}

// logPath resolves the per-role log file under logsDir.
func (s *Supervisor) logPath(role string) string {
	return filepath.Join(s.logsDir, role+".log")
}

// ensureLogsDir makes sure the log directory exists.
func (s *Supervisor) ensureLogsDir() error {
	return os.MkdirAll(s.logsDir, 0o755)
}

// writeTempFile writes data to a 0600 temp file in dir with the given glob
// pattern and returns the path.
func writeTempFile(dir, pattern, data string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	name := f.Name()
	if _, err := io.WriteString(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// appLogsDir returns the standard per-user log directory (~/.config/infermesh/logs).
func appLogsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "logs")
	}
	return filepath.Join(home, ".config", "infermesh", "logs")
}

// ---- Wails-bound actions -----------------------------------------------------

// StartRouter launches the router binary from the saved config. It resolves
// secrets from the keyring only when running in prod mode; dev mode needs none.
func (s *Supervisor) StartRouter() error {
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	s.mu.Lock()
	mp := s.router
	s.mu.Unlock()
	if mp != nil && mp.isAlive() {
		return errRouterAlreadyRunning
	}
	binaryPath := s.binaryPath(settings.RouterBinaryPath, defaultRouterBinaryPath)
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("%w: %s", errBinaryNotFound, binaryPath)
	}

	args, err := s.buildRouterArgs(settings)
	if err != nil {
		return err
	}
	env, err := s.buildEnv(settings)
	if err != nil {
		return err
	}
	logPath := s.logPath(roleRouter)
	if err := s.ensureLogsDir(); err != nil {
		return err
	}

	mp = newManagedProcess(logPath, s.cmdBuilder)
	if err := mp.start(binaryPath, args, env); err != nil {
		s.mu.Lock()
		s.router = mp
		s.routerErr = err
		s.routerPID = 0
		s.mu.Unlock()
		return err
	}

	s.mu.Lock()
	s.router = mp
	s.routerErr = nil
	s.routerPID = mp.pid
	s.mu.Unlock()
	return nil
}

// StopRouter stops the router child (graceful SIGTERM → SIGKILL). Returns nil
// if the router was not running.
func (s *Supervisor) StopRouter() error {
	s.mu.Lock()
	mp := s.router
	s.mu.Unlock()
	if mp == nil {
		return nil
	}
	if err := mp.stop(); err != nil {
		return err
	}
	return nil
}

// StartWorker launches the worker binary from the saved config.
func (s *Supervisor) StartWorker() error {
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	s.mu.Lock()
	mp := s.worker
	s.mu.Unlock()
	if mp != nil && mp.isAlive() {
		return errWorkerAlreadyRunning
	}
	binaryPath := s.binaryPath(settings.WorkerBinaryPath, defaultWorkerBinaryPath)
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("%w: %s", errBinaryNotFound, binaryPath)
	}

	args, err := s.buildWorkerArgs(settings)
	if err != nil {
		return err
	}
	env, err := s.buildEnv(settings)
	if err != nil {
		return err
	}
	logPath := s.logPath(roleWorker)
	if err := s.ensureLogsDir(); err != nil {
		return err
	}

	mp = newManagedProcess(logPath, s.cmdBuilder)
	if err := mp.start(binaryPath, args, env); err != nil {
		s.mu.Lock()
		s.worker = mp
		s.workerErr = err
		s.workerPID = 0
		s.mu.Unlock()
		return err
	}

	s.mu.Lock()
	s.worker = mp
	s.workerErr = nil
	s.workerPID = mp.pid
	s.mu.Unlock()
	return nil
}

// StopWorker stops the worker child. Returns nil if not running.
func (s *Supervisor) StopWorker() error {
	s.mu.Lock()
	mp := s.worker
	s.mu.Unlock()
	if mp == nil {
		return nil
	}
	return mp.stop()
}

// RestartRouter stops then starts the router.
func (s *Supervisor) RestartRouter() error {
	_ = s.StopRouter()
	return s.StartRouter()
}

// RestartWorker stops then starts the worker.
func (s *Supervisor) RestartWorker() error {
	_ = s.StopWorker()
	return s.StartWorker()
}

// StopAll stops both children and removes temp files. Intended for use on
// application shutdown.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	w := s.worker
	r := s.router
	s.mu.Unlock()
	if w != nil {
		_ = w.stop()
	}
	if r != nil {
		_ = r.stop()
	}
	s.cleanupTempFiles()
}

// IsRouterRunning reports whether the router child is alive.
func (s *Supervisor) IsRouterRunning() bool {
	s.mu.Lock()
	mp := s.router
	s.mu.Unlock()
	return mp != nil && mp.isAlive()
}

// IsWorkerRunning reports whether the worker child is alive.
func (s *Supervisor) IsWorkerRunning() bool {
	s.mu.Lock()
	mp := s.worker
	s.mu.Unlock()
	return mp != nil && mp.isAlive()
}

// ---- health & status --------------------------------------------------------

// workerHealthy performs an HTTP probe against the worker's /health endpoint.
func workerHealthy(ctx context.Context, port int) (bool, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}

// workerRegistered reports whether a worker on the given port appears in the
// router's /v1/workers pool.
func workerRegistered(workers []WorkerView, port int) bool {
	for _, w := range workers {
		if w.Port == port {
			return true
		}
	}
	return false
}

// buildProcessStatus maps a processInfo snapshot onto the UI projection. When
// the process is not running, startErr/exitCode are used to populate State and
// Error. When running, healthy is recorded.
func buildProcessStatus(info processInfo, startErr error, healthy, registered bool) ProcessStatus {
	ps := ProcessStatus{
		LogPath:    info.LogPath,
		StartedAt:  info.StartedAt,
		PID:        info.PID,
		Registered: registered,
	}
	if !info.Running {
		if startErr != nil && info.PID == 0 && info.ExitCode < 0 {
			ps.State = StateError
			ps.Error = startErr.Error()
			return ps
		}
		if info.ExitCode > 0 {
			ps.State = StateError
			ps.Error = fmt.Sprintf("process exited with code %d", info.ExitCode)
		} else {
			ps.State = StateStopped
		}
		return ps
	}
	ps.State = StateRunning
	ps.Running = true
	ps.Healthy = healthy
	if !healthy {
		ps.Error = "health check failed"
	}
	return ps
}

// Status reports the current state of both supervised children plus a live
// health probe against the router (which also yields the worker-registration
// list). It never errors: an unreachable router simply reads as unhealthy.
func (s *Supervisor) Status() (SupervisorStatus, error) {
	settings, err := s.loadSettings()
	if err != nil {
		// No settings yet: report everything stopped rather than failing, so
		// the UI can show the "nothing configured" state and prompt the user.
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			return SupervisorStatus{
				Router: ProcessStatus{State: StateStopped},
				Worker: ProcessStatus{State: StateStopped},
			}, nil
		}
		return SupervisorStatus{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	s.mu.Lock()
	var rInfo, wInfo processInfo
	var rErr, wErr error
	if s.router != nil {
		rInfo = s.router.status()
		rErr = s.routerErr
	}
	if s.worker != nil {
		wInfo = s.worker.status()
		wErr = s.workerErr
	}
	s.mu.Unlock()

	// Probe the router; a successful GetWorkers means it answered /v1/workers
	// with 200 and gives us the pool to test worker registration against.
	pool, rerr := s.routerProbe(ctx, settings)
	routerUp := rerr == nil

	// Probe the worker's own HTTP health endpoint (independent of the router).
	workerUp := false
	if s.workerProbe != nil {
		workerUp, _ = s.workerProbe(ctx, settings)
	}

	return SupervisorStatus{
		Router:    buildProcessStatus(rInfo, rErr, routerUp, false),
		Worker:    buildProcessStatus(wInfo, wErr, workerUp, workerRegistered(pool, settings.WorkerPort)),
		RouterURL: routerBaseURL(settings.RouterAddr),
	}, nil
}
