package main

// Unit tests for the process supervisor (issue #44). The Supervisor launches
// the headless router/worker binaries; these tests drive it with an in-memory
// fake childCmd so no real process is ever spawned. See supervisor.go and
// supervisor_process.go for the production code under test.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// ---- fake childCmd ----------------------------------------------------------

// fakeChildPID is deliberately not a real PID: managedProcess.stop() issues a
// real syscall.Kill(-pid, SIGTERM) against it, so the value must be far away
// from any real process group (the syscall returns ESRCH, harmlessly
// discarded).
const fakeChildPID = 424242

// fakeChildCmd is a deterministic in-memory childCmd double. It records every
// call and models the process lifecycle: Start() -> running; finish() ->
// exited (unblocks Wait() and makes liveness probes report not-running).
type fakeChildCmd struct {
	mu sync.Mutex

	startErr error // scripted Start() failure
	exitErr  error // error Wait() returns after finish()

	name   string
	args   []string
	env    []string
	stdout io.Writer
	stderr io.Writer

	started  bool
	finished bool
	done     chan struct{} // closed by finish()

	startCalls int
	waitCalls  int
	killCalls  int
	signals    []os.Signal
}

func newFakeChildCmd(name string, args ...string) *fakeChildCmd {
	return &fakeChildCmd{name: name, args: args, done: make(chan struct{})}
}

func (f *fakeChildCmd) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.started {
		return errors.New("fake child: already started")
	}
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeChildCmd) Wait() error {
	f.mu.Lock()
	f.waitCalls++
	done := f.done
	f.mu.Unlock()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitErr
}

func (f *fakeChildCmd) Signal(sig os.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// nil and syscall.Signal(0) are the liveness probe used by
	// isAlive()/status(); probe calls are not recorded so they don't corrupt
	// signal-sequence expectations.
	if sig == nil || sig == syscall.Signal(0) {
		if !f.started || f.finished {
			return errors.New("fake child: not running")
		}
		return nil
	}
	f.signals = append(f.signals, sig)
	return nil
}

func (f *fakeChildCmd) Kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls++
	return nil
}

func (f *fakeChildCmd) Pid() int { return fakeChildPID }

func (f *fakeChildCmd) SetEnv(env []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env = append([]string(nil), env...)
}

func (f *fakeChildCmd) SetStdout(w io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stdout = w
}

func (f *fakeChildCmd) SetStderr(w io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stderr = w
}

func (f *fakeChildCmd) Args() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror *exec.Cmd.Args semantics: the binary name is Args[0].
	return append([]string{f.name}, f.args...)
}

// finish simulates the child exiting: it unblocks Wait() and flips the
// liveness state so subsequent liveness probes (Signal(nil) or
// Signal(syscall.Signal(0))) report not-running. It is idempotent and safe
// for concurrent use.
func (f *fakeChildCmd) finish(exitErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished {
		return
	}
	f.finished = true
	if exitErr != nil {
		f.exitErr = exitErr
	}
	close(f.done)
}

// ---- test accessors (lock-protected) ----------------------------------------

func (f *fakeChildCmd) Env() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.env...)
}

func (f *fakeChildCmd) Stdout() io.Writer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdout
}

func (f *fakeChildCmd) Stderr() io.Writer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stderr
}

func (f *fakeChildCmd) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls
}

func (f *fakeChildCmd) KillCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killCalls
}

func (f *fakeChildCmd) SignalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.signals)
}

// ---- fake cmdBuilder --------------------------------------------------------

// fakeCmdBuilder is the seam behind Supervisor.cmdBuilder. It records every
// fake it produces so tests can inspect args/env/writers and script exits.
type fakeCmdBuilder struct {
	mu       sync.Mutex
	startErr error // when set, every produced fake fails Start()
	fakes    []*fakeChildCmd
}

func newFakeCmdBuilder() *fakeCmdBuilder { return &fakeCmdBuilder{} }

func (b *fakeCmdBuilder) build(name string, args ...string) childCmd {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := newFakeChildCmd(name, args...)
	f.startErr = b.startErr
	b.fakes = append(b.fakes, f)
	return f
}

func (b *fakeCmdBuilder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.fakes)
}

func (b *fakeCmdBuilder) at(i int) *fakeChildCmd {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.fakes) {
		return nil
	}
	return b.fakes[i]
}

func (b *fakeCmdBuilder) last() *fakeChildCmd {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.fakes) == 0 {
		return nil
	}
	return b.fakes[len(b.fakes)-1]
}

// finishAll exits every produced fake. Registered via t.Cleanup so started
// children always have their Wait() unblocked and no goroutines leak.
func (b *fakeCmdBuilder) finishAll() {
	b.mu.Lock()
	fakes := append([]*fakeChildCmd(nil), b.fakes...)
	b.mu.Unlock()
	for _, f := range fakes {
		f.finish(nil)
	}
}

// ---- supervisor test harness -------------------------------------------------

// supervisorFixture bundles a Supervisor wired with a fake cmdBuilder and
// hard-failing probe stubs (no test may hit a real router/worker over HTTP).
type supervisorFixture struct {
	s            *Supervisor
	builder      *fakeCmdBuilder
	kr           *memKeyring
	settings     Settings
	settingsPath string
	logsDir      string
	binDir       string
}

func newTestSupervisor(t *testing.T) *supervisorFixture {
	t.Helper()
	settingsDir := t.TempDir()
	binDir := t.TempDir()
	logsDir := t.TempDir()

	fx := &supervisorFixture{
		kr:           newMemKeyring(),
		settings:     DefaultSettings(),
		settingsPath: filepath.Join(settingsDir, "settings.yml"),
		logsDir:      logsDir,
		binDir:       binDir,
	}
	fx.settings.RouterBinaryPath = writeDummyBinary(t, binDir, "infermesh-router")
	fx.settings.WorkerBinaryPath = writeDummyBinary(t, binDir, "infermesh-worker")

	s := NewSupervisor(fx.kr, fx.settingsPath, binDir, logsDir)
	fx.builder = newFakeCmdBuilder()
	s.cmdBuilder = fx.builder.build
	// Replace the real HTTP probes with stubs that fail loudly if a test
	// forgets to override them.
	s.routerProbe = func(ctx context.Context, st Settings) ([]WorkerView, error) {
		return nil, errors.New("router probe not stubbed")
	}
	s.workerProbe = func(ctx context.Context, st Settings) (bool, error) {
		return false, errors.New("worker probe not stubbed")
	}
	fx.s = s

	t.Cleanup(fx.builder.finishAll)
	return fx
}

// save persists the fixture settings to disk (Start*/Status read them via
// loadSettings).
func (fx *supervisorFixture) save(t *testing.T) {
	t.Helper()
	if err := SaveTo(fx.settings, fx.settingsPath); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
}

// writeDummyBinary creates an executable-looking file so os.Stat passes for
// binaryPath checks (the binary itself is never executed).
func writeDummyBinary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write dummy binary %s: %v", p, err)
	}
	return p
}

// ---- helpers ----------------------------------------------------------------

// assertStatusMatches checks the UI-projected ProcessStatus fields that the
// caller cares about. Error is compared as a substring when non-empty.
func assertStatusMatches(t *testing.T, label string, got, want ProcessStatus) {
	t.Helper()
	if got.State != want.State {
		t.Errorf("%s State = %q, want %q", label, got.State, want.State)
	}
	if got.Running != want.Running {
		t.Errorf("%s Running = %v, want %v", label, got.Running, want.Running)
	}
	if got.Healthy != want.Healthy {
		t.Errorf("%s Healthy = %v, want %v", label, got.Healthy, want.Healthy)
	}
	if got.Registered != want.Registered {
		t.Errorf("%s Registered = %v, want %v", label, got.Registered, want.Registered)
	}
	if got.PID != want.PID {
		t.Errorf("%s PID = %d, want %d", label, got.PID, want.PID)
	}
	if want.Error != "" {
		if !strings.Contains(got.Error, want.Error) {
			t.Errorf("%s Error = %q, want it to contain %q", label, got.Error, want.Error)
		}
	} else if got.Error != "" {
		t.Errorf("%s Error = %q, want empty", label, got.Error)
	}
}

// assertArgPair finds flag in args and asserts the following element is wantVal.
func assertArgPair(t *testing.T, args []string, flag, wantVal string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			if args[i+1] != wantVal {
				t.Errorf("%s = %q, want %q (args %q)", flag, args[i+1], wantVal, args)
			}
			return
		}
	}
	t.Errorf("args missing %s (got %q)", flag, args)
}

func argsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func assertFileModeAndContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("content of %s mismatch:\n got: %q\nwant: %q", path, string(data), want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode of %s = %04o, want 0600", path, perm)
	}
}

// ---- tests ------------------------------------------------------------------

// TestSupervisorStartRouterDevMode asserts the dev-mode launch contract: exact
// args, env wired, stdout/stderr captured to logsDir/router.log, and the child
// reported running.
func TestSupervisorStartRouterDevMode(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Fatalf("cmdBuilder invocations = %d, want 1", got)
	}
	fake := fx.builder.last()
	if fake == nil {
		t.Fatal("no child cmd captured")
	}

	wantArgs := []string{fx.settings.RouterBinaryPath, "--dev-mode", "--addr", ":8080"}
	if got := fake.Args(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("router args mismatch:\n got: %q\nwant: %q", got, wantArgs)
	}
	if got, want := fake.Env(), os.Environ(); !reflect.DeepEqual(got, want) {
		t.Errorf("router env mismatch: got %d entries, want %d", len(got), len(want))
	}
	if len(fake.Env()) == 0 {
		t.Error("SetEnv was not called with a non-empty environment")
	}
	if fake.Stdout() == nil || fake.Stderr() == nil {
		t.Error("stdout/stderr writers were not wired")
	}
	logPath := filepath.Join(fx.logsDir, "router.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("router.log was not created: %v", err)
	}
	if got := fx.s.logPath(roleRouter); got != logPath {
		t.Errorf("logPath(router) = %q, want %q", got, logPath)
	}
	if !fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = false, want true")
	}
}

// TestSupervisorStartRouterProdModeArgs asserts prod-mode builds --prod-mode,
// --api-key, and writes the mTLS cert/key secrets to 0600 temp files under
// logsDir, and that StopAll removes those temp files.
func TestSupervisorStartRouterProdModeArgs(t *testing.T) {
	fx := newTestSupervisor(t)

	const apiKey = "test-router-key"
	const certData = "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----\n"
	const keyData = "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----\n"
	fx.kr.Set(secretRefRouterAPIKey, apiKey)
	fx.kr.Set(secretRefMTLSCert, certData)
	fx.kr.Set(secretRefMTLSKey, keyData)

	prod := false
	fx.settings.DevMode = &prod
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	args := fx.builder.last().Args()

	if !argsContain(args, "--prod-mode") {
		t.Errorf("args missing --prod-mode (got %q)", args)
	}
	if argsContain(args, "--dev-mode") {
		t.Errorf("prod-mode args must not contain --dev-mode (got %q)", args)
	}
	assertArgPair(t, args, "--api-key", apiKey)

	var certPath, keyPath string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--mtls-cert":
			certPath = args[i+1]
		case "--mtls-key":
			keyPath = args[i+1]
		}
	}
	if certPath == "" || keyPath == "" {
		t.Fatalf("args missing --mtls-cert/--mtls-key (got %q)", args)
	}
	if !strings.HasPrefix(certPath, fx.logsDir) || !strings.HasPrefix(keyPath, fx.logsDir) {
		t.Errorf("mTLS temp files should live under logsDir: cert=%q key=%q", certPath, keyPath)
	}
	assertFileModeAndContent(t, certPath, certData)
	assertFileModeAndContent(t, keyPath, keyData)

	// StopAll must tear down the child and remove the temp files.
	fx.builder.last().finish(nil)
	fx.s.StopAll()
	if _, err := os.Stat(certPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cert temp file still exists after StopAll (stat err: %v)", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key temp file still exists after StopAll (stat err: %v)", err)
	}
}

// TestSupervisorStartRouterAlreadyRunning encodes the issue #44 verification
// contract: a second StartRouter while the router is up must NOT launch a
// second child and MUST return errRouterAlreadyRunning. Regression: the
// already-running guard on StartRouter uses isAlive(), so only a live child
// trips it — a failed start (mp non-nil, not alive) can be retried.
func TestSupervisorStartRouterAlreadyRunning(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("first StartRouter: %v", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Fatalf("cmdBuilder invocations after first start = %d, want 1", got)
	}

	err := fx.s.StartRouter()
	if !errors.Is(err, errRouterAlreadyRunning) {
		t.Errorf("second StartRouter error = %v, want errRouterAlreadyRunning", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Errorf("second StartRouter launched another child: cmdBuilder invocations = %d, want 1", got)
	}
}

// TestSupervisorStartWorkerAlreadyRunning is the worker-side twin of the
// already-running contract above: a second StartWorker while the worker is up
// must NOT launch a second child and MUST return errWorkerAlreadyRunning.
func TestSupervisorStartWorkerAlreadyRunning(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartWorker(); err != nil {
		t.Fatalf("first StartWorker: %v", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Fatalf("cmdBuilder invocations after first start = %d, want 1", got)
	}

	err := fx.s.StartWorker()
	if !errors.Is(err, errWorkerAlreadyRunning) {
		t.Errorf("second StartWorker error = %v, want errWorkerAlreadyRunning", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Errorf("second StartWorker launched another child: cmdBuilder invocations = %d, want 1", got)
	}
}

// TestSupervisorRestartRouter guards the already-running guard against
// breaking restart: RestartRouter must stop the live child first, then launch
// a fresh one — it must NOT return errRouterAlreadyRunning. The first fake is
// finished before the restart to model the child responding to SIGTERM, so
// StopRouter returns promptly and StartRouter proceeds to build a second child.
func TestSupervisorRestartRouter(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Fatalf("cmdBuilder invocations after first start = %d, want 1", got)
	}
	fx.builder.at(0).finish(nil) // child exits on SIGTERM before RestartRouter

	if err := fx.s.RestartRouter(); err != nil {
		t.Fatalf("RestartRouter: %v", err)
	}
	if got := fx.builder.count(); got != 2 {
		t.Errorf("cmdBuilder invocations after restart = %d, want 2", got)
	}
	if !fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = false after restart, want true")
	}
}

// TestSupervisorRestartWorker is the worker-side twin: RestartWorker must not
// trip the already-running guard and must start a fresh child.
func TestSupervisorRestartWorker(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartWorker(); err != nil {
		t.Fatalf("StartWorker: %v", err)
	}
	if got := fx.builder.count(); got != 1 {
		t.Fatalf("cmdBuilder invocations after first start = %d, want 1", got)
	}
	fx.builder.at(0).finish(nil) // child exits on SIGTERM before RestartWorker

	if err := fx.s.RestartWorker(); err != nil {
		t.Fatalf("RestartWorker: %v", err)
	}
	if got := fx.builder.count(); got != 2 {
		t.Errorf("cmdBuilder invocations after restart = %d, want 2", got)
	}
	if !fx.s.IsWorkerRunning() {
		t.Error("IsWorkerRunning() = false after restart, want true")
	}
}

// TestSupervisorStartRouterBinaryMissing asserts a missing binary path fails
// with errBinaryNotFound's text and no child is launched.
func TestSupervisorStartRouterBinaryMissing(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.settings.RouterBinaryPath = filepath.Join(fx.binDir, "no-such-router")
	fx.save(t)

	err := fx.s.StartRouter()
	if err == nil {
		t.Fatal("StartRouter with missing binary: expected error, got nil")
	}
	if !strings.Contains(err.Error(), errBinaryNotFound.Error()) {
		t.Errorf("error = %v, want it to contain %q", err, errBinaryNotFound.Error())
	}
	if got := fx.builder.count(); got != 0 {
		t.Errorf("cmdBuilder invocations = %d, want 0", got)
	}
	if fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = true, want false")
	}
}

// TestSupervisorStartWorkerBinaryMissing is the worker twin of the missing
// binary contract.
func TestSupervisorStartWorkerBinaryMissing(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.settings.WorkerBinaryPath = filepath.Join(fx.binDir, "no-such-worker")
	fx.save(t)

	err := fx.s.StartWorker()
	if err == nil {
		t.Fatal("StartWorker with missing binary: expected error, got nil")
	}
	if !strings.Contains(err.Error(), errBinaryNotFound.Error()) {
		t.Errorf("error = %v, want it to contain %q", err, errBinaryNotFound.Error())
	}
	if got := fx.builder.count(); got != 0 {
		t.Errorf("cmdBuilder invocations = %d, want 0", got)
	}
	if fx.s.IsWorkerRunning() {
		t.Error("IsWorkerRunning() = true, want false")
	}
}

// TestSupervisorStartRouterStartError asserts a cmd.Start() failure is
// surfaced (wrapped), does not panic, and leaves the router not running. It
// also asserts the Status() projection contract: a process that failed to
// start with PID 0 must render StateError (buildProcessStatus, supervisor.go
// ~line 643). Regression: managedProcess.start records exitCode=-1 on the
// cmd.Start() error path so the StateError branch is reachable.
func TestSupervisorStartRouterStartError(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	startErr := errors.New("exec: permission denied")
	fx.builder.startErr = startErr

	err := fx.s.StartRouter()
	if err == nil {
		t.Fatal("StartRouter with failing Start(): expected error, got nil")
	}
	if !strings.Contains(err.Error(), startErr.Error()) {
		t.Errorf("StartRouter error = %v, want it to wrap %q", err, startErr.Error())
	}
	if fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = true after failed start, want false")
	}

	status, serr := fx.s.Status()
	if serr != nil {
		t.Fatalf("Status: %v", serr)
	}
	if status.Router.State != StateError {
		t.Errorf("Status().Router.State = %q, want %q (issue #44 contract: start failure must render StateError)", status.Router.State, StateError)
	}
	if status.Router.Running {
		t.Error("Status().Router.Running = true after failed start, want false")
	}
}

// TestSupervisorStopRouter asserts stop returns promptly after the child
// exits, the router is reported not running, Status renders StateStopped, and
// stopping when nothing is running is an idempotent no-op.
func TestSupervisorStopRouter(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	fx.builder.last().finish(nil)

	if err := fx.s.StopRouter(); err != nil {
		t.Fatalf("StopRouter: %v", err)
	}
	if fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = true after stop, want false")
	}

	status, err := fx.s.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Router.State != StateStopped {
		t.Errorf("Status().Router.State = %q, want %q", status.Router.State, StateStopped)
	}
	if status.Router.Running {
		t.Error("Status().Router.Running = true after stop, want false")
	}

	// Idempotent: stopping an already-stopped router returns nil.
	if err := fx.s.StopRouter(); err != nil {
		t.Errorf("second StopRouter: %v, want nil", err)
	}
	// Stopping a never-started supervisor is also a no-op.
	fx2 := newTestSupervisor(t)
	if err := fx2.s.StopRouter(); err != nil {
		t.Errorf("StopRouter on never-started supervisor: %v, want nil", err)
	}
	if err := fx2.s.StopWorker(); err != nil {
		t.Errorf("StopWorker on never-started supervisor: %v, want nil", err)
	}
}

// TestSupervisorStartWorkerDevMode asserts the exact worker argv in dev mode,
// including the always-present --enable-health-checks flag (mergeDefaults
// forces it non-nil) and the --router flag derived from routerURLForWorker.
func TestSupervisorStartWorkerDevMode(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Settings)
		wantArgs []string // [0] is the binary path placeholder
	}{
		{
			name: "defaults plus lmstudio model",
			mutate: func(s *Settings) {
				s.WorkerBackend = "lmstudio"
				s.WorkerModelPath = "/models/foo"
			},
			wantArgs: []string{"", "--dev-mode", "--backend", "lmstudio", "--model-path", "/models/foo",
				"--port", "8081", "--router", "http://127.0.0.1:8080", "--enable-health-checks", "true"},
		},
		{
			name: "health checks disabled",
			mutate: func(s *Settings) {
				s.WorkerEnableHealthChecks = boolPtr(false)
			},
			wantArgs: []string{"", "--dev-mode", "--backend", "llama-cpp",
				"--port", "8081", "--router", "http://127.0.0.1:8080", "--enable-health-checks", "false"},
		},
		{
			name: "relay url set",
			mutate: func(s *Settings) {
				s.RelayURL = "wss://relay.example.com"
			},
			wantArgs: []string{"", "--dev-mode", "--backend", "llama-cpp",
				"--port", "8081", "--router", "http://127.0.0.1:8080", "--relay-url", "wss://relay.example.com",
				"--enable-health-checks", "true"},
		},
		{
			name: "custom router addr",
			mutate: func(s *Settings) {
				s.RouterAddr = "127.0.0.1:9000"
			},
			wantArgs: []string{"", "--dev-mode", "--backend", "llama-cpp",
				"--port", "8081", "--router", "http://127.0.0.1:9000", "--enable-health-checks", "true"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestSupervisor(t)
			tc.mutate(&fx.settings)
			fx.save(t)

			if err := fx.s.StartWorker(); err != nil {
				t.Fatalf("StartWorker: %v", err)
			}
			want := append([]string(nil), tc.wantArgs...)
			want[0] = fx.settings.WorkerBinaryPath
			if got := fx.builder.last().Args(); !reflect.DeepEqual(got, want) {
				t.Errorf("worker args mismatch:\n got: %q\nwant: %q", got, want)
			}
			if !fx.s.IsWorkerRunning() {
				t.Error("IsWorkerRunning() = false, want true")
			}
		})
	}
}

// TestSupervisorStatusTransitions exercises the Status() projection across the
// lifecycle: running+healthy, unregistered, unhealthy probe, never started,
// abnormal exit, failed start, and missing settings.
func TestSupervisorStatusTransitions(t *testing.T) {
	probeWorkerUp := func(ctx context.Context, st Settings) (bool, error) { return true, nil }

	cases := []struct {
		name          string
		setup         func(t *testing.T, fx *supervisorFixture)
		saveSettings  bool
		routerProbe   func(ctx context.Context, st Settings) ([]WorkerView, error)
		workerProbe   func(ctx context.Context, st Settings) (bool, error)
		wantRouter    ProcessStatus
		wantWorker    ProcessStatus
		wantRouterURL string
	}{
		{
			name: "router and worker running, worker registered",
			setup: func(t *testing.T, fx *supervisorFixture) {
				if err := fx.s.StartRouter(); err != nil {
					t.Fatalf("StartRouter: %v", err)
				}
				if err := fx.s.StartWorker(); err != nil {
					t.Fatalf("StartWorker: %v", err)
				}
			},
			saveSettings: true,
			routerProbe: func(ctx context.Context, st Settings) ([]WorkerView, error) {
				return []WorkerView{{ID: "w1", Port: st.WorkerPort}}, nil
			},
			workerProbe:   probeWorkerUp,
			wantRouter:    ProcessStatus{State: StateRunning, Running: true, Healthy: true, PID: fakeChildPID},
			wantWorker:    ProcessStatus{State: StateRunning, Running: true, Healthy: true, Registered: true, PID: fakeChildPID},
			wantRouterURL: "http://127.0.0.1:8080",
		},
		{
			name: "running but not registered on router",
			setup: func(t *testing.T, fx *supervisorFixture) {
				if err := fx.s.StartRouter(); err != nil {
					t.Fatalf("StartRouter: %v", err)
				}
				if err := fx.s.StartWorker(); err != nil {
					t.Fatalf("StartWorker: %v", err)
				}
			},
			saveSettings: true,
			routerProbe: func(ctx context.Context, st Settings) ([]WorkerView, error) {
				return []WorkerView{{ID: "w1", Port: 9999}}, nil
			},
			workerProbe: probeWorkerUp,
			wantRouter:  ProcessStatus{State: StateRunning, Running: true, Healthy: true, PID: fakeChildPID},
			wantWorker:  ProcessStatus{State: StateRunning, Running: true, Healthy: true, Registered: false, PID: fakeChildPID},
		},
		{
			name: "running but health probe fails",
			setup: func(t *testing.T, fx *supervisorFixture) {
				if err := fx.s.StartRouter(); err != nil {
					t.Fatalf("StartRouter: %v", err)
				}
			},
			saveSettings: true,
			routerProbe: func(ctx context.Context, st Settings) ([]WorkerView, error) {
				return nil, errors.New("connection refused")
			},
			workerProbe: probeWorkerUp,
			wantRouter:  ProcessStatus{State: StateRunning, Running: true, Healthy: false, PID: fakeChildPID, Error: "health check failed"},
			wantWorker:  ProcessStatus{State: StateStopped},
		},
		{
			name:         "never started",
			saveSettings: true,
			wantRouter:   ProcessStatus{State: StateStopped},
			wantWorker:   ProcessStatus{State: StateStopped},
		},
		{
			name: "worker exited with non-zero code",
			setup: func(t *testing.T, fx *supervisorFixture) {
				if err := fx.s.StartWorker(); err != nil {
					t.Fatalf("StartWorker: %v", err)
				}
				// A non-ExitError maps to exit code 1 in managedProcess.waitExit.
				fx.builder.last().finish(errors.New("backend crashed"))
			},
			saveSettings: true,
			wantRouter:   ProcessStatus{State: StateStopped},
			wantWorker:   ProcessStatus{State: StateError, PID: fakeChildPID, Error: "exited with code 1"},
		},
		{
			name: "worker start failed",
			setup: func(t *testing.T, fx *supervisorFixture) {
				fx.builder.startErr = errors.New("start boom")
				if err := fx.s.StartWorker(); err == nil {
					t.Fatal("StartWorker should fail with scripted startErr")
				}
			},
			saveSettings: true,
			wantRouter:   ProcessStatus{State: StateStopped},
			// buildProcessStatus contract: a process whose start failed (startErr
			// set, PID 0, exit code -1) renders StateError with the wrapped
			// message ("start <binary>: start boom"); the same shape as
			// TestSupervisorStartRouterStartError.
			wantWorker: ProcessStatus{State: StateError, Error: "start boom"},
		},
		{
			name:          "missing settings file",
			saveSettings:  false,
			wantRouter:    ProcessStatus{State: StateStopped},
			wantWorker:    ProcessStatus{State: StateStopped},
			wantRouterURL: "http://127.0.0.1:8080",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestSupervisor(t)
			if tc.saveSettings {
				fx.save(t) // Start*/Status read config from disk; save before setup
			}
			if tc.setup != nil {
				tc.setup(t, fx)
			}
			if tc.routerProbe != nil {
				fx.s.routerProbe = tc.routerProbe
			}
			if tc.workerProbe != nil {
				fx.s.workerProbe = tc.workerProbe
			}

			status, err := fx.s.Status()
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			assertStatusMatches(t, "Router", status.Router, tc.wantRouter)
			assertStatusMatches(t, "Worker", status.Worker, tc.wantWorker)
			if tc.wantRouterURL != "" && status.RouterURL != tc.wantRouterURL {
				t.Errorf("RouterURL = %q, want %q", status.RouterURL, tc.wantRouterURL)
			}
		})
	}
}

// TestSupervisorStopAll asserts StopAll tears down both children and leaves
// Status fully stopped.
func TestSupervisorStopAll(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	if err := fx.s.StartWorker(); err != nil {
		t.Fatalf("StartWorker: %v", err)
	}
	if fx.builder.count() != 2 {
		t.Fatalf("cmdBuilder invocations = %d, want 2", fx.builder.count())
	}
	fx.builder.at(0).finish(nil)
	fx.builder.at(1).finish(nil)

	fx.s.StopAll()

	if fx.s.IsRouterRunning() {
		t.Error("IsRouterRunning() = true after StopAll, want false")
	}
	if fx.s.IsWorkerRunning() {
		t.Error("IsWorkerRunning() = true after StopAll, want false")
	}
	status, err := fx.s.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Router.State != StateStopped {
		t.Errorf("Status().Router.State = %q, want %q", status.Router.State, StateStopped)
	}
	if status.Worker.State != StateStopped {
		t.Errorf("Status().Worker.State = %q, want %q", status.Worker.State, StateStopped)
	}
}

// TestSupervisorLogCapture asserts child stdout and stderr are both captured
// into the per-role log file.
func TestSupervisorLogCapture(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	fake := fx.builder.last()

	const stdoutLine = "router listening on :8080\n"
	const stderrLine = "router warning: slow disk\n"
	if _, err := io.WriteString(fake.Stdout(), stdoutLine); err != nil {
		t.Fatalf("write captured stdout: %v", err)
	}
	if _, err := io.WriteString(fake.Stderr(), stderrLine); err != nil {
		t.Fatalf("write captured stderr: %v", err)
	}

	fake.finish(nil)
	// StopRouter waits on the waitExit goroutine, which closes the log file,
	// so the read below is deterministic.
	if err := fx.s.StopRouter(); err != nil {
		t.Fatalf("StopRouter: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(fx.logsDir, "router.log"))
	if err != nil {
		t.Fatalf("read router.log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, stdoutLine) {
		t.Errorf("router.log missing stdout line %q; got:\n%s", stdoutLine, content)
	}
	if !strings.Contains(content, stderrLine) {
		t.Errorf("router.log missing stderr line %q; got:\n%s", stderrLine, content)
	}
}

// TestManagedProcessRealAliveProbe locks the execCmd adapter's liveness probe
// against the real OS: isAlive() and status().Running must both be true for a
// genuinely running child and false after stop. The probe is
// Signal(syscall.Signal(0)) — a null-signal existence check. Signal(nil)
// returns "os: unsupported signal type" on POSIX for running AND finished
// processes, so it can never be used as a liveness probe.
func TestManagedProcessRealAliveProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execCmd liveness probe is POSIX-only; skipping on windows")
	}
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("/bin/sleep not found; skipping real-subprocess probe")
	}

	logPath := filepath.Join(t.TempDir(), "sleep.log")
	mp := newManagedProcess(logPath, realCmdBuilder)
	t.Cleanup(func() { _ = mp.stop() })

	if err := mp.start("/bin/sleep", []string{"30"}, nil); err != nil {
		t.Fatalf("start /bin/sleep: %v", err)
	}
	if !mp.isAlive() {
		t.Error("isAlive() = false while child running, want true")
	}
	if st := mp.status(); !st.Running {
		t.Errorf("status().Running = false while child running, want true (PID %d)", st.PID)
	}

	if err := mp.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if mp.isAlive() {
		t.Error("isAlive() = true after stop, want false")
	}
	if st := mp.status(); st.Running {
		t.Error("status().Running = true after stop, want false")
	}
}

// TestRouterURLForWorker is a pure unit test for the router-address -> worker
// --router URL normalisation.
func TestRouterURLForWorker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"default addr", ":8080", "http://127.0.0.1:8080"},
		{"empty uses default", "", "http://127.0.0.1:8080"},
		{"absolute URL passes through", "http://x:1", "http://x:1"},
		{"absolute URL trims trailing slash", "http://x:1/", "http://x:1"},
		{"bare host port", "127.0.0.1:9", "http://127.0.0.1:9"},
		{"whitespace trimmed", " :9000 ", "http://127.0.0.1:9000"},
		{"wildcard addr", "0.0.0.0:8080", "http://0.0.0.0:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerURLForWorker(tc.in); got != tc.want {
				t.Errorf("routerURLForWorker(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildProcessStatus is a pure unit test for the processInfo -> UI
// projection mapping (stopped / start-error / exited / running healthy /
// running unhealthy).
func TestBuildProcessStatus(t *testing.T) {
	cases := []struct {
		name       string
		info       processInfo
		startErr   error
		healthy    bool
		registered bool
		want       ProcessStatus
	}{
		{"never started", processInfo{}, nil, false, false, ProcessStatus{State: StateStopped}},
		{"log path preserved when stopped", processInfo{LogPath: "/tmp/x.log"}, nil, false, false, ProcessStatus{State: StateStopped, LogPath: "/tmp/x.log"}},
		{"start failure renders error", processInfo{ExitCode: -1}, errors.New("start boom"), false, false, ProcessStatus{State: StateError, Error: "start boom"}},
		{"exited with code", processInfo{ExitCode: 3}, nil, false, false, ProcessStatus{State: StateError, Error: "process exited with code 3"}},
		{"running healthy", processInfo{Running: true, PID: 42}, nil, true, true, ProcessStatus{State: StateRunning, Running: true, Healthy: true, Registered: true, PID: 42}},
		{"running unhealthy", processInfo{Running: true, PID: 42}, nil, false, false, ProcessStatus{State: StateRunning, Running: true, Healthy: false, Error: "health check failed", PID: 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProcessStatus(tc.info, tc.startErr, tc.healthy, tc.registered)
			if got.State != tc.want.State {
				t.Errorf("State = %q, want %q", got.State, tc.want.State)
			}
			if got.Error != tc.want.Error {
				t.Errorf("Error = %q, want %q", got.Error, tc.want.Error)
			}
			if got.Running != tc.want.Running {
				t.Errorf("Running = %v, want %v", got.Running, tc.want.Running)
			}
			if got.Healthy != tc.want.Healthy {
				t.Errorf("Healthy = %v, want %v", got.Healthy, tc.want.Healthy)
			}
			if got.Registered != tc.want.Registered {
				t.Errorf("Registered = %v, want %v", got.Registered, tc.want.Registered)
			}
			if got.PID != tc.want.PID {
				t.Errorf("PID = %d, want %d", got.PID, tc.want.PID)
			}
			if got.LogPath != tc.want.LogPath {
				t.Errorf("LogPath = %q, want %q", got.LogPath, tc.want.LogPath)
			}
		})
	}
}
