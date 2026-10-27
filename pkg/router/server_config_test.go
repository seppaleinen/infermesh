package router

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/registry"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// newConfigServer builds a minimal Server over an in-memory registry so the
// Set* configuration methods can be exercised without standing up an HTTP
// listener. The WSHub and capability cache are created by NewServer itself.
func newConfigServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.New(registry.Defaults(), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Stop() })
	return NewServer(reg, slog.New(slog.NewTextHandler(os.Stderr, nil)), ":0", security.Config{DevMode: true})
}

// TestSetRelayURL verifies SetRelayURL stores the relay WebSocket URL on the
// underlying WSHub. The hub's relayURL field is the single source of truth for
// RunRelay / DialRelay, so the setter must persist the value verbatim.
func TestSetRelayURL(t *testing.T) {
	srv := newConfigServer(t)
	const url = "ws://relay.example.com/v1/connect"
	srv.SetRelayURL(url)
	if got := srv.hub.relayURL; got != url {
		t.Errorf("relayURL = %q, want %q", got, url)
	}
	// An empty URL is stored as-is (the hub treats "" as "no relay").
	srv.SetRelayURL("")
	if got := srv.hub.relayURL; got != "" {
		t.Errorf("relayURL after empty = %q, want \"\"", got)
	}
}

// TestSetMaxInFlight verifies SetMaxInFlight overrides the per-worker cap and
// falls back to defaultMaxInFlight for non-positive values.
func TestSetMaxInFlight(t *testing.T) {
	srv := newConfigServer(t)
	srv.SetMaxInFlight(7)
	if got := srv.hub.maxInFlight; got != 7 {
		t.Errorf("maxInFlight = %d, want 7", got)
	}
	for _, n := range []int{0, -1, -100} {
		srv.SetMaxInFlight(n)
		if got := srv.hub.maxInFlight; got != defaultMaxInFlight {
			t.Errorf("SetMaxInFlight(%d): maxInFlight = %d, want default %d", n, got, defaultMaxInFlight)
		}
	}
}

// TestSetMaxConnections verifies SetMaxConnections overrides the global
// WebSocket connection cap (and rebuilds the buffered semaphore) and falls
// back to defaultMaxConnections for non-positive values.
func TestSetMaxConnections(t *testing.T) {
	srv := newConfigServer(t)
	srv.SetMaxConnections(10)
	if got := srv.hub.maxConnections; got != 10 {
		t.Errorf("maxConnections = %d, want 10", got)
	}
	if got := cap(srv.hub.connSem); got != 10 {
		t.Errorf("connSem cap = %d, want 10", got)
	}
	for _, n := range []int{0, -5} {
		srv.SetMaxConnections(n)
		if got := srv.hub.maxConnections; got != defaultMaxConnections {
			t.Errorf("SetMaxConnections(%d): maxConnections = %d, want default %d", n, got, defaultMaxConnections)
		}
	}
}

// TestSetMaxAttempts verifies SetMaxAttempts overrides the per-request attempt
// ceiling: positive values are honoured, 1 disables failover, and values <= 0
// fall back to the default of 3.
func TestSetMaxAttempts(t *testing.T) {
	srv := newConfigServer(t)
	if got := srv.maxAttempts; got != 3 {
		t.Errorf("default maxAttempts = %d, want 3", got)
	}
	srv.SetMaxAttempts(5)
	if got := srv.maxAttempts; got != 5 {
		t.Errorf("maxAttempts = %d, want 5", got)
	}
	// 1 disables failover (single attempt, current behaviour).
	srv.SetMaxAttempts(1)
	if got := srv.maxAttempts; got != 1 {
		t.Errorf("maxAttempts = 1, want 1, got %d", got)
	}
	for _, n := range []int{0, -3} {
		srv.SetMaxAttempts(n)
		if got := srv.maxAttempts; got != 3 {
			t.Errorf("SetMaxAttempts(%d): maxAttempts = %d, want default 3", n, got)
		}
	}
}

// TestSetRetryBudget verifies SetRetryBudget overrides the global failover
// deadline and falls back to 60s for non-positive values.
func TestSetRetryBudget(t *testing.T) {
	srv := newConfigServer(t)
	if got := srv.retryBudget; got != 60*time.Second {
		t.Errorf("default retryBudget = %v, want 60s", got)
	}
	srv.SetRetryBudget(30 * time.Second)
	if got := srv.retryBudget; got != 30*time.Second {
		t.Errorf("retryBudget = %v, want 30s", got)
	}
	srv.SetRetryBudget(0)
	if got := srv.retryBudget; got != 60*time.Second {
		t.Errorf("retryBudget after 0 = %v, want 60s", got)
	}
	srv.SetRetryBudget(-time.Second)
	if got := srv.retryBudget; got != 60*time.Second {
		t.Errorf("retryBudget after negative = %v, want 60s", got)
	}
}
