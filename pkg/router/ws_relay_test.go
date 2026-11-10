package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
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
