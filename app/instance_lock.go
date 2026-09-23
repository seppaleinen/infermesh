package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning signals that another process already holds the
// single-instance lock; the caller should exit without treating it as a
// failure.
var ErrAlreadyRunning = errors.New("another InferMesh instance is already running")

// instanceLock holds an exclusive flock on a path for the process lifetime.
// The file itself stays empty — only the kernel lock matters.
type instanceLock struct {
	path string
	file *os.File
}

// acquireInstanceLock takes a non-blocking exclusive flock on path.
//
// flock (rather than a pid-file-exists check) is deliberate: the kernel
// releases the lock automatically when the holding process dies for any
// reason — normal exit, crash, or kill -9 — so a stale lock file left behind
// by a dead instance never blocks a new launch, and no liveness heuristics
// are needed.
//
// EWOULDBLOCK/EAGAIN from another live holder maps to ErrAlreadyRunning
// (errors.Is-matchable); any other I/O error is returned as-is so the caller
// can fail open.
func acquireInstanceLock(path string) (*instanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &instanceLock{path: path, file: file}, nil
}

// release unlocks and closes the lock file. Safe on a nil receiver and
// idempotent on an already-released lock.
func (l *instanceLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	// Unlock before close; either way the descriptor is about to go away.
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

// lockFilePath resolves the single-instance lock file location:
//  1. $XDG_RUNTIME_DIR non-empty → <dir>/infermesh-desktop.lock
//     (the runtime dir is a user-private 0700 directory, so no uid suffix).
//  2. otherwise → $TMPDIR/infermesh-desktop-<uid>.lock (shared /tmp needs the
//     uid suffix to stay per-user).
func lockFilePath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "infermesh-desktop.lock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("infermesh-desktop-%d.lock", os.Getuid()))
}
