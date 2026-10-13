package main

import (
	"fmt"
	"os"
	"path/filepath"
)

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
		return fmt.Errorf("%w: %s (run make build)", errBinaryNotFound, binaryPath)
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
	// Update the desktop RouterClient's transport so the live poll talks
	// to the router over mTLS in prod mode (issue #103 R1).
	s.updateRouterClientTransport()
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
	s.clearClientCertPaths()
	s.clearCACertDir()
	// Clear the desktop RouterClient's transport so the live poll falls
	// back to plain HTTP (dev mode) once the router is gone (issue #103 R1).
	s.updateRouterClientTransport()
	return nil
}

// StartWorker launches the worker binary from the saved config.
func (s *Supervisor) StartWorker() error {
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.workerStarting {
		s.mu.Unlock()
		return errWorkerAlreadyRunning
	}
	mp := s.worker
	if mp != nil && mp.isAlive() {
		s.mu.Unlock()
		return errWorkerAlreadyRunning
	}
	s.workerStarting = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.workerStarting = false
		s.mu.Unlock()
	}()

	binaryPath := s.binaryPath(settings.WorkerBinaryPath, defaultWorkerBinaryPath)
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("%w: %s (run make build)", errBinaryNotFound, binaryPath)
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

// appLogsDir returns the standard per-user log directory (~/.config/infermesh/logs).
func appLogsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "logs")
	}
	return filepath.Join(home, ".config", "infermesh", "logs")
}

// logPath resolves the per-role log file under logsDir.
func (s *Supervisor) logPath(role string) string {
	return filepath.Join(s.logsDir, role+".log")
}

// ensureLogsDir makes sure the log directory exists.
func (s *Supervisor) ensureLogsDir() error {
	return os.MkdirAll(s.logsDir, 0o755)
}