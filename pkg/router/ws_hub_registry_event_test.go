package router

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/registry"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestHandleRegistryEvent_RemovesStaleConn_NoDeadlock verifies BUG-3: when
// HandleRegistryEvent runs for an EventRemoved, the stale wsConnection for
// that worker is fully torn down (doneCh closed, socket closed, in-flight
// calls failed, entry removed from h.conns) — all without any goroutine
// holding h.mu across the shutdown() call.
//
// This is a REAL regression test:
//   - (i) if the fix is absent, HandleRegistryEvent returns harmlessly but
//     hub.Client(id) != nil → the "stale conn is gone" assertion fails.
//   - (ii) if someone implements the naive wrong fix (call shutdown while
//     still holding h.mu), shutdown() re-acquires h.mu and deadlocks → the
//     2 s timeout fires → the "goroutine completed" assertion fails.
//   - (iii) with the correct fix, both pass.
func TestHandleRegistryEvent_RemovesStaleConn_NoDeadlock(t *testing.T) {
	hub := NewWSHub(nil, testLogger())

	const workerID = "stale-relay-worker"

	// Simulate a stale relay wsConnection (ws == nil, open doneCh) by
	// manually inserting it into h.conns.
	stale := &wsConnection{
		hub:     hub,
		id:      workerID,
		ws:      nil, // no real socket — relay handles transport
		worker:  protocol.WorkerInfo{ID: workerID, IP: "127.0.0.1", Transport: protocol.TransportWS},
		sendCh:  make(chan []byte, 256),
		doneCh:  make(chan struct{}),
		pending: make(map[string]*pendingCall),
	}
	hub.mu.Lock()
	hub.conns[workerID] = stale
	hub.mu.Unlock()

	// Precondition: the stale conn is visible.
	if hub.Client(workerID) == nil {
		t.Fatal("precondition: hub.Client(workerID) == nil before event")
	}

	// Spawn a goroutine that calls HandleRegistryEvent(EventRemoved, workerID).
	done := make(chan struct{})
	go func() {
		hub.HandleRegistryEvent(protocol.DiscoveryEvent{
			Type:   protocol.EventRemoved,
			Worker: protocol.WorkerInfo{ID: workerID},
		})
		close(done)
	}()

	// Assert the goroutine completes within 2 seconds.
	select {
	case <-done:
		// Good: HandleRegistryEvent returned without holding h.mu across shutdown.
	case <-time.After(2 * time.Second):
		t.Fatal("DEADLOCK: HandleRegistryEvent held h.mu while calling shutdown(), which re-acquires h.mu")
	}

	// Assert the stale conn is gone from the hub's connection map.
	if hub.Client(workerID) != nil {
		t.Fatal("stale conn not removed: hub.Client(workerID) != nil after EventRemoved")
	}
	hub.mu.RLock()
	_, stillThere := hub.conns[workerID]
	hub.mu.RUnlock()
	if stillThere {
		t.Fatal("stale conn not removed from h.conns map after EventRemoved")
	}

	// Assert the stale conn's doneCh is closed (full teardown).
	select {
	case <-stale.doneCh:
		// doneCh closed, as expected.
	default:
		t.Fatal("stale conn's doneCh not closed after EventRemoved")
	}
}

// TestRegistrySweep_PrunesStaleConn_NoDeadlock verifies BUG-3 wiring
// (end-to-end, sweep-driven): a registry sweep that removes a worker (TTL
// expiry) deterministically results in that worker's stale h.conns entry
// being torn down, with no goroutine holding h.mu across the shutdown()
// call. Bounded by the sweep interval plus event-delivery latency.
//
// This is a REAL regression test: with neither change, the sweep removes the
// worker from the registry but h.conns keeps the stale entry forever → the
// poll loop never breaks → 5 s timeout → fail. With only the BUG-3a
// HandleRegistryEvent fix and no wiring (registryEventLoop), the function is
// never called by the sweep → same failure. With BUG-3a + BUG-3b (wiring),
// the sweep's RegistryRemoved reaches HandleRegistryEvent → shutdown →
// entry pruned → the poll loop breaks well under 5 s.
func TestRegistrySweep_PrunesStaleConn_NoDeadlock(t *testing.T) {
	// Registry with the minimum valid short TTLs (Config.Validate enforces
	// UnavailableTTL >= 1s, RemoveTTL >= UnavailableTTL, CheckInterval >= 500ms).
	regCfg := registry.Defaults()
	regCfg.UnavailableTTL = 1 * time.Second
	regCfg.RemoveTTL = 1 * time.Second
	regCfg.CheckInterval = 500 * time.Millisecond
	reg, err := registry.New(regCfg, testLogger())
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry.Start: %v", err)
	}
	t.Cleanup(func() { _ = reg.Stop() })

	// Hub started over the registry so registryEventLoop runs (BUG-3b wiring).
	hub := NewWSHub(reg, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	server := httptest.NewServer(hub.Handler())
	t.Cleanup(func() { server.Close() })

	// Register a real WS worker, then leave it idle (no heartbeats).
	w := dialFakeWorker(t, server.URL+"/v1/connect")
	info := protocol.WorkerInfo{
		ID:        "sweep-worker",
		Hostname:  "sweep-worker",
		IP:        "127.0.0.1",
		Port:      8081,
		Status:    protocol.StatusAvailable,
		Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// Assert the worker is connected over WebSocket.
	if hub.Client("sweep-worker") == nil {
		t.Fatal("precondition: hub.Client(\"sweep-worker\") == nil after register")
	}

	// The worker goes silent; the registry sweep must remove it (TTL expiry)
	// and the hub must prune the stale h.conns entry. Poll for up to 5 s with
	// a 20 ms sleep (deterministic polling, no arbitrary long sleeps).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Client("sweep-worker") == nil {
			// Success: the sweep removed the worker and pruned h.conns.
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("registry sweep did not prune the stale h.conns entry (BUG-3 wiring missing or HandleRegistryEvent not removing conns)")
}
