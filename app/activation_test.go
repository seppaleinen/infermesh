package main

// GUI-free, headless tests for the single-instance activation channel
// (issue #93). They use real Unix sockets under t.TempDir() only and never
// invoke platformNotifyAlreadyRunning — fallbacks are recording stubs.

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSocketPath returns a path for a real Unix socket in a uniquely
// named, auto-removed temp directory.
//
// It exists because t.TempDir() is unusable for socket paths on macOS/BSD:
// their sockaddr_un.sun_path is capped at 104 bytes and t.TempDir() is
// already ~110 bytes there (…/T/TestActivationListenerConcurrentActivations<rand>/001),
// so bind() fails with EINVAL before any protocol runs. The helper keeps
// the t.TempDir() contract in spirit — unique per test, under the OS temp
// dir, cleaned up automatically — while staying well under the limit.
func testSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "imact-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func TestActivationSocketPath(t *testing.T) {
	t.Run("uses XDG_RUNTIME_DIR without uid suffix", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		want := filepath.Join(dir, "infermesh-desktop.sock")
		if got := activationSocketPath(lockFilePath()); got != want {
			t.Errorf("activationSocketPath(lockFilePath()) = %q, want %q", got, want)
		}
	})

	t.Run("empty XDG_RUNTIME_DIR falls back to temp dir with uid", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := activationSocketPath(lockFilePath())
		wantPrefix := filepath.Join(os.TempDir(), "infermesh-desktop-")
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("activationSocketPath(lockFilePath()) = %q, want prefix %q", got, wantPrefix)
		}
		uid := strconv.Itoa(os.Getuid())
		wantSuffix := "-" + uid + ".sock"
		if len(got) < len(wantSuffix) || got[len(got)-len(wantSuffix):] != wantSuffix {
			t.Errorf("activationSocketPath(lockFilePath()) = %q, want suffix %q", got, wantSuffix)
		}
	})

	t.Run("derives from lock path by extension swap", func(t *testing.T) {
		got := activationSocketPath("/x/y/infermesh-desktop.lock")
		want := "/x/y/infermesh-desktop.sock"
		if got != want {
			t.Errorf("activationSocketPath(%q) = %q, want %q", "/x/y/infermesh-desktop.lock", got, want)
		}
	})
}

func TestHandleSecondLaunchActivatesRunningInstance(t *testing.T) {
	path := testSocketPath(t, "act.sock")
	var activations atomic.Int32
	srv, err := startActivationListener(path, func() bool { activations.Add(1); return true })
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	var fallbackCalls atomic.Int32
	outcome, err := handleSecondLaunch(path, func() error {
		fallbackCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("handleSecondLaunch error = %v, want nil", err)
	}
	if outcome != outcomeActivated {
		t.Errorf("outcome = %d, want %d (outcomeActivated)", outcome, outcomeActivated)
	}
	if got := activations.Load(); got != 1 {
		t.Errorf("activate called %d times, want 1", got)
	}
	if got := fallbackCalls.Load(); got != 0 {
		t.Errorf("fallback called %d times, want 0", got)
	}
}

func TestHandleSecondLaunchNoListenerShowsFallback(t *testing.T) {
	// Nobody ever binds this path: the dial fails fast.
	path := testSocketPath(t, "nobody.sock")

	var fallbackCalls atomic.Int32
	outcome, err := handleSecondLaunch(path, func() error {
		fallbackCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Errorf("handleSecondLaunch error = %v, want nil (fallback handled it)", err)
	}
	if outcome != outcomeFallbackShown {
		t.Errorf("outcome = %d, want %d (outcomeFallbackShown)", outcome, outcomeFallbackShown)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
}

func TestHandleSecondLaunchWedgedFirstInstanceBoundedNoHang(t *testing.T) {
	path := testSocketPath(t, "wedged.sock")
	// A listener that never accepts: the kernel completes the client's
	// connect into the backlog, but nothing ever reads the request or
	// replies — the wedged/suspended-first-instance shape.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	var fallbackCalls atomic.Int32
	start := time.Now()
	outcome, err := handleSecondLaunch(path, func() error {
		fallbackCalls.Add(1)
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("handleSecondLaunch error = %v, want nil (fallback handled it)", err)
	}
	if outcome != outcomeFallbackShown {
		t.Errorf("outcome = %d, want %d (outcomeFallbackShown)", outcome, outcomeFallbackShown)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("handleSecondLaunch took %v, want < 2s (must never hang)", elapsed)
	}
}

func TestStartActivationListenerReplacesStaleSocket(t *testing.T) {
	path := testSocketPath(t, "stale.sock")
	// A plain (non-socket) file left behind by a crashed predecessor.
	if err := os.WriteFile(path, []byte("stale non-socket content"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	var activations atomic.Int32
	srv, err := startActivationListener(path, func() bool { activations.Add(1); return true })
	if err != nil {
		t.Fatalf("startActivationListener over stale file: %v", err)
	}
	defer srv.Close()

	if err := requestActivation(context.Background(), path); err != nil {
		t.Fatalf("requestActivation after unlink-before-bind: %v", err)
	}
	if got := activations.Load(); got != 1 {
		t.Errorf("activate called %d times, want 1", got)
	}
}

func TestStartActivationListenerRejectsUnknownPayload(t *testing.T) {
	path := testSocketPath(t, "act.sock")
	var activations atomic.Int32
	srv, err := startActivationListener(path, func() bool { activations.Add(1); return true })
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	// Raw client speaks a payload the protocol does not know.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("bogus\n")); err != nil {
		t.Fatalf("write bogus payload: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err == nil {
		t.Errorf("server replied %q to unknown payload, want no ack (EOF)", reply)
	}
	conn.Close()

	if got := activations.Load(); got != 0 {
		t.Errorf("activate called %d times for unknown payload, want 0", got)
	}

	// The client-visible result of a no-ack exchange is an error: a peer
	// that closes without replying must make requestActivation fail rather
	// than hang or return nil.
	dumbPath := testSocketPath(t, "close-without-ack.sock")
	ln, err := net.Listen("unix", dumbPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = bufio.NewReader(c).ReadString('\n') // consume, never ack
		c.Close()
	}()
	if err := requestActivation(context.Background(), dumbPath); err == nil {
		t.Error("requestActivation = nil against a close-without-ack peer, want error")
	}
}

func TestRequestActivationRejectsNonAck(t *testing.T) {
	path := testSocketPath(t, "dumb.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = bufio.NewReader(c).ReadString('\n') // consume the request
		_, _ = c.Write([]byte("nope\n"))           // valid line, wrong content
	}()

	if err := requestActivation(context.Background(), path); err == nil {
		t.Error("requestActivation = nil on non-ack reply, want error")
	}
}

func TestActivationListenerConcurrentActivations(t *testing.T) {
	path := testSocketPath(t, "act.sock")
	var activations atomic.Int32
	srv, err := startActivationListener(path, func() bool {
		activations.Add(1)
		// Widen the window so the two requests genuinely overlap and a
		// serialization bug would surface as a deadlock/timeouts.
		time.Sleep(20 * time.Millisecond)
		return true
	})
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			errs <- requestActivation(context.Background(), path)
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent requestActivation %d: %v", i, err)
		}
	}
	if got := activations.Load(); got != 2 {
		t.Errorf("activate called %d times, want 2", got)
	}

	// Close is nil-safe and idempotent, and removes the socket file.
	if err := srv.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second Close = %v, want nil (idempotent)", err)
	}
	var nilServer *activationServer
	if err := nilServer.Close(); err != nil {
		t.Errorf("nil Close = %v, want nil", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still present after Close (stat err = %v)", err)
	}
}

// TestHandleSecondLaunchFallbackFailure covers the outcomeFallbackFailed
// branch (activation.go:97-102): no ack AND the visible indication failed —
// the "nothing can render a dialog" path main() logs as outcome=2.
func TestHandleSecondLaunchFallbackFailure(t *testing.T) {
	t.Run("fallback returns error", func(t *testing.T) {
		// Nobody binds this path, so the activation handshake fails and
		// the fallback is reached.
		path := testSocketPath(t, "fallback-err.sock")
		sentinel := errors.New("no display available")

		var fallbackCalls atomic.Int32
		outcome, err := handleSecondLaunch(path, func() error {
			fallbackCalls.Add(1)
			return sentinel
		})

		if outcome != outcomeFallbackFailed {
			t.Errorf("outcome = %d, want %d (outcomeFallbackFailed)", outcome, outcomeFallbackFailed)
		}
		if err == nil {
			t.Fatal("handleSecondLaunch error = nil, want non-nil when both activation and fallback fail")
		}
		// The error must wrap the fallback failure (errors.Is-able) and
		// carry the activation failure for diagnostics.
		if !errors.Is(err, sentinel) {
			t.Errorf("error %q does not wrap fallback sentinel %q", err, sentinel)
		}
		if !strings.Contains(err.Error(), "activation:") {
			t.Errorf("error %q does not carry the activation failure context", err)
		}
		if got := fallbackCalls.Load(); got != 1 {
			t.Errorf("fallback called %d times, want 1", got)
		}
	})

	t.Run("nil fallback configured", func(t *testing.T) {
		path := testSocketPath(t, "fallback-nil.sock")
		outcome, err := handleSecondLaunch(path, nil)
		if outcome != outcomeFallbackFailed {
			t.Errorf("outcome = %d, want %d (outcomeFallbackFailed)", outcome, outcomeFallbackFailed)
		}
		if err == nil {
			t.Fatal("handleSecondLaunch error = nil, want non-nil when no fallback is configured")
		}
		if !strings.Contains(err.Error(), "no fallback is configured") {
			t.Errorf("error %q does not mention the missing fallback", err)
		}
	})
}

// TestActivationListenerSocketPermissions pins the 0600 contract
// (activation.go:166-172): only the user running the app may talk to the
// activation channel. Without the explicit chmod the socket would inherit
// the process umask (commonly 0755/0777), letting any local user raise the
// window or spam the channel.
func TestActivationListenerSocketPermissions(t *testing.T) {
	path := testSocketPath(t, "perms.sock")
	srv, err := startActivationListener(path, func() bool { return true })
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions = %04o, want 0600", perm)
	}
}

// TestActivationListenerSurvivesPanickingActivate pins the contract in
// serveConn/runActivate (activation.go:207-238): a panicking activate
// callback must be recovered, must produce NO ack (so the client falls back
// instead of being told the window was raised when it was not), and must
// not kill the listener — a later request is still served.
func TestActivationListenerSurvivesPanickingActivate(t *testing.T) {
	path := testSocketPath(t, "panic.sock")
	var calls atomic.Int32
	srv, err := startActivationListener(path, func() bool {
		if calls.Add(1) == 1 {
			panic("simulated activation panic")
		}
		return true
	})
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	// First request: activate panics -> recover -> connection closed with
	// no ack -> the client must see an error, not success.
	if err := requestActivation(context.Background(), path); err == nil {
		t.Error("requestActivation = nil after activate panic, want error (no ack sent)")
	}
	// The panic must not have taken down the accept loop: a second request
	// is served, activate completes, and the ack flows.
	if err := requestActivation(context.Background(), path); err != nil {
		t.Fatalf("requestActivation after recovered panic: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("activate called %d times, want 2 (panicked once, succeeded once)", got)
	}
}

// TestActivationListenerNoAckWhenActivateReportsFalse pins the readiness-
// gate contract (MAJOR 1 of the issue #93 review): when the activate
// callback reports that activation did NOT actually happen — e.g. the
// first instance's app event loop is not up yet and Show/Focus would
// silently no-op — the server must send no ack. requestActivation then
// errors and handleSecondLaunch falls back to the visible indication, so
// a request landing in the pre-app.Run window can never exit silently.
func TestActivationListenerNoAckWhenActivateReportsFalse(t *testing.T) {
	path := testSocketPath(t, "not-ready.sock")
	var activations atomic.Int32
	srv, err := startActivationListener(path, func() bool {
		activations.Add(1)
		return false // simulates main.go's !appReady gate
	})
	if err != nil {
		t.Fatalf("startActivationListener: %v", err)
	}
	defer srv.Close()

	// Direct client: a false activation must be observable as no ack.
	if err := requestActivation(context.Background(), path); err == nil {
		t.Error("requestActivation = nil when activate returned false, want error (no ack sent)")
	}

	// End to end: the second instance must fall back visibly instead of
	// treating the request as activated.
	var fallbackCalls atomic.Int32
	outcome, err := handleSecondLaunch(path, func() error {
		fallbackCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Errorf("handleSecondLaunch error = %v, want nil (fallback handled it)", err)
	}
	if outcome != outcomeFallbackShown {
		t.Errorf("outcome = %s, want %s", outcome, outcomeFallbackShown)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
	if got := activations.Load(); got != 2 {
		t.Errorf("activate called %d times, want 2 (attempted twice, acked never)", got)
	}
}

// TestActivationListenerNoAckWithNilActivate pins runActivate's nil-callback
// branch (activation.go:262-264). This is a deliberate behavior change from
// the pre-rework code, which treated a nil callback as completed and SENT THE
// ACK: a listener started without a callback must now never ack, so the
// client falls back instead of being told activation happened when there was
// no callback to run it.
func TestActivationListenerNoAckWithNilActivate(t *testing.T) {
	path := testSocketPath(t, "nil-cb.sock")
	srv, err := startActivationListener(path, nil)
	if err != nil {
		t.Fatalf("startActivationListener(nil): %v", err)
	}
	defer srv.Close()

	if err := requestActivation(context.Background(), path); err == nil {
		t.Error("requestActivation = nil with nil activate callback, want error (no ack sent)")
	}
	// The no-ack round must not have damaged the listener: a second
	// request is still served and still unacknowledged (EOF, not a hang —
	// the client's own deadline bounds it).
	if err := requestActivation(context.Background(), path); err == nil {
		t.Error("second requestActivation = nil with nil activate callback, want error (no ack sent)")
	}
}

// TestSecondLaunchOutcomeString pins the diagnostic vocabulary that
// main.go's `outcome=%s` log line relies on (added in the review rework):
// readable names instead of bare integers, including the unknown-value guard.
func TestSecondLaunchOutcomeString(t *testing.T) {
	cases := []struct {
		outcome secondLaunchOutcome
		want    string
	}{
		{outcomeActivated, "activated"},
		{outcomeFallbackShown, "fallback-shown"},
		{outcomeFallbackFailed, "fallback-failed"},
		{secondLaunchOutcome(99), "unknown(99)"},
	}
	for _, tc := range cases {
		if got := tc.outcome.String(); got != tc.want {
			t.Errorf("secondLaunchOutcome(%d).String() = %q, want %q", int(tc.outcome), got, tc.want)
		}
	}
}
