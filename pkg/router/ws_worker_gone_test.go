package router

// =============================================================================
// REGRESSION SUITE FOR ISSUE #201: relay-mode worker flapping loop
// =============================================================================
//
// The fleet observed relay workers connect, register, die ~13 ms later, and
// reconnect every ~30 s forever. A four-step deterministic loop:
//
//  1. Worker registers via relay; a STALE synthetic conn for the same ID is
//     still in h.conns (the relay never told the router the worker died).
//  2. handleRelayRegister superseded it with old.enqueueClose("superseded").
//  3. The close frame is stamped with the worker ID, and the relay routes
//     outbound frames by worker ID to the CURRENTLY LIVE socket — so the
//     close intended for the dead conn killed the brand-new connection.
//  4. The stale conn persisted until the 120 s registry sweep — slower than
//     the ~30 s reconnect cycle, so step 1 always repeated.
//
// The fix is two parts:
//   (a) relay-mode supersede discards the old synthetic conn SILENTLY —
//       no MsgClose frame goes out the relay path (direct workers keep
//       MsgClose("superseded"));
//   (b) the relay emits a MsgWorkerGone frame (carrying a per-connection
//       nonce) when a worker disconnects, and the router prunes the conn +
//       fails its in-flight calls on receipt.
//
// Each test below FAILS on the pre-fix baseline (d9aefe8 or earlier):
//   - TestRelaySupersedeSendsNoCloseFrame        (part a)
//   - TestWorkerGoneRemovesConnAndFailsPending   (part b; MsgWorkerGone
//     did not exist, so the conn survived the stale-conn window)
//   - TestWorkerGoneNonceMismatchKeepsNewConn    (part b; without the
//     nonce guard the late signal would prune the reconnected conn)
//   - TestDirectSupersedeStillSendsCloseFrame    (non-regression guard:
//     the direct path must keep sending MsgClose("superseded"))

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// flappingWorker builds the WorkerInfo used by the #201 regression tests.
func flappingWorker(workerID string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:       workerID,
		Hostname: "flap-host",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
	}
}

// drainFrames non-blockingly pulls every frame from received until the
// channel is quiet for quiet, returning them in arrival order. It is used
// to assert that a frame did (or did not) make it out the relay path.
func drainFrames(received chan protocol.Message, quiet time.Duration) []protocol.Message {
	var out []protocol.Message
	deadline := time.Now().Add(quiet * 3)
	for time.Now().Before(deadline) {
		select {
		case m := <-received:
			out = append(out, m)
		case <-time.After(quiet):
			return out
		}
	}
	return out
}

// frameTypes renders a frame list as a []string of type names for log
// output.
func frameTypes(frames []protocol.Message) []string {
	out := make([]string, 0, len(frames))
	for _, m := range frames {
		out = append(out, string(m.Type))
	}
	return out
}

// TestRelaySupersedeSendsNoCloseFrame is the core #201 regression test
// (part a). A stale synthetic conn exists for a worker ID; the relay
// forwards a fresh register for the same worker (the worker's socket was
// replaced at the relay). The superseded conn must be discarded silently:
// the new connection survives, the fake worker receives NO close frame,
// and the old conn's pending state is clean.
//
// Pre-fix behavior: handleRelayRegister called old.enqueueClose("superseded"),
// which pushed a MsgClose frame onto relaySendCh. On the fleet the relay
// routed that frame (stamped worker_id) to the NEW socket and killed the
// worker. This test asserts the frame is never enqueued at all.
func TestRelaySupersedeSendsNoCloseFrame(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, _ := startScriptedRelay(t, hub)

	const workerID = "flap-worker-201"
	worker := flappingWorker(workerID)

	// First registration (the "stale" conn as seen from the router).
	hub.handleRelayRegister(relayRegisterMsg(t, worker))
	oldConn := hub.Client(workerID)
	if oldConn == nil {
		t.Fatal("precondition: first relay register did not create a conn")
	}
	expectFrame(t, received, protocol.MsgWelcome) // first welcome

	// Re-register the same worker (relay replaced its socket; the router's
	// h.conns still holds the stale synthetic conn). This is the
	// supersede path. (Do NOT use expectFrame here: it consumes non-matching
	// frames, which would hide the MsgClose on the pre-fix baseline.)
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	// Drain everything the supersede put on the relay path. On the
	// pre-fix baseline this drain contains a MsgClose("superseded") frame
	// (enqueued by old.enqueueClose) plus the new welcome; on the fix it
	// contains only the welcome. Asserting the full drain is unambiguous:
	// a partial drain could hide the close behind the welcome.
	frames := drainFrames(received, 80*time.Millisecond)
	var closeFrames []protocol.Message
	welcomeCount := 0
	for _, m := range frames {
		if m.Type == protocol.MsgClose {
			closeFrames = append(closeFrames, m)
		}
		if m.Type == protocol.MsgWelcome {
			welcomeCount++
		}
	}
	if welcomeCount != 1 {
		t.Fatalf("expected exactly 1 welcome frame after re-register, got %d (drained %d frames: %v)", welcomeCount, len(frames), frameTypes(frames))
	}
	if len(closeFrames) != 0 {
		t.Fatalf("relay-mode supersede enqueued %d MsgClose frame(s) onto the relay path; must discard the old conn silently (issue #201)", len(closeFrames))
	}

	// The hub must hold exactly the NEW conn, not the old one.
	cur, ok := hub.Client(workerID).(*wsWorkerClient)
	if !ok {
		t.Fatal("hub has no conn after re-register")
	}
	if cur.conn == oldConn.(*wsWorkerClient).conn {
		t.Fatal("hub still holds the old (superseded) conn after re-register")
	}

	// The old conn's state must be clean: removed from the map (asserted
	// above), done channel closed, no pending entries left behind.
	select {
	case <-oldConn.(*wsWorkerClient).conn.doneCh:
	default:
		t.Fatal("superseded conn's done channel not closed (not discarded)")
	}
	oldPC := oldConn.(*wsWorkerClient).conn
	oldPC.pendingMu.Lock()
	nPending := len(oldPC.pending)
	oldPC.pendingMu.Unlock()
	if nPending != 0 {
		t.Fatalf("superseded conn still holds %d pending entries after discard", nPending)
	}

	// The new conn must be functional: a relay call routed through it must
	// reach the relay (this is the fleet symptom — inference dispatched
	// mid-flap used to be silently dropped / hung).
	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_, _ = client.Complete(ctx, worker, "chat", []byte(`{"model":"m"}`))
	}()
	req := expectFrame(t, received, protocol.MsgInferenceRequest)
	if req.ID == "" {
		t.Fatal("inference request frame has empty correlation id")
	}
	if req.WorkerID != workerID {
		t.Fatalf("inference request frame WorkerID = %q, want %q", req.WorkerID, workerID)
	}
}

// TestWorkerGoneRemovesConnAndFailsPending is the #201 regression test for
// part (b). When the relay reports a worker_gone, the router must remove
// the synthetic conn from h.conns immediately (not wait for the 120 s
// registry sweep) and fail the worker's in-flight hub-level relay calls.
//
// Pre-fix behavior: MsgWorkerGone did not exist; the relay's
// workerReadLoop cleanup only logged, so the stale conn lived until the
// sweep and in-flight calls hung until the caller ctx / 60 s hard cap.
func TestWorkerGoneRemovesConnAndFailsPending(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "gone-worker-201"
	worker := flappingWorker(workerID)

	// Register with a relay-injected nonce (as the real relay does).
	nonce := "abc123nonce"
	reg, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker:     worker,
		RelayNonce: nonce,
	})
	if err != nil {
		t.Fatalf("build register: %v", err)
	}
	hub.handleRelayRegister(reg)
	expectFrame(t, received, protocol.MsgWelcome)

	hub.mu.RLock()
	conn, ok := hub.conns[workerID]
	hub.mu.RUnlock()
	if !ok {
		t.Fatal("precondition: conn missing after register")
	}
	if conn.relayNonce != nonce {
		t.Fatalf("conn relayNonce = %q, want %q (register nonce not captured)", conn.relayNonce, nonce)
	}

	// An in-flight hub-level relay call (the fleet symptom: a dispatch
	// lands while the worker is about to die).
	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var out []byte
	var outErr error
	go func() {
		out, outErr = client.Complete(ctx, worker, "chat", []byte(`{"model":"m"}`))
		close(done)
	}()
	req := expectFrame(t, received, protocol.MsgInferenceRequest)
	_ = req

	// The relay reports the worker disconnected.
	gone, err := protocol.NewMessage(protocol.MsgWorkerGone, protocol.WorkerGonePayload{
		WorkerID:   workerID,
		RelayNonce: nonce,
	})
	if err != nil {
		t.Fatalf("build worker_gone: %v", err)
	}
	gone.WorkerID = workerID
	sendToRelay <- gone

	// The in-flight call must fast-fail with errConnClosed — not hang
	// until the caller ctx or the 60 s hard cap.
	select {
	case <-done:
		if outErr == nil {
			t.Fatalf("in-flight call resolved successfully after worker_gone: %s", out)
		}
		if !errors.Is(outErr, errConnClosed) {
			t.Fatalf("in-flight call failed with %v, want errConnClosed (fast-fail)", outErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: in-flight relay call not failed promptly by worker_gone")
	}

	// The conn must be removed from h.conns (the stale-conn window that
	// drove the flapping loop is gone).
	deadline := time.Now().Add(1 * time.Second)
	for {
		hub.mu.RLock()
		_, stillThere := hub.conns[workerID]
		hub.mu.RUnlock()
		if !stillThere {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker_gone did not remove the conn from h.conns (stale-conn window remains)")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// No pending state may leak.
	hub.relayPendingMu.Lock()
	pendingCount := len(hub.relayPending)
	hub.relayPendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("relayPending leak after worker_gone: %d", pendingCount)
	}
	if n := hub.relayInFlightAt(workerID); n != 0 {
		t.Fatalf("relayInFlight leak after worker_gone: %d", n)
	}
}

// TestWorkerGoneNonceMismatchKeepsNewConn proves the nonce guard: a
// worker_gone that arrives AFTER a reconnect (same worker ID, fresh
// socket) must be matched to the conn it names. The signal carries the
// CURRENT conn's nonce (the relay holds the nonce it stamped on the
// latest register), so a stale signal for a dead socket must never prune
// the live successor. Without the guard, the late signal would kill the
// reconnected worker — the exact flapping this issue fixes.
//
// Pre-fix behavior: MsgWorkerGone did not exist at all; with a naive
// worker-ID-only prune (an obvious first attempt), this test would fail
// because the new conn would be removed.
func TestWorkerGoneNonceMismatchKeepsNewConn(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "late-gone-worker"
	worker := flappingWorker(workerID)

	// Register #1 with nonce-A.
	regA, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker:     worker,
		RelayNonce: "nonce-A",
	})
	if err != nil {
		t.Fatalf("build register A: %v", err)
	}
	hub.handleRelayRegister(regA)
	expectFrame(t, received, protocol.MsgWelcome)

	// Register #2 (reconnect) with nonce-B — supersedes conn A.
	regB, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker:     worker,
		RelayNonce: "nonce-B",
	})
	if err != nil {
		t.Fatalf("build register B: %v", err)
	}
	hub.handleRelayRegister(regB)
	expectFrame(t, received, protocol.MsgWelcome)

	// The relay emits worker_gone for the NEW socket (nonce-B). The router
	// must match it against the live conn's nonce and discard it — this
	// is the legitimate teardown.
	if hub.Client(workerID) == nil {
		t.Fatal("precondition: conn missing after re-register")
	}
	gone, err := protocol.NewMessage(protocol.MsgWorkerGone, protocol.WorkerGonePayload{
		WorkerID:   workerID,
		RelayNonce: "nonce-B",
	})
	if err != nil {
		t.Fatalf("build worker_gone: %v", err)
	}
	gone.WorkerID = workerID
	sendToRelay <- gone

	deadline := time.Now().Add(1 * time.Second)
	for {
		hub.mu.RLock()
		_, stillThere := hub.conns[workerID]
		hub.mu.RUnlock()
		if !stillThere {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("matching worker_gone did not remove the conn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Re-register once more (nonce-C), then deliver a STALE worker_gone
	// whose nonce does NOT match the live conn (a signal for a dead socket
	// that arrived out of order). The live conn must survive.
	regC, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker:     worker,
		RelayNonce: "nonce-C",
	})
	if err != nil {
		t.Fatalf("build register C: %v", err)
	}
	hub.handleRelayRegister(regC)
	expectFrame(t, received, protocol.MsgWelcome)

	hub.mu.RLock()
	connC, ok := hub.conns[workerID]
	hub.mu.RUnlock()
	if !ok {
		t.Fatal("precondition: conn missing after re-register #3")
	}

	stale, err := protocol.NewMessage(protocol.MsgWorkerGone, protocol.WorkerGonePayload{
		WorkerID:   workerID,
		RelayNonce: "nonce-A", // dead conn's nonce — must NOT prune conn C
	})
	if err != nil {
		t.Fatalf("build stale worker_gone: %v", err)
	}
	stale.WorkerID = workerID
	sendToRelay <- stale

	// Give the reader loop time to process the stale frame.
	time.Sleep(150 * time.Millisecond)
	hub.mu.RLock()
	_, stillThere := hub.conns[workerID]
	hub.mu.RUnlock()
	if !stillThere {
		t.Fatal("stale worker_gone pruned the live conn (nonce guard missing)")
	}
	// The conn must still be the nonce-C one.
	if connC.relayNonce != "nonce-C" {
		t.Fatalf("live conn nonce = %q, want nonce-C", connC.relayNonce)
	}
}

// TestDirectSupersedeStillSendsCloseFrame is the non-regression guard for
// part (a): only the RELAY-mode supersede may go silent. A direct (non-relay)
// worker that re-registers with the same ID must still receive
// MsgClose("superseded") on its old socket — that is the closed, verified
// direct-connection behavior.
func TestDirectSupersedeStillSendsCloseFrame(t *testing.T) {
	_, server := testWSHub(t)

	info := protocol.WorkerInfo{
		ID:        "direct-flap-201",
		Hostname:  "direct-host",
		IP:        "127.0.0.1",
		Port:      8081,
		Status:    protocol.StatusAvailable,
		Version:   "v1",
		Transport: protocol.TransportWS,
	}

	// First direct connection registers.
	w1 := dialFakeWorker(t, server.URL+"/v1/connect")
	w1.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	if m := w1.recv(t, 3*time.Second); m.Type != protocol.MsgWelcome {
		t.Fatalf("expected welcome on first conn, got %s", m.Type)
	}

	// Second direct connection, same ID, same peer → supersede path in
	// handleConn. The old socket must receive MsgClose("superseded").
	w2 := dialFakeWorker(t, server.URL+"/v1/connect")
	w2.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	if m := w2.recv(t, 3*time.Second); m.Type != protocol.MsgWelcome {
		t.Fatalf("expected welcome on second conn, got %s", m.Type)
	}

	// The OLD socket must get the close frame (with the "superseded"
	// reason). This must NOT change when the relay-mode path goes silent.
	closeMsg := w1.recv(t, 3*time.Second)
	if closeMsg.Type != protocol.MsgClose {
		t.Fatalf("direct supersede: old conn expected MsgClose, got %s (direct path regressed)", closeMsg.Type)
	}
	var cp protocol.ClosePayload
	if err := closeMsg.DecodePayload(&cp); err != nil {
		t.Fatalf("decode close payload: %v", err)
	}
	if !strings.Contains(cp.Reason, "superseded") {
		t.Fatalf("direct supersede: close reason = %q, want superseded", cp.Reason)
	}
}
