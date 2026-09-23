package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockFilePath(t *testing.T) {
	t.Run("uses XDG_RUNTIME_DIR without uid suffix", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		want := filepath.Join(dir, "infermesh-desktop.lock")
		if got := lockFilePath(); got != want {
			t.Errorf("lockFilePath() = %q, want %q", got, want)
		}
	})

	t.Run("empty XDG_RUNTIME_DIR falls back to temp dir with uid", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := lockFilePath()
		wantPrefix := filepath.Join(os.TempDir(), "infermesh-desktop-")
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("lockFilePath() = %q, want prefix %q", got, wantPrefix)
		}
		uid := strconv.Itoa(os.Getuid())
		wantSuffix := "-" + uid + ".lock"
		if len(got) < len(wantSuffix) || got[len(got)-len(wantSuffix):] != wantSuffix {
			t.Errorf("lockFilePath() = %q, want suffix %q", got, wantSuffix)
		}
	})
}

func TestAcquireInstanceLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.release()

	second, err := acquireInstanceLock(path)
	if second != nil {
		t.Error("second acquire returned non-nil lock, want nil")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second acquire error = %v, want ErrAlreadyRunning", err)
	}
}

func TestAcquireInstanceLockStaleFileRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	// Simulate a lock file left behind by a dead process: content present,
	// no flock holder. flock semantics mean acquisition still succeeds.
	if err := os.WriteFile(path, []byte("stale pid content"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	lock, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire over stale file: %v", err)
	}
	defer lock.release()
}

func TestInstanceLockReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestInstanceLockReleaseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := lock.release(); err != nil {
		t.Errorf("second release: %v, want nil", err)
	}
	// nil receiver must also be safe (deferred release in main() when the
	// acquire failed and lock was never set).
	var nilLock *instanceLock
	if err := nilLock.release(); err != nil {
		t.Errorf("nil release: %v, want nil", err)
	}
}
