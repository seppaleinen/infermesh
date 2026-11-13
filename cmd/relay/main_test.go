package main

// Tests for the NAT reachability fix in cmd/relay. A worker behind NAT advertises
// its private IP (pkg/worker/host.go LocalIP); the relay rewrites it with the
// real client source address so the fleet router can route back to the worker.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

// ipFromRemote unit tests — the pure parsing core of the rewrite.
func TestIpFromRemote(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"ipv4 with port", "172.19.121.23:54321", "172.19.121.23"},
		{"bare ipv4", "127.0.0.1", "127.0.0.1"},
		{"v4-in-v6 mapped with port", "[::ffff:172.19.121.23]:54321", "172.19.121.23"},
		{"bare v4-in-v6 mapped", "::ffff:10.0.0.5", "10.0.0.5"},
		{"ipv6 with port", "[2001:db8::1]:54321", "2001:db8::1"},
		{"bare ipv6", "2001:db8::1", "2001:db8::1"},
		{"empty returns empty", "", ""},
		{"garbage returns empty", "not-an-ip", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ipFromRemote(c.input)
			if got != c.want {
				t.Fatalf("ipFromRemote(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// newTestRelayServer spins up an in-process relay using the same /v1/connect
// handler as startWSServer so handleWorker runs for real. It returns both the
// server and the backing *Relay (the relay is needed to poll router state).
func newTestRelayServer(t *testing.T) (*httptest.Server, *Relay) {
	t.Helper()
	r := &Relay{
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		listenAddr:   "127.0.0.1:0",
		workers:      make(map[string]*websocket.Conn),
		workerQueue:  make([]protocol.Message, 0),
		workerSendCh: make(map[string]chan []byte),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/connect", func(w http.ResponseWriter, req *http.Request) {
		wsutil.Server(r.handleWS).ServeHTTP(w, req)
	})
	return httptest.NewServer(mux), r
}

// wsURL converts an httptest server URL to a ws:// dial target.
func wsURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "http://", "ws://", 1)
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, err := wsutil.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// waitForRouterConnected polls the relay until a router connection is set.
func waitForRouterConnected(t *testing.T, r *Relay) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.routerMu.Lock()
		ok := r.routerConn != nil
		r.routerMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("router never connected to relay")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustMessage builds a protocol envelope for test payloads.
func mustMessage(t *testing.T, mt protocol.MessageType, payload ...interface{}) protocol.Message {
	t.Helper()
	if len(payload) == 0 {
		m, err := protocol.NewMessage(mt, nil)
		if err != nil {
			t.Fatalf("NewMessage(%s): %v", mt, err)
		}
		return m
	}
	m, err := protocol.NewMessage(mt, payload[0])
	if err != nil {
		t.Fatalf("NewMessage(%s): %v", mt, err)
	}
	return m
}

// TestRelayRewritesPrivateIPOnRegister drives handleWorker end-to-end: a router
// connects, then a worker registers advertising its private IP. The forwarded
// register must carry the real client source address (loopback in-test) and
// preserve every other Worker field. This proves the reachability rewrite that
// fixes fleet routing to workers behind NAT.
func TestRelayRewritesPrivateIPOnRegister(t *testing.T) {
	srv, relay := newTestRelayServer(t)

	// Router connects first so the worker's register forwards immediately.
	routerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(routerWS, mustMessage(t, protocol.MsgRouterReady)); err != nil {
		t.Fatalf("failed to send router_ready: %v", err)
	}
	waitForRouterConnected(t, relay)

	// Worker advertises a private RFC1918 IP (the NAT bug scenario).
	regPayload := protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        "worker-1",
			Hostname:  "nat-client",
			IP:        "172.19.121.23", // private IP behind NAT
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Version:   "relay-test-version",
			Transport: "ws",
			Capabilities: protocol.Capabilities{
				Engines: []string{"llama-cpp"},
			},
		},
		Relay: true,
	}
	msg := mustMessage(t, protocol.MsgRegister, regPayload)

	// Worker registers; handleWorker rewrites the IP and forwards to the router.
	workerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(workerWS, msg); err != nil {
		t.Fatalf("failed to send register: %v", err)
	}

	// Router receives the forwarded register envelope.
	_ = routerWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	var fwd protocol.Message
	if err := websocket.JSON.Receive(routerWS, &fwd); err != nil {
		t.Fatalf("router did not receive forwarded register: %v", err)
	}

	if fwd.Type != protocol.MsgRegister {
		t.Fatalf("forwarded message type = %q, want register", fwd.Type)
	}
	if fwd.WorkerID != "worker-1" {
		t.Fatalf("forwarded worker_id = %q, want worker-1", fwd.WorkerID)
	}

	var decoded protocol.RegisterPayload
	if err := fwd.DecodePayload(&decoded); err != nil {
		t.Fatalf("failed to decode forwarded register: %v", err)
	}

	gotIP := decoded.Worker.IP
	wantSource := "127.0.0.1" // loopback client source address (relay's real dialer)
	if gotIP != wantSource {
		t.Fatalf("forwarded worker IP = %q, want routable source %q", gotIP, wantSource)
	}

	// Other fields must be preserved unchanged by the rewrite.
	want := decoded.Worker
	if want.ID != "worker-1" {
		t.Errorf("ID not preserved: %q", want.ID)
	}
	if want.Hostname != "nat-client" {
		t.Errorf("Hostname not preserved: %q", want.Hostname)
	}
	if want.Port != 8081 {
		t.Errorf("Port not preserved: %d", want.Port)
	}
	if want.Version != "relay-test-version" {
		t.Errorf("Version not preserved: %q", want.Version)
	}
	if want.Transport != "ws" {
		t.Errorf("Transport not preserved: %q", want.Transport)
	}
	if want.Status != protocol.StatusAvailable {
		t.Errorf("Status not preserved: %q", want.Status)
	}
	if len(want.Capabilities.Engines) != 1 || want.Capabilities.Engines[0] != "llama-cpp" {
		t.Errorf("Capabilities.Engines not preserved: %+v", want.Capabilities.Engines)
	}
	if !decoded.Relay {
		t.Errorf("Relay flag not preserved: %v", decoded.Relay)
	}
}

// TestRoutableIPSentinel verifies the no-source fallback returns the empty
// sentinel so callers keep the worker's advertised IP unchanged.
func TestRoutableIPSentinel(t *testing.T) {
	if routableIP(nil) != "" {
		t.Fatal("routableIP(nil) should be empty sentinel")
	}
	if ipFromRemote("") != "" {
		t.Fatal("ipFromRemote(\"\") should be empty sentinel")
	}
}

// TestSendToRouter_ClosesConnOnWriteFailure verifies the issue #195/#196 fix:
// when the router stops reading, the relay's single writer goroutine's write
// to routerConn times out and the writer closes the connection so the router's
// reader loop exits and runRelay re-dials — instead of wedging permanently.
//
// With the single-writer architecture, sendToRouter now ENQUEUES (returns nil)
// and the writer loop does the actual write; on write deadline the writer loop
// closes routerConn. The test enqueues a 4 MiB frame to a non-reading router
// and asserts routerConn becomes nil within a timeout.
func TestSendToRouter_ClosesConnOnWriteFailure(t *testing.T) {
	orig := wsutil.WriteTimeout
	wsutil.SetWriteTimeout(100 * time.Millisecond)
	t.Cleanup(func() { wsutil.SetWriteTimeout(orig) })

	srv, relay := newTestRelayServer(t)

	// Router connects and sends router_ready, then stops reading so the
	// relay's outbound write to it blocks until the deadline fires.
	routerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(routerWS, mustMessage(t, protocol.MsgRouterReady)); err != nil {
		t.Fatalf("failed to send router_ready: %v", err)
	}
	waitForRouterConnected(t, relay)

	// Give the writer loop a moment to start draining the queue before we
	// enqueue a blocking frame, so the enqueue happens after the writer is
	// already parked on the socket.
	time.Sleep(50 * time.Millisecond)

	// Send a payload large enough to exceed the kernel send buffer (~64 KiB
	// on macOS) so the write genuinely blocks rather than completing into the
	// buffer. The router never reads it. 4 MiB was verified empirically to
	// block reliably; 2 MiB completes into the buffer.
	big := strings.Repeat("z", 4<<20) // 4 MiB
	msg := protocol.Message{
		Type:     protocol.MsgHeartbeat,
		WorkerID: "worker-1",
		Payload:  jsonRaw(t, map[string]string{"payload": big}),
	}

	// sendToRouter enqueues the frame onto routerSendCh; the single writer
	// goroutine does the actual write and times out. The enqueue itself
	// returns nil (the queue has room).
	if err := relay.sendToRouter(context.Background(), msg); err != nil {
		t.Fatalf("sendToRouter enqueue err = %v, want nil (enqueue succeeds; writer owns the write)", err)
	}

	// routerConn must be nilled (closed) by the writer loop on write
	// failure, so the reader loop exits and runRelay re-dials — this is
	// what breaks the permanent wedge.
	deadline := time.Now().Add(5 * time.Second)
	for {
		relay.routerMu.Lock()
		got := relay.routerConn
		relay.routerMu.Unlock()
		if got == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("routerConn not closed after write failure; link would wedge permanently")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = routerWS.Close()
}

// jsonRaw marshals v into a json.RawMessage.
func jsonRaw(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return json.RawMessage(b)
}

// =============================================================================
// REGRESSION SUITE FOR ISSUE #203: relay↔worker single writer
// =============================================================================
//
// Before the fix, the relay wrote to each worker's socket from two goroutines:
//
//   1. workerReadLoop echoed heartbeats directly via wsutil.WriteJSON(ws, echo)
//   2. routerReadLoop called forwardToWorker/broadcastToWorkers which wrote
//      directly via wsutil.WriteJSON(conn, msg)
//
// Both goroutines called wsutil.WriteJSON on the SAME *websocket.Conn without
// coordination. x/net/websocket is NOT safe for concurrent writes: its Codec
// serializes frames internally via a mutex (wio), but two concurrent calls
// can still interleave at the frame level if the first call blocks in the
// kernel send buffer (w.Write) and the second acquires the lock for its own
// frame. The reader then sees a torn frame and tears down the connection —
// on the fleet this looked like a spontaneous worker disconnect.
//
// The fix mirrors the #196 relay↔router single-writer pattern for the
// relay↔worker side:
//
//   - Per-worker send queue (chan []byte) registered atomically with the socket
//   - One workerWriterLoop per connection drains the queue and calls
//     wsutil.WriteText with the per-frame WriteTimeout deadline
//   - Heartbeat echo and forwardToWorker/broadcastToWorkers ENQUEUE instead
//     of writing directly
//   - On full queue: drop oldest (redundant heartbeat) and log — same pattern
//     as bufferWorkerMessage
//
// Each test below FAILS on the pre-fix baseline (6842f29 or earlier):
//   - TestWorkerConcurrentEchoAndForward_NoCorruption: two goroutines
//     hammering the same worker socket via relay path produced corrupted
//     frames or torn-down connection under -race
//   - TestWorkerHeartbeatEchoKeepsLinkAlive: heartbeat echo still keeps the
//     link alive across the 90s idle deadline (verifies the echo semantics
//     are intact after the enqueue refactor)

// TestWorkerConcurrentEchoAndForward_NoCorruption is the core #203
// regression test. A worker is connected through the relay; the router is
// also connected. The test drives two concurrent write paths to the SAME
// worker socket through the relay:
//
//  1. workerReadLoop receives heartbeats from the worker and echoes them
//  2. routerReadLoop forwards router messages to the worker via forwardToWorker
//
// Both paths now go through enqueueWorkerFrame -> workerWriterLoop. On the
// pre-fix baseline they wrote directly and concurrently.
//
// The bug manifests when the worker reads SLOWLY (e.g., busy processing
// inference): the kernel send buffer fills, writes block, and the
// per-frame write deadline fires. With two concurrent writers (pre-fix),
// the deadline race causes spurious write failures that tear down the
// connection even though the worker is still reading. The single-writer
// fix eliminates this race.
//
// This test uses a SLOW reader (10ms delay between reads) and a tight
// write timeout (100ms) to stress the deadline race. On the fix, the
// single writer handles backpressure cleanly and more frames get through.
// On the pre-fix baseline, the deadline race causes early teardown.
func TestWorkerConcurrentEchoAndForward_NoCorruption(t *testing.T) {
	// Tight write timeout to trigger deadline race on slow reader
	orig := wsutil.WriteTimeout
	wsutil.SetWriteTimeout(100 * time.Millisecond)
	t.Cleanup(func() { wsutil.SetWriteTimeout(orig) })

	srv, relay := newTestRelayServer(t)

	// Router connects and sends router_ready
	routerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(routerWS, mustMessage(t, protocol.MsgRouterReady)); err != nil {
		t.Fatalf("failed to send router_ready: %v", err)
	}
	waitForRouterConnected(t, relay)

	// Worker registers
	workerID := "worker-203-concurrent"
	regPayload := protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        workerID,
			Hostname:  "test-host",
			IP:        "127.0.0.1",
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Version:   "test",
			Transport: "ws",
			Capabilities: protocol.Capabilities{
				Engines: []string{"llama-cpp"},
			},
		},
		Relay: true,
	}
	workerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(workerWS, mustMessage(t, protocol.MsgRegister, regPayload)); err != nil {
		t.Fatalf("failed to send register: %v", err)
	}

	// Give the relay a moment to set up the worker writer loop
	time.Sleep(50 * time.Millisecond)

	// Track frames the worker receives
	received := make(chan protocol.Message, 500)
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			if err := wsutil.SetReadDeadline(workerWS, time.Now().Add(30*time.Second)); err != nil {
				return
			}
			var msg protocol.Message
			if err := wsutil.ReceiveJSON(workerWS, &msg); err != nil {
				// Connection torn down or decode failure — this is the bug
				received <- protocol.Message{Type: protocol.MsgError, WorkerID: "DECODE_FAILURE", Payload: jsonRaw(t, map[string]string{"error": err.Error()})}
				return
			}
			received <- msg
			// SLOW READER: delay between reads so kernel send buffer fills
			// and writes block, triggering the write deadline race.
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Helper: wait for frames
	waitForFrames := func(targetType protocol.MessageType, count int, timeout time.Duration) int {
		deadline := time.Now().Add(timeout)
		got := 0
		for time.Now().Before(deadline) {
			select {
			case msg := <-received:
				if msg.Type == targetType {
					got++
					if got >= count {
						return got
					}
				}
			case <-time.After(50 * time.Millisecond):
			}
		}
		return got
	}

	// ---- Concurrent write phase ----
	// Path 1: Worker sends heartbeats -> workerReadLoop echoes them back
	// Path 2: Router forwards inference requests via forwardToWorker

	var wg sync.WaitGroup
	wg.Add(2)

	// Path 1: Heartbeat producer (worker side) - sends heartbeats
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			hb := protocol.Message{Type: protocol.MsgHeartbeat}
			if err := wsutil.SendJSON(workerWS, hb); err != nil {
				received <- protocol.Message{Type: protocol.MsgError, WorkerID: "HB_SEND_FAIL", Payload: jsonRaw(t, map[string]string{"error": err.Error()})}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// Path 2: Router message producer - calls forwardToWorker with large payloads
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// Large payload to fill kernel send buffer
			big := strings.Repeat("x", 1<<20) // 1 MiB
			msg := protocol.Message{
				Type:     protocol.MsgInferenceRequest,
				WorkerID: workerID,
				Payload:  jsonRaw(t, map[string]string{"data": big}),
			}
			relay.forwardToWorker(msg)
			time.Sleep(25 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Wait for frames to arrive (reader is slow, so allow more time)
	hbCount := waitForFrames(protocol.MsgHeartbeat, 15, 10*time.Second)
	fwdCount := waitForFrames(protocol.MsgInferenceRequest, 15, 10*time.Second)

	// Verify NO decode failures occurred (corrupted frames)
	select {
	case msg := <-received:
		if msg.Type == protocol.MsgError {
			t.Fatalf("WORKER CONNECTION TORN DOWN / DECODE FAILURE (this is the #203 bug): %s", string(msg.Payload))
		}
	default:
	}

	t.Logf("received %d heartbeats, %d forwards", hbCount, fwdCount)

	// On the fix, the single writer handles backpressure cleanly and we should
	// receive a meaningful number of frames before the write timeout eventually
	// closes the connection. On pre-fix, the deadline race causes early teardown
	// and we get far fewer frames (or decode failures).
	// We require at least 10 of each type to pass — pre-fix typically gets <5
	// due to the deadline race between the two concurrent writers.
	if hbCount < 10 || fwdCount < 10 {
		t.Fatalf("TOO FEW FRAMES RECEIVED (pre-fix deadline race tears down connection early): heartbeats=%d, forwards=%d", hbCount, fwdCount)
	}

	_ = workerWS.Close()
	_ = routerWS.Close()
}

// TestWorkerHeartbeatEchoKeepsLinkAlive verifies that the heartbeat echo
// semantics are intact after the #203 refactor. The worker's wsReaderLoop
// relies on the relay's echoed heartbeat to reset its 90s read deadline
// across NAT/proxy. If the echo stops, the intermediate proxy drops the
// idle link and the worker disconnects.
//
// This test uses a tightened read deadline (100ms) on the worker side and
// verifies that heartbeat echoes from the relay keep the connection alive
// beyond the deadline — without the echo, the worker's read would time out.
func TestWorkerHeartbeatEchoKeepsLinkAlive(t *testing.T) {
	const tightReadTimeout = 100 * time.Millisecond

	srv, relay := newTestRelayServer(t)

	routerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(routerWS, mustMessage(t, protocol.MsgRouterReady)); err != nil {
		t.Fatalf("failed to send router_ready: %v", err)
	}
	waitForRouterConnected(t, relay)

	workerID := "worker-203-heartbeat"
	regPayload := protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        workerID,
			Hostname:  "test-host",
			IP:        "127.0.0.1",
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Version:   "test",
			Transport: "ws",
			Capabilities: protocol.Capabilities{
				Engines: []string{"llama-cpp"},
			},
		},
		Relay: true,
	}
	workerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(workerWS, mustMessage(t, protocol.MsgRegister, regPayload)); err != nil {
		t.Fatalf("failed to send register: %v", err)
	}

	// Give the relay a moment to set up the worker writer loop
	time.Sleep(50 * time.Millisecond)

	// Worker reads in a loop. With a 100ms read deadline and 30s heartbeat
	// interval, the ONLY way the worker keeps reading is if the relay echoes
	// the heartbeat back (resetting the read deadline each time).
	// We send a few heartbeats and verify the worker can still read after
	// multiple deadline periods would have elapsed without the echo.
	heartbeatInterval := 30 * time.Millisecond

	var readErr error
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for i := 0; i < 10; i++ {
			if err := wsutil.SetReadDeadline(workerWS, time.Now().Add(tightReadTimeout)); err != nil {
				readErr = fmt.Errorf("setreaddeadline: %w", err)
				return
			}
			var msg protocol.Message
			if err := wsutil.ReceiveJSON(workerWS, &msg); err != nil {
				readErr = fmt.Errorf("receive i%d: %w", i, err)
				return
			}
			// We expect heartbeats (echoed) and possibly other frames
			if msg.Type != protocol.MsgHeartbeat {
				// Non-heartbeat frames are fine (e.g., welcome)
				continue
			}
		}
	}()

	// Send heartbeats from the worker at 30ms intervals — enough for the
	// reader to complete its 10 iterations (each read resets the deadline)
	for i := 0; i < 12; i++ {
		hb := protocol.Message{Type: protocol.MsgHeartbeat}
		if err := wsutil.SendJSON(workerWS, hb); err != nil {
			t.Fatalf("send heartbeat %d: %v", i, err)
		}
		time.Sleep(heartbeatInterval)
	}

	// Wait for reader to complete
	select {
	case <-readDone:
		if readErr != nil {
			t.Fatalf("HEARTBEAT ECHO FAILED TO KEEP LINK ALIVE (this is the #203 regression): %v", readErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not complete in time")
	}

	_ = workerWS.Close()
	_ = routerWS.Close()
}
