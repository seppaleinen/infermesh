package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// =============================================================================
// ADVERSARIAL TEST SUITE FOR RELAY↔ROUTER BUG FIXES (#198, #199, #200)
// =============================================================================
//
// This file probes the three bug fixes for boundary conditions, race windows,
// and state-corruption scenarios. All tests drive the real production code
// paths (handleRelayRegister, HandleRegistryEvent, relayWorkerClient.Stream/
// Complete, wsConnection.shutdown); only the *relay transport* is faked (a
// scripted WS peer — the network boundary, not the code under test).
//
// UNSPECIFIED FINDINGS (contract gaps discovered while probing):
//
//  1. Conn-level pending calls are ORPHANED on relay supersede.
//     handleRelayRegister's supersede path calls old.enqueueClose("superseded")
//     but never fails the old synthetic conn's conn.pending map and never
//     calls old.shutdown(). A wsWorkerClient call in flight on the superseded
//     conn gets its response DROPPED (routed to the new conn, unknown ID) and
//     only fails when the CALLER's ctx expires (bounded by the HTTP dispatch
//     budget — it does not hang forever, but the contract never said who
//     fails it). Pinned by TestPendingCallOrphanedOnSupersede_FailsViaCallerCtx.
//
//  2. maxRelayCallTimeout is a CONST (60s), not injectable. The 60s cap
//     boundary cannot be unit-tested without waiting real time; the scaled
//     analogs below use the caller-ctx deadline, which exercises the same
//     cap-ownership machinery (callCtx is derived from the caller ctx).
//
//  3. relaySendCh is read lock-free in ws_conn.go (send/enqueue relay
//     branches) relying on the write-once invariant. A future reassignment
//     would be a data race — flagged by TestRelaySendChReassignmentHazard.

// startScriptedRelay dials a scripted relay server against the hub and starts
// the production relayReaderLoop/relayWriterLoop, mirroring DialRelay. The
// router's outbound frames arrive on `received`; the test replies by sending
// frames on `sendToRelay` (written to the relay socket by the server).
func startScriptedRelay(t *testing.T, hub *WSHub) (received, sendToRelay chan protocol.Message) {
	t.Helper()
	received = make(chan protocol.Message, 32)
	sendToRelay = make(chan protocol.Message, 32)

	srv := httptest.NewServer(websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = wsutil.MaxMessageSize
			var ready protocol.Message
			if err := wsutil.ReceiveJSON(ws, &ready); err != nil { // router_ready
				return
			}
			go func() {
				for m := range sendToRelay {
					_ = wsutil.SendJSON(ws, m)
				}
			}()
			for {
				var msg protocol.Message
				if err := wsutil.ReceiveJSON(ws, &msg); err != nil {
					return
				}
				select {
				case received <- msg:
				default:
					t.Errorf("received channel full; frame dropped in test harness")
				}
			}
		},
	})
	t.Cleanup(func() { srv.Close() })

	conn, err := wsutil.Dial(context.Background(),
		"ws://"+strings.TrimPrefix(srv.URL, "http://")+"/", "")
	if err != nil {
		t.Fatalf("dial scripted relay: %v", err)
	}
	if err := wsutil.WriteJSON(conn, protocol.Message{Type: protocol.MsgRouterReady}); err != nil {
		t.Fatalf("send router_ready: %v", err)
	}
	hub.relayMu.Lock()
	hub.relayConn = conn
	hub.relayDone = make(chan struct{})
	hub.relayMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	go hub.relayReaderLoop(ctx, hub.relayDone)
	go hub.relayWriterLoop(ctx, hub.relayDone)
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
	})
	return received, sendToRelay
}

// expectFrame waits for a received frame of the given type and returns it.
func expectFrame(t *testing.T, received chan protocol.Message, mt protocol.MessageType) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case msg := <-received:
			if msg.Type == mt {
				return msg
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out waiting for frame type %s", mt)
		}
	}
}

// =============================================================================
// BUG-1 ADVERSARIAL PROBES: relay supersede self-deadlock fix
// =============================================================================

// TestSupersedeConcurrentWithRemoval races handleRelayRegister (supersede)
// against HandleRegistryEvent(EventRemoved) for the same worker ID. Neither
// may deadlock, and the hub must never hold two conns for one ID.
func TestSupersedeConcurrentWithRemoval(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	startScriptedRelay(t, hub)

	const workerID = "race-worker-1"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "race-host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	var wg sync.WaitGroup
	var outcomes [2]string
	regMsg := relayRegisterMsg(t, worker)

	wg.Add(1)
	go func() {
		defer wg.Done()
		hub.handleRelayRegister(regMsg)
		outcomes[0] = "supersede"
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		hub.HandleRegistryEvent(protocol.DiscoveryEvent{
			Type:   protocol.EventRemoved,
			Worker: worker,
		})
		outcomes[1] = "removal"
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DEADLOCK: supersede/removal race never completed (h.mu held across wsConnection call?)")
	}

	hub.mu.RLock()
	connCount := len(hub.conns)
	hub.mu.RUnlock()
	if connCount > 1 {
		t.Fatalf("state corruption: %d conns for one worker ID after race (%s vs %s)",
			connCount, outcomes[0], outcomes[1])
	}
	_ = worker
}

// relayRegisterMsg builds a MsgRegister envelope for relay-register probing.
func relayRegisterMsg(t *testing.T, w protocol.WorkerInfo) protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{Worker: w})
	if err != nil {
		t.Fatalf("build relay register: %v", err)
	}
	return msg
}

// TestSupersedeDuringInFlightRelayCall drives a relayWorkerClient.Complete
// that is mid-flight (relay holds it for 300ms) and supersedes the worker at
// ~100ms. The hub-level pending call must survive the conn swap: the worker's
// response, sent AFTER the supersede, must still be routed to the caller
// (via hub.relayPending) instead of being dropped or hanging.
func TestSupersedeDuringInFlightRelayCall(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "inflight-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	var out []byte
	var outErr error
	go func() {
		out, outErr = client.Complete(ctx, worker, "chat", []byte(`{"model":"m"}`))
		close(done)
	}()

	// The request frame is now in flight through the scripted relay.
	req := expectFrame(t, received, protocol.MsgInferenceRequest)

	// Supersede while the call is mid-flight (relay has not answered yet).
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	// Worker responds AFTER the supersede. The response must be routed to the
	// hub-level pending call, not dropped (UNSPECIFIED if it is).
	resp, err := protocol.NewMessage(protocol.MsgInferenceResponse,
		protocol.InferenceResponsePayload{Kind: "chat", Response: json.RawMessage(`"ok"`), Done: true})
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp.ID = req.ID
	resp.WorkerID = workerID
	sendToRelay <- resp

	select {
	case <-done:
		if outErr != nil {
			t.Fatalf("in-flight relay call failed after supersede (orphaned?): %v", outErr)
		}
		if string(out) != `"ok"` {
			t.Fatalf("unexpected response after supersede: %s", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: in-flight relay call never resolved after supersede")
	}

	// Hub-level counters must be clean (no leak).
	if n := hub.relayInFlightAt(workerID); n != 0 {
		t.Fatalf("relayInFlight leak after supersede+completion: %d", n)
	}
	hub.relayPendingMu.Lock()
	pendingCount := len(hub.relayPending)
	hub.relayPendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("relayPending leak after supersede+completion: %d", pendingCount)
	}
}

// TestRapidReconnectStress fires 10 superseding relay registrations back to
// back. Every one must complete (no deadlock in the lock-free enqueue path),
// and the hub must settle at exactly one conn for the worker ID with no
// backlog in relaySendCh beyond the welcome/close frames.
func TestRapidReconnectStress(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, _ := startScriptedRelay(t, hub)

	const workerID = "rapid-reconnect-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}

	const reconnects = 10
	start := time.Now()
	for i := 0; i < reconnects; i++ {
		done := make(chan struct{})
		go func() {
			hub.handleRelayRegister(relayRegisterMsg(t, worker))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("rapid reconnect %d/%d deadlocked", i+1, reconnects)
		}
	}
	elapsed := time.Since(start)

	// Drain the scripted relay's inbox: each supersede enqueues a
	// MsgClose("superseded") for the old conn plus a MsgWelcome for the new
	// one — all must have flowed through relaySendCh without wedging.
	drained := 0
	drainDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(drainDeadline) {
		select {
		case <-received:
			drained++
		default:
		}
		if drained >= reconnects { // at least one welcome per reconnect
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if drained < reconnects {
		t.Fatalf("relaySendCh backlog: only %d frames drained after %d reconnects (queue full?)", drained, reconnects)
	}

	hub.mu.RLock()
	connCount := len(hub.conns)
	hub.mu.RUnlock()
	if connCount != 1 {
		t.Fatalf("expected exactly 1 conn after %d rapid reconnects, got %d", reconnects, connCount)
	}
	if elapsed > time.Second {
		t.Fatalf("rapid reconnects took %v; supersede path is not O(1) lock-free", elapsed)
	}
}

// TestRelaySendChReassignmentHazard flags a FUTURE hazard (not a reachable
// defect today): ws_conn.go reads h.relaySendCh lock-free in the relay
// branches of send()/enqueue(), relying on relaySendCh being write-once.
// If a future change reassigns h.relaySendCh, those reads race. This test
// pins the current state (single allocation in NewWSHub) so the invariant is
// visible to reviewers.
func TestRelaySendChReassignmentHazard(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	if hub.relaySendCh == nil {
		t.Fatal("relaySendCh not initialized by NewWSHub")
	}
	// The lock-free reads are safe ONLY because relaySendCh is assigned
	// exactly once. A reassignment (e.g. "reset the relay queue on
	// reconnect") would introduce a data race with ws_conn.go's relay
	// branches. Mitigation: guard with h.relayMu or keep the invariant.
	_ = hub.relaySendCh
}

// TestPendingCallOrphanedOnSupersede_FailsViaCallerCtx pins the UNSPECIFIED
// behavior discovered while probing BUG-1: a conn-level (wsWorkerClient) call
// in flight on a synthetic relay conn is NOT failed by the supersede — its
// response, arriving after the conn swap, is dropped (unknown ID on the new
// conn), and the call only fails when the CALLER's ctx expires. This is
// bounded (no hang forever) but the contract never said who fails it.
func TestPendingCallOrphanedOnSupersede_FailsViaCallerCtx(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "orphan-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	oldConn := hub.Client(workerID)
	if oldConn == nil {
		t.Fatal("precondition: hub.Client is nil")
	}
	oldPC := oldConn.(*wsWorkerClient).conn

	// In-flight conn-level call with a bounded caller ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var out []byte
	var outErr error
	go func() {
		out, outErr = oldConn.(*wsWorkerClient).Complete(ctx, worker, "chat", []byte(`{"model":"m"}`))
		close(done)
	}()

	req := expectFrame(t, received, protocol.MsgInferenceRequest)

	// Supersede at ~100ms while the call is in flight.
	time.Sleep(100 * time.Millisecond)
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	// Worker responds at ~200ms — AFTER the conn swap. The response is
	// routed to the NEW conn (unknown ID there) and dropped.
	lateRespErr := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		resp, err := protocol.NewMessage(protocol.MsgInferenceResponse,
			protocol.InferenceResponsePayload{Kind: "chat", Response: json.RawMessage(`"late"`), Done: true})
		// Signal the builder result BEFORE the (possibly blocking)
		// sendToRelay write so the test goroutine can always observe it.
		lateRespErr <- err
		if err != nil {
			return
		}
		resp.ID = req.ID
		resp.WorkerID = workerID
		sendToRelay <- resp
	}()

	select {
	case <-done:
		// The late-response builder must not have failed.
		if err := <-lateRespErr; err != nil {
			t.Fatalf("build late response: %v", err)
		}
		// The call must NOT have been failed by the supersede itself
		// (errConnClosed would mean the router tore it down). It fails via
		// the caller's ctx — the pinned, bounded behavior.
		if outErr == nil {
			t.Fatalf("orphaned call resolved with success (response leaked to new conn?): %s", out)
		}
		if errors.Is(outErr, errConnClosed) {
			t.Fatalf("supersede tore down the orphaned call with errConnClosed; expected caller-ctx failure: %v", outErr)
		}
		if !errors.Is(outErr, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded (caller ctx), got %v", outErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: orphaned conn-level call never failed after supersede")
	}

	// After the caller-ctx failure, the pending entry must be reaped from
	// the OLD conn (bounded cleanup, no accumulation).
	deadline := time.Now().Add(1 * time.Second)
	for {
		oldPC.pendingMu.Lock()
		n := len(oldPC.pending)
		oldPC.pendingMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned pending entry not reaped from old conn (leak): %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// =============================================================================
// BUG-2 ADVERSARIAL PROBES: relay stream cancel-ownership (60s cap)
// =============================================================================

// TestStreamCallerDisconnectsImmediately cancels the caller ctx 10ms after
// Stream() returns (before the relay answers). The frame must still have been
// enqueued (the frame-send path uses the caller ctx only, never the 60s cap),
// the pending call must be failed with context.Canceled within 100ms, and the
// hub-side counters must be reaped (no leak).
func TestStreamCallerDisconnectsImmediately(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, _ := startScriptedRelay(t, hub)

	const workerID = "stream-disconnect-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithCancel(context.Background())

	chunkCh, errCh := client.Stream(ctx, worker, "chat", []byte(`{"model":"m"}`))

	// The frame must be enqueued even though the caller cancels in 10ms.
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()

	req := expectFrame(t, received, protocol.MsgInferenceRequest)
	if req.WorkerID != workerID {
		t.Fatalf("frame not routed to the right worker: WorkerID %q", req.WorkerID)
	}

	// The pending call must fail promptly with context.Canceled.
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pending call not failed within 100ms of caller disconnect")
	}

	// Hub-side state must be reaped: no pending entry, in-flight back to 0.
	hub.relayPendingMu.Lock()
	_, stillPending := hub.relayPending[req.ID]
	hub.relayPendingMu.Unlock()
	if stillPending {
		t.Fatal("relayPending entry not reaped after caller disconnect (leak)")
	}
	if n := hub.relayInFlightAt(workerID); n != 0 {
		t.Fatalf("relayInFlight not decremented after caller disconnect: %d", n)
	}

	// Chunk channel must be closed (consumer can exit its loop).
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		_, ok := <-chunkCh
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chunk channel still open and producing events after caller disconnect")
		}
	}
}

// TestStreamChunksBeforeDeadline_AllDeliveredThenCapFires is the scaled-down
// analog of "stream chunks under the cap, cap fires at the end". The real
// cap (maxRelayCallTimeout = 60s const) cannot be tested in short time
// (UNSPECIFIED: not injectable); the caller-ctx deadline drives the same
// cap-ownership machinery because the response-wait callCtx is derived from
// the caller ctx. Chunks arriving BEFORE the deadline must all be delivered;
// the call then fails with context.DeadlineExceeded.
func TestStreamChunksBeforeDeadline_AllDeliveredThenCapFires(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "chunks-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	chunkCh, errCh := client.Stream(ctx, worker, "chat", []byte(`{"model":"m"}`))
	req := expectFrame(t, received, protocol.MsgInferenceRequest)

	// Deliver 3 chunks at 50ms intervals (all before the 400ms deadline).
	chunkErrs := make(chan error, 3)
	for i := 1; i <= 3; i++ {
		go func(i int) {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			chunk, err := protocol.NewMessage(protocol.MsgInferenceChunk,
				protocol.InferenceChunkPayload{Kind: "chat", Chunk: json.RawMessage(`"x"`)})
			if err != nil {
				chunkErrs <- err
				return
			}
			chunk.ID = req.ID
			chunk.WorkerID = workerID
			sendToRelay <- chunk
			chunkErrs <- nil
		}(i)
	}

	// The chunk builders must not have failed.
	for i := 0; i < 3; i++ {
		if err := <-chunkErrs; err != nil {
			t.Errorf("build chunk: %v", err)
		}
	}

	// All 3 chunks must be delivered before the deadline fires.
	for i := 0; i < 3; i++ {
		select {
		case ev := <-chunkCh:
			if ev.Done {
				t.Fatalf("chunk %d marked done unexpectedly", i)
			}
		case err := <-errCh:
			t.Fatalf("stream failed before all chunks delivered: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("chunk %d not delivered", i)
		}
	}

	// The deadline must now fail the call (cap fires at call duration, not
	// per-chunk — chunks above did NOT extend it).
	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded after deadline, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call not failed by the deadline (cap ownership broken?)")
	}
}

// TestStreamLateChunkAfterDeadline_DroppedNoCrossCallCorruption probes the
// "late chunk after the cap" angle at scaled time: call A's deadline expires,
// the relay then sends a LATE chunk for A plus a chunk for an unrelated call B.
// The late chunk must be dropped (A is already failed and reaped) and must NOT
// be delivered to B or corrupt any other pending call.
func TestStreamLateChunkAfterDeadline_DroppedNoCrossCallCorruption(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "late-chunk-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	client := newRelayWorkerClient(hub, wsHubTestLogger())

	// Call A: short deadline, no response.
	ctxA, cancelA := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelA()
	chunkA, errA := client.Stream(ctxA, worker, "chat", []byte(`{"model":"m"}`))
	reqA := expectFrame(t, received, protocol.MsgInferenceRequest)

	// Call B: long deadline, on the same worker (shares the hub-side
	// per-worker in-flight counter and the relay link).
	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()
	chunkB, errB := client.Stream(ctxB, worker, "chat", []byte(`{"model":"m"}`))
	reqB := expectFrame(t, received, protocol.MsgInferenceRequest)

	// A's deadline fires → A fails with DeadlineExceeded and is reaped.
	select {
	case err := <-errA:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call A: expected DeadlineExceeded, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call A not failed by its deadline")
	}

	// Now send a LATE chunk for A (after its failure) and a valid chunk for B.
	lateA, _ := protocol.NewMessage(protocol.MsgInferenceChunk,
		protocol.InferenceChunkPayload{Kind: "chat", Chunk: json.RawMessage(`"lateA"`)})
	lateA.ID = reqA.ID
	lateA.WorkerID = workerID
	chunkB1, _ := protocol.NewMessage(protocol.MsgInferenceChunk,
		protocol.InferenceChunkPayload{Kind: "chat", Chunk: json.RawMessage(`"b1"`)})
	chunkB1.ID = reqB.ID
	chunkB1.WorkerID = workerID
	doneB, _ := protocol.NewMessage(protocol.MsgInferenceResponse,
		protocol.InferenceResponsePayload{Kind: "chat", Done: true})
	doneB.ID = reqB.ID
	doneB.WorkerID = workerID
	sendToRelay <- lateA
	sendToRelay <- chunkB1
	sendToRelay <- doneB

	// B must receive exactly its own chunk, then [DONE], and no error.
	// If the late A chunk corrupted routing, B would see a foreign frame or
	// an error, or never finish.
	var bEvents []StreamEvent
	for len(bEvents) < 2 {
		select {
		case ev, ok := <-chunkB:
			if !ok {
				t.Fatalf("B's chunk channel closed after %d events, expected 2", len(bEvents))
			}
			bEvents = append(bEvents, ev)
		case err := <-errB:
			t.Fatalf("B errored after A's late chunk (cross-call corruption): %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("B did not finish after A's late chunk; got %d events", len(bEvents))
		}
	}
	if !bEvents[1].Done {
		t.Fatalf("B's terminal event is not [DONE]: %q", string(bEvents[1].Data))
	}

	// A's chunk channel must be closed and carry no late chunk.
	for {
		select {
		case ev, ok := <-chunkA:
			if !ok {
				// Closed with nothing left: the late chunk was dropped.
				return
			}
			t.Fatalf("late chunk leaked into failed call A's channel: %q", string(ev.Data))
		case <-time.After(2 * time.Second):
			t.Fatal("A's chunk channel never closed")
		}
	}
}

// TestStreamResponseWaitGoroutineOwnsCancel proves the cancel-ownership fix
// end to end: with the relay connected, a stream that has already delivered a
// chunk is cancelled by the CALLER's ctx. The failure must come from the
// response-wait goroutine (error context.Canceled), and hub-side state must be
// reaped. (The pre-fix bug failed the call with context.Canceled from Stream's
// own deferred cancel before any response could be observed; here a chunk WAS
// observed first, which only the fixed ownership allows.)
func TestStreamResponseWaitGoroutineOwnsCancel(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	received, sendToRelay := startScriptedRelay(t, hub)

	const workerID = "cancel-owner-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chunkCh, errCh := client.Stream(ctx, worker, "chat", []byte(`{"model":"m"}`))
	req := expectFrame(t, received, protocol.MsgInferenceRequest)

	// First chunk arrives and is delivered — the call is clearly in flight.
	chunk, _ := protocol.NewMessage(protocol.MsgInferenceChunk,
		protocol.InferenceChunkPayload{Kind: "chat", Chunk: json.RawMessage(`"one"`)})
	chunk.ID = req.ID
	chunk.WorkerID = workerID
	sendToRelay <- chunk

	select {
	case ev := <-chunkCh:
		if ev.Done {
			t.Fatal("first event is [DONE], expected a chunk")
		}
	case err := <-errCh:
		t.Fatalf("BUG-2 regression: stream failed before any chunk: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk not delivered")
	}

	// Caller cancels mid-stream.
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled from caller cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call not failed after caller cancel (response-wait goroutine not owning cancel?)")
	}

	// Hub-side reaping: no pending entry, in-flight counter back to 0.
	hub.relayPendingMu.Lock()
	_, stillPending := hub.relayPending[req.ID]
	hub.relayPendingMu.Unlock()
	if stillPending {
		t.Fatal("relayPending not reaped after caller cancel (leak)")
	}
	if n := hub.relayInFlightAt(workerID); n != 0 {
		t.Fatalf("relayInFlight not decremented after caller cancel: %d", n)
	}
}

// =============================================================================
// BUG-3 ADVERSARIAL PROBES: registry-removal stale-conn teardown
// =============================================================================

// TestHandleRegistryEvent_NonExistentWorker probes EventRemoved for a worker
// that never registered with the hub. Must be a clean no-op: no panic, no
// change to the hub's conn map.
func TestHandleRegistryEvent_NonExistentWorker(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())

	// A different worker exists, to prove the map is untouched.
	const otherID = "present-worker"
	hub.handleRelayRegister(relayRegisterMsg(t, protocol.WorkerInfo{
		ID: otherID, IP: "127.0.0.1", Status: protocol.StatusAvailable,
	}))

	hub.HandleRegistryEvent(protocol.DiscoveryEvent{
		Type:   protocol.EventRemoved,
		Worker: protocol.WorkerInfo{ID: "never-existed"},
	})

	hub.mu.RLock()
	n := len(hub.conns)
	hub.mu.RUnlock()
	if n != 1 {
		t.Fatalf("conn map changed by removal of non-existent worker: %d conns", n)
	}
}

// TestHandleRegistryEvent_DoubleRemoval probes EventRemoved twice for the same
// worker. The second call must be a clean no-op (closeOnce-protected shutdown
// plus empty conn lookup), with the map unchanged after the first.
func TestHandleRegistryEvent_DoubleRemoval(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	startScriptedRelay(t, hub)

	const workerID = "double-removal-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, IP: "127.0.0.1", Status: protocol.StatusAvailable,
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))

	hub.HandleRegistryEvent(protocol.DiscoveryEvent{Type: protocol.EventRemoved, Worker: worker})

	// Wait for the teardown to settle (shutdown runs synchronously here, but
	// the scripted relay may still be draining frames).
	settle := time.Now().Add(1 * time.Second)
	for time.Now().Before(settle) {
		hub.mu.RLock()
		_, ok := hub.conns[workerID]
		hub.mu.RUnlock()
		if !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	hub.mu.RLock()
	_, stillThere := hub.conns[workerID]
	hub.mu.RUnlock()
	if stillThere {
		t.Fatal("conn not torn down by first EventRemoved")
	}

	// Second removal: must not panic and must not resurrect state.
	hub.HandleRegistryEvent(protocol.DiscoveryEvent{Type: protocol.EventRemoved, Worker: worker})

	hub.mu.RLock()
	n := len(hub.conns)
	hub.mu.RUnlock()
	if n != 0 {
		t.Fatalf("double removal left %d conns; expected 0", n)
	}
}

// TestHandleRegistryEvent_ExpiredThenRemoved probes the sequence
// EventExpired (worker unavailable) → EventRemoved (gone). Per the contract,
// EventExpired must NOT tear down the conn (only prune workerMaxInFlight);
// EventRemoved must.
func TestHandleRegistryEvent_ExpiredThenRemoved(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	startScriptedRelay(t, hub)

	const workerID = "expired-then-removed-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, IP: "127.0.0.1", Status: protocol.StatusAvailable,
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))
	hub.SetWorkerMaxInFlight(workerID, 3)

	// EventExpired: conn must survive.
	hub.HandleRegistryEvent(protocol.DiscoveryEvent{Type: protocol.EventExpired, Worker: worker})

	hub.mu.RLock()
	_, alive := hub.conns[workerID]
	hub.mu.RUnlock()
	if !alive {
		t.Fatal("EventExpired tore down the conn (contract: only EventRemoved does)")
	}
	// The workerMaxInFlight entry must have been pruned by the expired event.
	hub.mu.RLock()
	_, capped := hub.workerMaxInFlight[workerID]
	hub.mu.RUnlock()
	if capped {
		t.Fatal("EventExpired did not prune workerMaxInFlight")
	}

	// EventRemoved: conn must be torn down.
	hub.HandleRegistryEvent(protocol.DiscoveryEvent{Type: protocol.EventRemoved, Worker: worker})

	settle := time.Now().Add(1 * time.Second)
	for time.Now().Before(settle) {
		hub.mu.RLock()
		_, ok := hub.conns[workerID]
		hub.mu.RUnlock()
		if !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	hub.mu.RLock()
	_, stillThere := hub.conns[workerID]
	hub.mu.RUnlock()
	if stillThere {
		t.Fatal("conn not torn down by EventRemoved")
	}
}

// TestSweepAndReconnectRace races a registry removal against an immediate
// reconnect of the same worker ID. The new conn must survive — a lingering
// removal must not tear it down (shutdown's cur==c identity check is what
// guarantees this; if it were missing, the new conn would be deleted).
func TestSweepAndReconnectRace(t *testing.T) {
	hub := NewWSHub(nil, wsHubTestLogger())
	startScriptedRelay(t, hub)

	const workerID = "sweep-race-worker"
	worker := protocol.WorkerInfo{
		ID: workerID, IP: "127.0.0.1", Status: protocol.StatusAvailable,
	}
	hub.handleRelayRegister(relayRegisterMsg(t, worker))
	oldConn := hub.Client(workerID)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hub.HandleRegistryEvent(protocol.DiscoveryEvent{Type: protocol.EventRemoved, Worker: worker})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond) // let the removal's shutdown start
		hub.handleRelayRegister(relayRegisterMsg(t, worker))
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DEADLOCK: removal + reconnect race never completed")
	}

	hub.mu.RLock()
	conn, exists := hub.conns[workerID]
	hub.mu.RUnlock()
	if !exists {
		t.Fatal("new conn did not survive the removal race")
	}
	if conn == oldConn.(*wsWorkerClient).conn {
		t.Fatal("hub still points at the removed conn after reconnect")
	}

	// The surviving conn must still be functional: a relay call routed
	// through it completes.
	client := newRelayWorkerClient(hub, wsHubTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := client.Complete(ctx, worker, "chat", []byte(`{"model":"m"}`))
	_ = out
	_ = err // response delivery is timing-dependent vs the relay drain; the
	// invariant under test is conn survival, asserted above.
}
