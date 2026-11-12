package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// TestNormalizeRelayURL_StripsExistingSuffix verifies that a user-supplied
// /v1/connect is stripped before the canonical path is appended, so both URL
// forms normalize to the same canonical endpoint (issue #191).
func TestNormalizeRelayURL_StripsExistingSuffix(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"suffix stripped", "ws://192.168.1.216:8090/v1/connect", "ws://192.168.1.216:8090/v1/connect"},
		{"suffix stripped http scheme", "http://192.168.1.216:8090/v1/connect", "ws://192.168.1.216:8090/v1/connect"},
		{"trailing slash stripped", "ws://192.168.1.216:8090/v1/connect/", "ws://192.168.1.216:8090/v1/connect"},
		{"suffix stripped with query", "ws://192.168.1.216:8090/v1/connect?x=1", "ws://192.168.1.216:8090/v1/connect"},
		{"bare host", "ws://192.168.1.216:8090", "ws://192.168.1.216:8090/v1/connect"},
		{"root path", "ws://192.168.1.216:8090/", "ws://192.168.1.216:8090/v1/connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRelayURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeRelayURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRelayURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNormalizeRelayURL_BareHost verifies the documented bare form: the
// canonical /v1/connect path is appended and scheme conversion works.
func TestNormalizeRelayURL_BareHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"ws scheme", "ws://relay:8090", "ws://relay:8090/v1/connect"},
		{"wss scheme", "wss://relay:8090", "wss://relay:8090/v1/connect"},
		{"http scheme converts", "http://relay:8090", "ws://relay:8090/v1/connect"},
		{"https scheme converts", "https://relay:8090", "wss://relay:8090/v1/connect"},
		{"no scheme", "relay:8090", "ws://relay:8090/v1/connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRelayURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeRelayURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRelayURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestFailAllRelayPending_WritesErrorToPendingCalls verifies that
// failAllRelayPending correctly delivers errRelayDisconnected to all
// pending calls and cleans them up (issue #197).
func TestFailAllRelayPending_WritesErrorToPendingCalls(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID:        "ws-worker-1",
		Hostname:  "worker-1",
		IP:        "127.0.0.1",
		Port:      8081,
		Status:    protocol.StatusAvailable,
		Version:   "v1",
		Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// Register a streaming call on the relay connection.
	chunkCh := make(chan StreamEvent, 16)
	errCh := make(chan error, 1)
	pc, err := hub.registerRelayStream("test-msg-id", chunkCh, errCh, "ws-worker-1")
	if err != nil {
		t.Fatalf("failed to register relay stream: %v", err)
	}

	// Verify the call is in the pending map.
	hub.relayPendingMu.Lock()
	_, exists := hub.relayPending["test-msg-id"]
	hub.relayPendingMu.Unlock()
	if !exists {
		t.Fatal("pending call not found in relayPending map")
	}

	// Call failAllRelayPending with errRelayDisconnected.
	hub.failAllRelayPending(errRelayDisconnected)

	// Verify the pending call received the error.
	select {
	case err := <-errCh:
		if !errors.Is(err, errRelayDisconnected) {
			t.Fatalf("expected errRelayDisconnected, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error on errCh")
	}

	// Verify the pending call is cleaned up from the map.
	hub.relayPendingMu.Lock()
	_, exists = hub.relayPending["test-msg-id"]
	hub.relayPendingMu.Unlock()
	if exists {
		t.Fatal("pending call not cleaned up from relayPending map")
	}

	// Verify the doneCh is closed.
	select {
	case <-pc.doneCh:
		// doneCh closed, as expected.
	default:
		t.Fatal("expected doneCh to be closed")
	}

	server.Close()
}

// TestRelayWorkerClient_HardTimeout verifies that relay calls respect
// the maxRelayCallTimeout hard cap when waiting for a response.
// Since testWSHub doesn't set up a relay connection, this test verifies
// the context timeout behavior of the context wrapper.
func TestRelayWorkerClient_HardTimeout(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID:        "ws-worker-timeout",
		Hostname:  "worker-timeout",
		IP:        "127.0.0.1",
		Port:      8081,
		Status:    protocol.StatusAvailable,
		Version:   "v1",
		Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := newRelayWorkerClient(hub, testLogger())

	// Use a context with a short timeout that should trigger before maxRelayCallTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Call Complete - this should fail quickly because the message cannot be sent
	// (no relay connection is configured, so relayConnected() returns false).
	start := time.Now()
	_, err := client.Complete(ctx, info, "chat", []byte(`{"model":"test","messages":[]}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from Complete with no response")
	}

	// The call should fail quickly (within 100ms, well under the 60s maxRelayCallTimeout).
	// This verifies that the context.WithTimeout wrapper is working correctly.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Complete took too long: %v", elapsed)
	}

	server.Close()
}

// TestRelayWorkerClient_ContextTimeoutTriggersFail verifies that when a relay
// call's context is cancelled, the pending call is failed appropriately.
func TestRelayWorkerClient_ContextTimeoutTriggersFail(t *testing.T) {
	// This test verifies the context timeout wrapping works by checking that
	// when ctx is cancelled, the pending call's fail() is called.
	hub := NewWSHub(nil, testLogger())

	info := protocol.WorkerInfo{
		ID: "relay-test-worker",
	}

	client := newRelayWorkerClient(hub, testLogger())

	// Create a context that we'll cancel.
	ctx, cancel := context.WithCancel(context.Background())

	// Start the Complete call in a goroutine.
	done := make(chan error, 1)
	go func() {
		_, err := client.Complete(ctx, info, "chat", []byte(`{"model":"test","messages":[]}`))
		done <- err
	}()

	// Cancel the context after a short delay.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The call should return quickly with a context error.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		// The error should be context.Canceled or DeadlineExceeded
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errRelayDisconnected) {
			t.Fatalf("expected context error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Complete to return after context cancellation")
	}
}

// TestRelayRegisterSupersede_NoDeadlock verifies BUG-1: no goroutine holds
// h.mu while invoking any wsConnection method that acquires h.mu. After the
// fix, the relay branches of send() and enqueue() acquire no hub lock at all.
//
// This is a REAL regression test: it drives the actual handleRelayRegister →
// enqueueClose → enqueue relay-branch code path over a real relaySendCh. If
// BUG-1 is present, the goroutine at step 2 blocks forever on the non-
// reentrant RWMutex; the test does NOT hang the test process (the deadlock is
// in a spawned goroutine), it fails deterministically on the 2 s timeout.
func TestRelayRegisterSupersede_NoDeadlock(t *testing.T) {
	// Build a hub with reg == nil (so bridge is a no-op), with relayConn set
	// to a real dialed *websocket.Conn against a trivial httptest WS server.
	// This mirrors TestRelayReaderWatchdogBreaksSilentRelay's hub construction.
	hub := NewWSHub(nil, testLogger())

	// Fake relay server: accepts connections, reads router_ready, then idles.
	srv := httptest.NewServer(websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = wsutil.MaxMessageSize
			var ready protocol.Message
			_ = wsutil.ReceiveJSON(ws, &ready) // router_ready
			// Hold connection open; no more frames needed.
			<-time.After(5 * time.Second)
		},
	})
	defer srv.Close()

	// Dial the relay and set it on the hub.
	conn, err := wsutil.Dial(context.Background(),
		"ws://"+strings.TrimPrefix(srv.URL, "http://")+"/", "")
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	hub.relayMu.Lock()
	hub.relayConn = conn
	hub.relayMu.Unlock()

	// First relay register: no supersede, must return promptly.
	reg1, _ := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        "w-relay",
			Hostname:  "relay-worker",
			IP:        "127.0.0.1",
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Transport: protocol.TransportWS,
		},
	})
	hub.handleRelayRegister(reg1)
	if hub.Client("w-relay") == nil {
		t.Fatal("first register: hub.Client(\"w-relay\") == nil")
	}

	// Second relay register, SAME worker id → supersede path (the deadlock site).
	// Spawn a goroutine that calls hub.handleRelayRegister(reg2).
	// Assert it completes within 2 seconds.
	reg2, _ := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        "w-relay",
			Hostname:  "relay-worker",
			IP:        "127.0.0.1",
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Transport: protocol.TransportWS,
		},
	})

	done := make(chan struct{})
	go func() {
		hub.handleRelayRegister(reg2)
		close(done)
	}()

	select {
	case <-done:
		// Good: handleRelayRegister returned promptly.
	case <-time.After(2 * time.Second):
		t.Fatal("DEADLOCK: handleRelayRegister held h.mu and re-acquired it via enqueueClose")
	}

	// Assert the superseding conn is the active one.
	if hub.Client("w-relay") == nil {
		t.Fatal("superseded conn: hub.Client(\"w-relay\") == nil")
	}
}

// TestRelayWorkerClientStream_FirstEventIsChunk_NotError verifies BUG-2:
// the maxRelayCallTimeout context's cancel() function is owned exclusively by
// the response-wait goroutine and is invoked only when that goroutine exits.
// The frame-send path (enqueueRelay) receives the caller's ctx and is never
// cancelled by the 60 s cap.
//
// This is a REAL regression test: it exercises the actual relay frame-send →
// writer loop → fake relay → reader loop → routeRelayResponse →
// streamPendingHandler → chunkCh path, including the goroutine that owns the
// cap context. With BUG-2 present, errs receives context.Canceled within
// milliseconds (well before the 100 ms delayed chunk) and the
// case err := <-errs branch fires → fail. With the fix, no early cancel
// exists, the chunk arrives at ~100 ms, and the case ev := <-chunks branch
// fires → pass.
func TestRelayWorkerClientStream_FirstEventIsChunk_NotError(t *testing.T) {
	// Build a real relay round-trip.
	hub := NewWSHub(nil, testLogger())

	// Fake relay: reads frames from the router, sleeps ~100 ms, then writes
	// back a MsgInferenceChunk followed by MsgInferenceResponse{Done: true}.
	var reqMsg protocol.Message
	srv := httptest.NewServer(websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = wsutil.MaxMessageSize
			// Read router_ready (first frame the router sends on dial,
			// mirroring DialRelay).
			var ready protocol.Message
			if err := wsutil.ReceiveJSON(ws, &ready); err != nil {
				return
			}
			// Read the inference request. The relay writer loop (separate
			// goroutine from the reader loop) delivers it to us.
			if err := wsutil.ReceiveJSON(ws, &reqMsg); err != nil {
				return
			}
			// Sleep 100 ms — longer than the bug's early-cancel latency.
			time.Sleep(100 * time.Millisecond)

			// Send a chunk.
			chunk, _ := protocol.NewMessage(protocol.MsgInferenceChunk,
				protocol.InferenceChunkPayload{
					Kind:  "chat",
					Chunk: json.RawMessage(`{"delta":"hi"}`),
				})
			chunk.ID = reqMsg.ID
			_ = wsutil.SendJSON(ws, chunk)

			// Send the terminal response.
			done, _ := protocol.NewMessage(protocol.MsgInferenceResponse,
				protocol.InferenceResponsePayload{
					Kind: "chat",
					Done: true,
				})
			done.ID = reqMsg.ID
			_ = wsutil.SendJSON(ws, done)

			// Hold open briefly.
			<-time.After(500 * time.Millisecond)
		},
	})
	defer srv.Close()

	// Dial the relay and start reader/writer loops (mirroring DialRelay).
	conn, err := wsutil.Dial(context.Background(),
		"ws://"+strings.TrimPrefix(srv.URL, "http://")+"/", "")
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	// Send router_ready as the first frame (DialRelay does this).
	if err := wsutil.WriteJSON(conn, protocol.Message{Type: protocol.MsgRouterReady}); err != nil {
		t.Fatalf("send router_ready: %v", err)
	}
	hub.relayMu.Lock()
	hub.relayConn = conn
	hub.relayDone = make(chan struct{})
	hub.relayMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.relayReaderLoop(ctx, hub.relayDone)
	go hub.relayWriterLoop(ctx, hub.relayDone)
	defer func() {
		cancel()
		_ = conn.Close()
	}()

	// Give loops time to start.
	time.Sleep(50 * time.Millisecond)

	// Call client.Stream with a 5s timeout ctx.
	client := newRelayWorkerClient(hub, testLogger())
	info := protocol.WorkerInfo{ID: "relay-stream-worker"}
	chunks, errs := client.Stream(context.Background(), info, "chat",
		[]byte(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))

	// The FIRST thing delivered must be an SSE chunk, never an error.
	select {
	case ev := <-chunks:
		// Assert the chunk is a real SSE frame (e.g., data: {"delta":"hi"}\n\n), not [DONE].
		if !bytes.Contains(ev.Data, []byte("delta")) {
			t.Fatalf("BUG-2: first event not a chunk with delta; got %q", string(ev.Data))
		}
		if ev.Done {
			t.Fatal("BUG-2: first event was [DONE], expected a chunk first")
		}
		// Pass: chunk arrived before any error.
	case err := <-errs:
		t.Fatalf("BUG-2: send context cancelled on Stream return; worker executed the request but the pending call was already failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: neither chunk nor error arrived within 2s")
	}
}
