package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stopTimeout bounds the grace period between SIGTERM and SIGKILL. A worker
// that is mid-inference can take a while to unwind; 5s is generous enough to
// let a clean shutdown proceed but short enough that a hung backend is killed
// promptly.
const stopTimeout = 5 * time.Second

// childCmd is the narrow interface the supervisor drives for each child. Both
// *exec.Cmd (via the execCmd adapter) and the test double (fakeCmd) implement
// it, so start/stop/status transitions are unit-testable without the OS.
type childCmd interface {
	Start() error
	Wait() error
	Signal(os.Signal) error
	Kill() error
	Pid() int
	SetEnv([]string)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
	Args() []string
}

// cmdBuilder is the seam behind exec.Command; tests substitute a fake.
type cmdBuilder func(name string, args ...string) childCmd

// execCmd adapts *exec.Cmd to the childCmd interface.
type execCmd struct {
	*exec.Cmd
}

func (e *execCmd) Signal(sig os.Signal) error {
	if e.Cmd.Process != nil {
		return e.Cmd.Process.Signal(sig)
	}
	return errors.New("process not started")
}
func (e *execCmd) Kill() error {
	if e.Cmd.Process != nil {
		return e.Cmd.Process.Kill()
	}
	return errors.New("process not started")
}
func (e *execCmd) Pid() int {
	if e.Cmd.Process != nil {
		return e.Cmd.Process.Pid
	}
	return 0
}
func (e *execCmd) SetEnv(env []string)   { e.Cmd.Env = env }
func (e *execCmd) SetStdout(w io.Writer) { e.Cmd.Stdout = w }
func (e *execCmd) SetStderr(w io.Writer) { e.Cmd.Stderr = w }
func (e *execCmd) Args() []string        { return e.Cmd.Args }

// realCmdBuilder is the production command factory.
func realCmdBuilder(name string, args ...string) childCmd {
	return &execCmd{exec.Command(name, args...)}
}

// managedProcess wraps a childCmd with lifecycle tracking: stdout/stderr
// capture to a single log file, exit-code recording, and graceful stop
// (SIGTERM to the process group, escalate to SIGKILL after stopTimeout).
//
// Every method is safe for concurrent call. The zero value is usable: call
// start() before anything else.
type managedProcess struct {
	mu        sync.Mutex
	cmd       childCmd
	logFile   *os.File
	logPath   string
	pid       int
	started   time.Time
	done      chan struct{}
	exitErr   error
	exitCode  int
	stopped   bool
	cmdBuilder cmdBuilder
}

// newManagedProcess creates a managed process using the supplied command
// factory (defaults to realCmdBuilder; pass a fake in tests).
func newManagedProcess(logPath string, builder cmdBuilder) *managedProcess {
	return &managedProcess{
		logPath:    logPath,
		cmdBuilder: builder,
	}
}

// processInfo is the serialised view returned by managedProcess.status().
type processInfo struct {
	Running   bool  `json:"running"`
	PID       int   `json:"pid"`
	Stopped   bool  `json:"stopped,omitempty"`
	ExitCode  int   `json:"exit_code,omitempty"`
	LogPath   string `json:"log_path"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// start creates the child process, wires stdout/stderr to logPath (create or
// append), and spawns a goroutine that waits for exit. It returns an error if
// the binary is missing, the log file cannot be opened, or cmd.Start fails.
// Calling start on an already-started process returns an error.
func (m *managedProcess) start(binaryPath string, args, env []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		return errors.New("process already started")
	}

	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	logFile, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", m.logPath, err)
	}

	cmd := m.cmdBuilder(binaryPath, args...)
	cmd.SetEnv(env)
	cmd.SetStdout(logFile)
	cmd.SetStderr(logFile)

	// Setpgid + killing the negative PID later lets us tear down the whole
	// subtree (e.g. a worker that spawned a backend helper) with one syscall.
	// The real execCmd honours this via its *exec.Cmd.SysProcAttr; test doubles
	// ignore it.
	if ec, ok := cmd.(*execCmd); ok {
		ec.Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start %s: %w", binaryPath, err)
	}

	m.cmd = cmd
	m.logFile = logFile
	m.pid = cmd.Pid()
	m.started = time.Now()
	m.done = make(chan struct{})
	m.exitCode = -1
	go m.waitExit()
	return nil
}

// waitExit blocks until the child exits, records the exit code, closes the
// done channel, and closes the log file. Runs in its own goroutine.
func (m *managedProcess) waitExit() {
	err := m.cmd.Wait()

	m.mu.Lock()
	m.exitErr = err
	if err != nil {
		m.exitCode = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			m.exitCode = ee.ExitCode()
		}
	} else {
		m.exitCode = 0
	}
	_ = m.logFile.Close()
	close(m.done)
	m.mu.Unlock()
}

// isAlive reports whether the child is still running. A finished or never
// started process reports false.
func (m *managedProcess) isAlive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil {
		return false
	}
	return m.cmd.Signal(nil) == nil
}

// stop sends SIGTERM to the process group, waits up to stopTimeout, then
// escalates to SIGKILL. It is idempotent: calling it on an already-stopped or
// never-started process returns nil.
func (m *managedProcess) stop() error {
	m.mu.Lock()
	if m.cmd == nil || m.done == nil || m.stopped {
		m.mu.Unlock()
		return nil
	}
	pid := m.cmd.Pid()
	done := m.done
	m.stopped = true
	m.mu.Unlock()

	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	select {
	case <-done:
	case <-time.After(stopTimeout):
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		<-done
	}
	return nil
}

// status returns the current snapshot of the process. Safe for concurrent
// call; does not block on the child.
func (m *managedProcess) status() processInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := processInfo{
		ExitCode: m.exitCode,
		LogPath:  m.logPath,
	}
	if m.cmd == nil {
		return info
	}
	info.PID = m.cmd.Pid()
	info.Stopped = m.stopped
	info.StartedAt = m.started
	if m.stopped || m.exitErr != nil {
		info.Running = false
	} else {
		info.Running = m.cmd.Signal(nil) == nil
	}
	return info
}