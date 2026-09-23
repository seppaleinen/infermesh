//go:build smoke

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestAppSmoke launches the built desktop app binary and asserts it stays
// alive through a 15s boot window. All GUI probes are advisory (t.Log only);
// process liveness is the pass criterion. The file carries the `smoke` build
// tag so it is excluded from normal `go test ./...` runs.
func TestAppSmoke(t *testing.T) {
	if os.Getenv("SKIP") == "1" || os.Getenv("INFERMESH_SMOKE_SKIP") == "1" {
		t.Skip("smoke test skipped via SKIP/INFERMESH_SMOKE_SKIP")
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("no display on linux; skipping GUI smoke test")
	}

	abs := smokeBinary(t)

	cmd := exec.Command(abs)
	var buf lockedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start app binary: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer tearDown(t, cmd, done)

	waitAlive(t, done, &buf)

	switch runtime.GOOS {
	case "darwin":
		probeDarwin(t, abs, cmd.Process.Pid)
	case "linux":
		probeLinux(t)
	case "windows":
		t.Log("GUI probe not implemented on windows; relying on process liveness")
	}
}

// smokeBinary resolves the app binary path: INFERMESH_APP_BIN if set, else the
// contract default relative to the test working directory (app/). Returns the
// absolute path and fails the test when the file does not exist.
func smokeBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("INFERMESH_APP_BIN")
	if bin == "" {
		bin = "../bin/app/infermesh-app"
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("resolve app binary path %q: %v", bin, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("app binary not found at %s; run 'make build-app' or set INFERMESH_APP_BIN", abs)
	}
	return abs
}

// waitAlive polls the process until the 15s boot window elapses. If the app
// exits first, the test fails with the app's captured output tail.
func waitAlive(t *testing.T, done <-chan error, buf *lockedBuffer) {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("app exited during boot window: %v\n%s", err, lastOutput(buf))
		case <-ticker.C:
		case <-deadline.C:
			t.Log("app stayed alive through 15s boot window")
			return
		}
	}
}

// probeDarwin runs advisory macOS probes: pgrep against the binary path and a
// LaunchServices registration check via lsappinfo. Failures are logged, never
// fatal.
func probeDarwin(t *testing.T, absBin string, pid int) {
	if _, err := exec.LookPath("pgrep"); err == nil {
		out, err := exec.Command("pgrep", "-f", absBin).CombinedOutput()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			t.Log("pgrep: app process found")
		} else {
			t.Log("pgrep: app process not found (advisory)")
		}
	}
	if _, err := exec.LookPath("lsappinfo"); err == nil {
		out, err := exec.Command("lsappinfo", "list").CombinedOutput()
		if err != nil {
			t.Logf("lsappinfo invocation failed: %v (continuing)", err)
			return
		}
		if strings.Contains(string(out), "pid = "+itoa(pid)) {
			t.Log("registered with LaunchServices")
		} else {
			t.Log("not registered (expected for bare binaries), continuing")
		}
	}
}

// probeLinux runs an advisory xdotool window search. When xdotool is absent the
// probe degrades to process-alive only.
func probeLinux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("xdotool"); err != nil {
		t.Log("xdotool not found; degrading to process-alive probe")
		return
	}
	out, err := exec.Command("xdotool", "search", "--name", "InferMesh").CombinedOutput()
	if err != nil {
		t.Logf("xdotool search failed: %v (advisory)", err)
		return
	}
	if len(bytes.TrimSpace(out)) > 0 {
		t.Log("window found")
	} else {
		t.Log("window not found (advisory)")
	}
}

// tearDown terminates the child and waits for it, so the smoke test never
// leaks a process. SIGTERM first on unix (5s grace), then Kill; windows Kills
// directly. If the process already exited (failure path: waitAlive consumed
// the Wait result), return immediately instead of blocking on the drained
// done channel. The final Wait result ("signal: terminated" expected on unix)
// is logged, not asserted.
func tearDown(t *testing.T, cmd *exec.Cmd, done <-chan error) {
	t.Helper()
	if cmd.ProcessState != nil {
		t.Logf("process already exited: %v", cmd.ProcessState)
		return
	}
	if runtime.GOOS == "windows" {
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("kill failed: %v", err)
		}
		select {
		case err := <-done:
			t.Logf("final process result: %v", err)
		case <-time.After(2 * time.Second):
			t.Log("process did not exit within 2s after Kill (continuing)")
		}
		return
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("SIGTERM failed: %v", err)
	}
	select {
	case err := <-done:
		t.Logf("final process result: %v", err)
	case <-time.After(5 * time.Second):
		t.Log("app still running after 5s; killing")
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("kill failed: %v", err)
		}
		select {
		case err := <-done:
			t.Logf("final process result: %v", err)
		case <-time.After(2 * time.Second):
			t.Log("process did not exit within 2s after Kill (continuing)")
		}
	}
}

// lockedBuffer is a goroutine-safe bytes.Buffer used for the child's combined
// stdout+stderr, so the os/exec copy goroutine can Write while the test reads
// (lastOutput) without a data race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lastOutput returns the tail of the combined stdout+stderr buffer so boot
// failures surface the app's own diagnostics.
func lastOutput(buf *lockedBuffer) string {
	out := buf.String()
	const max = 1200
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// itoa formats a non-negative int without importing fmt/strconv, keeping the
// smoke test's import list minimal (stdlib only).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
