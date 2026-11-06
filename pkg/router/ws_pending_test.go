package router

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestStreamPendingHandler_NonBlocking proves the streaming pending handler
// never blocks the reader loop when the HTTP consumer is not draining.
//
// The relay↔router wedge (issue #198) is caused by the reader loop blocking
// on pc.chunkCh <- ... when a slow/disconnected client stops consuming SSE
// chunks. A blocked reader loop stops reading from the relay, the relay's
// single writer goroutine can't drain its send queue, worker heartbeats and
// inference responses are dropped, and the router marks the worker
// unavailable. This test pins the invariant: the handler must return
// immediately even when chunkCh is full and doneCh is open.
func TestStreamPendingHandler_NonBlocking(t *testing.T) {
	// Pre-fill chunkCh so the first send would block if the handler were
	// synchronous. doneCh is open (not closed) to prove the handler does
	// not rely on doneCh alone to avoid blocking.
	chunkCh := make(chan StreamEvent, 1)
	chunkCh <- StreamEvent{Data: []byte("data: prior\n\n")} // fill the buffer
	doneCh := make(chan struct{})
	errCh := make(chan error, 1)

	pc := &pendingCall{
		chunkCh: chunkCh,
		errCh:   errCh,
		doneCh:  doneCh,
	}
	pc.handle = streamPendingHandler(pc)

	msg, err := protocol.NewMessage(protocol.MsgInferenceChunk, protocol.InferenceChunkPayload{
		Kind:  "chat",
		Chunk: json.RawMessage(`"hello"`),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		pc.handle(msg)
		close(done)
	}()

	select {
	case <-done:
		// Handler returned without blocking — the invariant holds.
	case <-time.After(2 * time.Second):
		t.Fatal("streamPendingHandler blocked on a full chunkCh; this wedges the relay reader loop")
	}
}

// TestCallPendingHandler_NonBlocking proves the single-response pending
// handler never blocks the reader loop when the HTTP consumer is not
// reading respCh. Unlike the streaming handler (which drops chunks), the
// call handler fails the pending call so the HTTP client gets a fast
// error instead of a hang.
func TestCallPendingHandler_NonBlocking(t *testing.T) {
	respCh := make(chan protocol.Message, 1)
	respCh <- protocol.Message{} // fill the buffer
	doneCh := make(chan struct{})
	errCh := make(chan error, 1)

	pc := &pendingCall{
		respCh: respCh,
		errCh:  errCh,
		doneCh: doneCh,
	}
	pc.handle = callPendingHandler(pc)

	msg, err := protocol.NewMessage(protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind:     "chat",
		Response: json.RawMessage(`"hello"`),
		Done:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		pc.handle(msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callPendingHandler blocked on a full respCh; this wedges the relay reader loop")
	}

	// The call must have been failed (not silently dropped) so the HTTP
	// handler observes an error rather than waiting forever.
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected non-nil error when consumer is not reading")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected error on errCh when consumer is not reading")
	}
}
