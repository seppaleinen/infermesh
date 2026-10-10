package main

// Single-instance activation channel (issue #93).
//
// The flock guard in instance_lock.go stops a second InferMesh desktop
// launch from starting a second app, but that launch used to exit silently.
// This file implements the primary fix: a Unix-domain-socket activation
// channel living next to the lock file, over which the second instance asks
// the running (flock-holding) instance to bring its window to the front.
//
// Protocol — one line in, one line out, per connection:
//
//	client -> server: "activate\n"
//	server -> client: "ok\n" (only after the activate callback ran AND
//	                   reported success — a callback that returns false or
//	                   panics gets the connection closed with no ack)
//
// Anything else (unknown payload, no listener, wedge, timeout) is a client-
// side failure: handleSecondLaunch then falls back to the visible platform
// indication (activation_dialog*.go). Everything here is bounded — the
// client never waits longer than ~1s for an ack.
//
// Safety: only the flock holder ever listens, so unlink-before-bind of a
// stale socket file (left by a crashed predecessor) cannot race another
// live instance off the path.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	activationRequestMsg = "activate"
	activationAckMsg     = "ok"

	activationDialTimeout       = 500 * time.Millisecond
	activationAckTimeout        = 500 * time.Millisecond
	activationServerReadTimeout = time.Second

	// maxActivationLine bounds a single protocol line so a hostile or
	// broken peer cannot stream unbounded data within the read deadline.
	maxActivationLine = 64
)

// activationSocketPath derives the activation socket path from the flock
// path by swapping the extension:
//
//	$XDG_RUNTIME_DIR/infermesh-desktop.lock       -> .../infermesh-desktop.sock
//	$TMPDIR/infermesh-desktop-<uid>.lock          -> .../infermesh-desktop-<uid>.sock
//
// so both single-instance artifacts always live side by side.
func activationSocketPath(lockPath string) string {
	return strings.TrimSuffix(lockPath, ".lock") + ".sock"
}

// secondLaunchOutcome classifies what a second instance did after losing
// the flock race. Logged by main() for diagnostics.
type secondLaunchOutcome int

const (
	// outcomeActivated: the running instance acknowledged and brought its
	// window to the front; nothing was shown by the newcomer.
	outcomeActivated secondLaunchOutcome = iota
	// outcomeFallbackShown: no ack (no listener / wedged / old build), but
	// the visible "already running" indication was shown.
	outcomeFallbackShown
	// outcomeFallbackFailed: no ack AND the visible indication failed;
	// only reachable when nothing can render a dialog.
	outcomeFallbackFailed
)

// String implements fmt.Stringer so diagnostics log readable outcome
// names ("activated") instead of bare integers ("0").
func (o secondLaunchOutcome) String() string {
	switch o {
	case outcomeActivated:
		return "activated"
	case outcomeFallbackShown:
		return "fallback-shown"
	case outcomeFallbackFailed:
		return "fallback-failed"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// handleSecondLaunch is the whole second-instance decision path, extracted
// so it is GUI-free testable. notifyFallback is the visible indication
// (main passes platformNotifyAlreadyRunning; tests pass recording stubs).
//
//  1. Primary: bounded activation handshake over the Unix socket — worst
//     case ~1s (activationDialTimeout + activationAckTimeout).
//  2. Fallback: if the running instance does not acknowledge, show the
//     time-bounded visible indication (its own platform bound applies).
//
// Never blocks beyond ~1s plus the fallback's own bound.
func handleSecondLaunch(socketPath string, notifyFallback func() error) (secondLaunchOutcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), activationDialTimeout+activationAckTimeout)
	defer cancel()

	activateErr := requestActivation(ctx, socketPath)
	if activateErr == nil {
		return outcomeActivated, nil
	}
	if notifyFallback == nil {
		return outcomeFallbackFailed, fmt.Errorf("activation via %s failed (%w) and no fallback is configured", socketPath, activateErr)
	}
	if fallbackErr := notifyFallback(); fallbackErr != nil {
		return outcomeFallbackFailed, fmt.Errorf("activation via %s failed (%w); fallback failed: %w", socketPath, activateErr, fallbackErr)
	}
	// The running instance could not be signalled but the user saw why:
	// that is a handled outcome, not an error.
	return outcomeFallbackShown, nil
}

// requestActivation does the bounded client handshake. Returns nil ONLY on
// a valid "ok" ack. Any dial/write/timeout/non-ack => non-nil error.
func requestActivation(ctx context.Context, socketPath string) error {
	dialer := net.Dialer{Timeout: activationDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("activation: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Bound the whole write+ack exchange even if the peer accepts the
	// connection but never replies (a wedged first instance): never wait
	// longer than the handshake budget, or the caller's (usually tighter)
	// context deadline.
	deadline := time.Now().Add(activationDialTimeout + activationAckTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte(activationRequestMsg + "\n")); err != nil {
		return fmt.Errorf("activation: write to %s: %w", socketPath, err)
	}
	reply, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("activation: no ack from %s: %w", socketPath, err)
	}
	if strings.TrimSpace(reply) != activationAckMsg {
		return fmt.Errorf("activation: unexpected reply %q from %s", reply, socketPath)
	}
	return nil
}

// activationServer serves the activation channel for the process lifetime
// of the flock holder.
type activationServer struct {
	ln        net.Listener
	path      string
	closeOnce sync.Once
}

// startActivationListener binds socketPath, unlinks a stale socket file
// first (the CALLER must hold the flock — only the lock holder may
// unlink/own this path; see main.go's gotLock gate), chmods the socket
// 0600, and serves requests for the process lifetime.
//
// Each valid request: run activate() (recover-guarded, synchronous so the
// ack is ordered after the side effect), then write "ok\n" ONLY if
// activate returned true. If activate returns false (activation did not
// actually happen, e.g. the app event loop is not up yet) or panics, the
// connection is closed with no ack — the client must fall back to the
// visible indication rather than be told the window was raised when it
// was not. An unknown payload also gets the connection closed with no
// ack. Per-connection reads are bounded by activationServerReadTimeout.
func startActivationListener(socketPath string, activate func() bool) (*activationServer, error) {
	// Unlink-before-bind: only the flock holder reaches this point, so any
	// existing socket file is stale (crashed predecessor) or ours.
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("activation: listen on %s: %w", socketPath, err)
	}
	// Only the user running the app may talk to the activation channel.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("activation: chmod %s: %w", socketPath, err)
	}

	s := &activationServer{ln: ln, path: socketPath}
	go s.acceptLoop(activate)
	return s, nil
}

// acceptLoop serves connections until the listener is closed. Accept
// errors caused by Close are expected and silent; any other Accept error
// is logged once before the loop gives up.
func (s *activationServer) acceptLoop(activate func() bool) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("activation: accept: %v", err)
			}
			return
		}
		// One goroutine per connection: a slow/absent client must not
		// stall other activation requests.
		go s.serveConn(conn, activate)
	}
}

// serveConn runs the server half of the protocol for a single connection.
func (s *activationServer) serveConn(conn net.Conn, activate func() bool) {
	_ = conn.SetDeadline(time.Now().Add(activationServerReadTimeout))

	req, err := readLine(conn)
	if err != nil {
		_ = conn.Close() // malformed/timed-out request: no ack
		return
	}
	if strings.TrimSpace(req) != activationRequestMsg {
		_ = conn.Close() // unknown payload: close, no ack
		return
	}

	// Synchronous and recover-guarded: the ack is ordered after the side
	// effect, and a panicking activate callback must not take down the
	// listener goroutine. NO ack is sent unless activation genuinely ran
	// (returned true) — otherwise the client falls back to the visible
	// indication instead of being told the window was raised when it
	// was not.
	if !runActivate(activate) {
		_ = conn.Close()
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(activationAckTimeout))
	if _, err := conn.Write([]byte(activationAckMsg + "\n")); err != nil {
		log.Printf("activation: failed to send ack to %s: %v", conn.RemoteAddr(), err)
	}
	_ = conn.Close()
}

// runActivate invokes the activation callback and reports whether
// activation genuinely happened. Returns false when the callback is nil,
// returns false itself (e.g. the app is not ready yet), or panics — the
// recover keeps a misbehaving callback from taking down the listener
// goroutine, and in every false case the server withholds the ack.
func runActivate(activate func() bool) (activated bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("activation: activate callback panicked: %v", r)
			activated = false
		}
	}()
	if activate == nil {
		return false
	}
	return activate()
}

// Close stops the listener and removes the socket file. Nil-safe and
// idempotent: only the first call does anything and it reports that call's
// error; later calls return nil.
func (s *activationServer) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		err = s.ln.Close()
		if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
			err = rmErr
		}
	})
	return err
}

// readLine reads one newline-terminated protocol line, bounded both by the
// caller's connection deadline and by maxActivationLine bytes. Returns an
// error if no complete line arrives.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxActivationLine)).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
