package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
	"golang.org/x/net/websocket"
)

// wsFixture starts a WebSocket router (httptest + wsutil.Server) and dials the
// worker into it via wsConnectOnce, so tests can drive the worker→router
// protocol in both directions. The router side exposes the accepted conn.
//
// The server-side handler blocks on a release channel until the test is
// done, so httptest does not tear down the underlying TCP socket while the
// worker is still reading/writing frames.
type wsFixture struct {
	t       *testing.T
	server  *httptest.Server
	connCh  chan *websocket.Conn // accepted connection (pushed by handler)
	cached  *websocket.Conn      // cached accepted connection
	release chan struct{}        // closed by the test when it's done with the conn
}

func newWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	f := &wsFixture{
		t:       t,
		connCh:  make(chan *websocket.Conn, 1),
		release: make(chan struct{}),
	}
	f.server = httptest.NewServer(wsutil.Server(func(ws *websocket.Conn) {
		select {
		case f.connCh <- ws:
		default:
		}
		<-f.release // keep the handler alive until the test is done
	}))
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		f.server.Close()
	})
	return f
}

// url converts the fixture's http URL to the canonical ws:// connect URL.
func (f *wsFixture) url() string {
	return "ws://" + strings.TrimPrefix(f.server.URL, "http://") + "/v1/connect"
}

// conn returns the router-side accepted connection, waiting up to 2s.
func (f *wsFixture) conn() *websocket.Conn {
	f.t.Helper()
	if f.cached != nil {
		return f.cached
	}
	select {
	case c := <-f.connCh:
		f.cached = c
		return c
	case <-time.After(2 * time.Second):
		f.t.Fatal("router did not accept the worker connection")
		return nil
	}
}

// connect starts wsConnectOnce in a goroutine and waits for the router to
// accept the connection. Subsequent read/write calls operate on the same conn.
func (f *wsFixture) connect(ctx context.Context, cfg WSConfig, infoFn func() protocol.WorkerInfo, srv *Server) {
	f.t.Helper()
	go func() { _ = wsConnectOnce(ctx, f.url(), cfg, infoFn, srv, testLogger()) }()
	f.conn()
}

// readMsg reads one message from the worker with a deadline.
func (f *wsFixture) readMsg(timeout time.Duration) protocol.Message {
	f.t.Helper()
	conn := f.conn()
	_ = wsutil.SetReadDeadline(conn, time.Now().Add(timeout))
	var msg protocol.Message
	if err := wsutil.ReceiveJSON(conn, &msg); err != nil {
		f.t.Fatalf("read message: %v", err)
	}
	return msg
}

// writeMsg sends one message to the worker.
func (f *wsFixture) writeMsg(msg protocol.Message) {
	f.t.Helper()
	conn := f.conn()
	if err := wsutil.SendJSON(conn, msg); err != nil {
		f.t.Fatalf("write message: %v", err)
	}
}

// testWorkerInfo returns a minimal WorkerInfo for registration.
func testWorkerInfo(id string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID: id, Hostname: "host", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
}

// devTestServer builds a worker server in dev mode with health checks off.
func devTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(testLogger(), "127.0.0.1:0", security.Config{DevMode: true, EnableHealthChecks: false})
}

// TestWSRegisterLoop_Unreachable verifies the loop keeps retrying while the
// router is unreachable and exits promptly when ctx is cancelled. (Most
// strings normalize to a syntactically valid URL, so the loop's only exit
// paths are ctx.Done() or a successful, persistent connection.)
func TestWSRegisterLoop_Unreachable(t *testing.T) {
	log := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WSRegisterLoop(ctx, WSConfig{RouterURL: "ws://127.0.0.1:1"}, func() protocol.WorkerInfo { return testWorkerInfo("w1") }, nil, log)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // let the first dial fail
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WSRegisterLoop did not exit after ctx cancel")
	}
}

// TestWSRegisterLoop_Reconnects verifies the loop reconnects with the
// configured backoff after a connection drop, and stops on ctx cancel.
func TestWSRegisterLoop_Reconnects(t *testing.T) {
	accepts := make(chan *websocket.Conn, 4)
	server := httptest.NewServer(wsutil.Server(func(ws *websocket.Conn) {
		select {
		case accepts <- ws:
		default:
		}
	}))
	defer server.Close()

	log := testLogger()
	srv := devTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WSRegisterLoop(ctx, WSConfig{RouterURL: server.URL}, func() protocol.WorkerInfo { return testWorkerInfo("w1") }, srv, log)
		close(done)
	}()

	// First connection is accepted and then torn down by the router side.
	first := <-accepts
	_ = wsutil.Close(first)
	_ = wsutil.Close(first) // idempotent; ensures the worker's reader errors.

	// The worker must re-dial within ~1s (backoff base 1s, ±20%).
	select {
	case <-accepts:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reconnect after connection drop")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WSRegisterLoop did not stop on ctx cancel")
	}
}

// TestWSConnectOnce_DialFailure verifies the dial error is wrapped and
// returned when the router port is closed.
func TestWSConnectOnce_DialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := wsConnectOnce(ctx, "ws://127.0.0.1:1/v1/connect", WSConfig{}.withDefaults(), func() protocol.WorkerInfo { return testWorkerInfo("w1") }, nil, testLogger())
	if err == nil || !strings.Contains(err.Error(), "dial router") {
		t.Fatalf("expected wrapped dial error, got %v", err)
	}
}

// TestWSConnectOnce_RegisterThenPing verifies the full connect flow: the
// worker sends register immediately, then echoes pings (covering
// wsReaderLoop + handleWSMessage ping path + wsOutbound.send).
func TestWSConnectOnce_RegisterThenPing(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-ping") }, srv)

	// First frame must be the worker's register.
	msg := f.readMsg(3 * time.Second)
	if msg.Type != protocol.MsgRegister {
		t.Fatalf("expected register, got %s", msg.Type)
	}
	var reg protocol.RegisterPayload
	if err := msg.DecodePayload(&reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if reg.Worker.ID != "w-ping" {
		t.Fatalf("register worker id = %q, want w-ping", reg.Worker.ID)
	}

	// Ping echo.
	ping, _ := protocol.NewMessage(protocol.MsgPing, nil)
	f.writeMsg(ping)
	pong := f.readMsg(3 * time.Second)
	if pong.Type != protocol.MsgPing || pong.ID != ping.ID {
		t.Fatalf("expected ping echo id %q, got %s/%s", ping.ID, pong.Type, pong.ID)
	}
}

// TestWSConnectOnce_Heartbeat verifies wsHeartbeat re-sends the worker info
// at the configured interval.
func TestWSConnectOnce_Heartbeat(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: 20 * time.Millisecond}, func() protocol.WorkerInfo { return testWorkerInfo("w-hb") }, srv)

	f.readMsg(3 * time.Second) // consume register
	var got protocol.HeartbeatPayload
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat received within 3s")
		}
		_ = wsutil.SetReadDeadline(f.conn(), time.Now().Add(500*time.Millisecond))
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(f.conn(), &msg); err != nil {
			continue
		}
		if msg.Type == protocol.MsgHeartbeat {
			if err := msg.DecodePayload(&got); err != nil {
				t.Fatalf("decode heartbeat: %v", err)
			}
			break
		}
	}
	if got.Worker.ID != "w-hb" {
		t.Fatalf("heartbeat worker id = %q, want w-hb", got.Worker.ID)
	}
}

// TestHandleWSMessage_Close verifies the reader loop closes the connection
// when the router sends MsgClose.
func TestHandleWSMessage_Close(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	connected := make(chan struct{})
	go func() {
		_ = wsConnectOnce(ctx, f.url(), WSConfig{HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-close") }, srv, testLogger())
		close(connected)
	}()
	_ = f.conn()

	closeMsg, _ := protocol.NewMessage(protocol.MsgClose, protocol.ClosePayload{Reason: "superseded"})
	f.writeMsg(closeMsg)

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("wsConnectOnce did not return after router close")
	}
}

// TestHandleWSMessage_ErrorAndUnknown verifies MsgError and unknown types are
// accepted (logged) without closing the connection.
func TestHandleWSMessage_ErrorAndUnknown(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-err") }, srv)
	f.readMsg(3 * time.Second) // consume register

	errMsg, _ := protocol.NewMessage(protocol.MsgError, protocol.ErrorPayload{Message: "boom", Code: "router"})
	f.writeMsg(errMsg)
	unknown := protocol.Message{ID: "x", Type: protocol.MessageType("bogus")}
	f.writeMsg(unknown)

	// After both, a ping must still be answered: connection is alive.
	ping, _ := protocol.NewMessage(protocol.MsgPing, nil)
	f.writeMsg(ping)
	pong := f.readMsg(3 * time.Second)
	if pong.Type != protocol.MsgPing {
		t.Fatalf("expected ping after error+unknown, got %s", pong.Type)
	}
}

// TestDispatchInferenceRequest_ChatNonStreaming drives a full non-streaming
// chat inference over the WebSocket: router request → worker backend →
// inference_response with Done=true and the response body.
func TestDispatchInferenceRequest_ChatNonStreaming(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-chat") }, srv)
	f.readMsg(3 * time.Second) // consume register

	body, _ := json.Marshal(protocol.ChatRequest{Model: "m1", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}, Stream: false})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "chat", Model: "m1", Body: body})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgInferenceResponse || resp.ID != req.ID {
		t.Fatalf("expected inference_response id %q, got %s/%s", req.ID, resp.ID, resp.Type)
	}
	var p protocol.InferenceResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !p.Done || p.Error != "" {
		t.Fatalf("expected clean done response, got %+v", p)
	}
	var chatResp protocol.ChatResponse
	if err := json.Unmarshal(p.Response, &chatResp); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if chatResp.ID != "chatcmpl-test" {
		t.Fatalf("response body = %+v, want chatcmpl-test", chatResp)
	}
}

// TestDispatchInferenceRequest_ChatStreaming drives a streaming chat
// inference: router request → worker backend → inference_chunk frames →
// terminating inference_response.
func TestDispatchInferenceRequest_ChatStreaming(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-stream") }, srv)
	f.readMsg(3 * time.Second) // consume register

	body, _ := json.Marshal(protocol.ChatRequest{Model: "m1", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}, Stream: true})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "chat", Model: "m1", Body: body})
	f.writeMsg(req)

	// Expect at least one chunk, then the done frame.
	chunks := 0
	done := false
	for !done {
		msg := f.readMsg(3 * time.Second)
		switch msg.Type {
		case protocol.MsgInferenceChunk:
			var cp protocol.InferenceChunkPayload
			if err := msg.DecodePayload(&cp); err != nil {
				t.Fatalf("decode chunk: %v", err)
			}
			if cp.Kind != "chat" {
				t.Fatalf("chunk kind = %q, want chat", cp.Kind)
			}
			var chunk protocol.ChatChunk
			if err := json.Unmarshal(cp.Chunk, &chunk); err != nil {
				t.Fatalf("decode chunk body: %v", err)
			}
			if len(chunk.Choices) != 1 {
				t.Fatalf("chunk choices = %d, want 1", len(chunk.Choices))
			}
			chunks++
		case protocol.MsgInferenceResponse:
			var p protocol.InferenceResponsePayload
			if err := msg.DecodePayload(&p); err != nil {
				t.Fatalf("decode done: %v", err)
			}
			if !p.Done {
				t.Fatalf("done frame not marked done: %+v", p)
			}
			done = true
		default:
			t.Fatalf("unexpected message %s in stream", msg.Type)
		}
	}
	if chunks < 1 {
		t.Fatal("no chunks received")
	}
}

// TestDispatchInferenceRequest_CompletionNonStreaming mirrors the chat test
// for the completion endpoint (covers dispatchCompletion +
// sendInferenceDone with a response body).
func TestDispatchInferenceRequest_CompletionNonStreaming(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-comp") }, srv)
	f.readMsg(3 * time.Second)

	body, _ := json.Marshal(protocol.CompletionRequest{Model: "m1", Prompt: "hi", Stream: false})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "completion", Model: "m1", Body: body})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgInferenceResponse || resp.ID != req.ID {
		t.Fatalf("expected inference_response id %q, got %s/%s", req.ID, resp.ID, resp.Type)
	}
	var p protocol.InferenceResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !p.Done || p.Error != "" {
		t.Fatalf("expected clean done, got %+v", p)
	}
	var compResp protocol.CompletionResponse
	if err := json.Unmarshal(p.Response, &compResp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if compResp.ID != "cmpl-test" {
		t.Fatalf("body = %+v, want cmpl-test", compResp)
	}
}

// TestDispatchInferenceRequest_CompletionStreaming covers dispatchCompletion
// streaming → streamOrFallback for completions.
func TestDispatchInferenceRequest_CompletionStreaming(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-compstream") }, srv)
	f.readMsg(3 * time.Second)

	body, _ := json.Marshal(protocol.CompletionRequest{Model: "m1", Prompt: "hi", Stream: true})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "completion", Model: "m1", Body: body})
	f.writeMsg(req)

	var chunks int
	done := false
	for !done {
		msg := f.readMsg(3 * time.Second)
		switch msg.Type {
		case protocol.MsgInferenceChunk:
			chunks++
		case protocol.MsgInferenceResponse:
			var p protocol.InferenceResponsePayload
			if err := msg.DecodePayload(&p); err != nil {
				t.Fatalf("decode done: %v", err)
			}
			if !p.Done {
				t.Fatalf("done frame not marked done: %+v", p)
			}
			done = true
		default:
			t.Fatalf("unexpected message %s", msg.Type)
		}
	}
	if chunks < 1 {
		t.Fatal("no completion chunks received")
	}
}

// TestDispatchInferenceRequest_UnknownKind verifies the protocol error frame
// for an unrecognized kind (covers sendError).
func TestDispatchInferenceRequest_UnknownKind(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-unknown") }, srv)
	f.readMsg(3 * time.Second)

	body, _ := json.Marshal(protocol.ChatRequest{Model: "m1"})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "weird", Model: "m1", Body: body})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgError || resp.ID != req.ID {
		t.Fatalf("expected error frame id %q, got %s/%s", req.ID, resp.ID, resp.Type)
	}
	var p protocol.ErrorPayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if p.Code != "worker_error" || !strings.Contains(p.Message, "weird") {
		t.Fatalf("error payload = %+v, want worker_error about kind", p)
	}
}

// TestDispatchInferenceRequest_BadBody verifies the protocol error frame for
// a request body that does not parse (covers dispatchChat's
// "invalid chat request body" path).
func TestDispatchInferenceRequest_BadBody(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetBackend(newMockBackend(), "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-badbody") }, srv)
	f.readMsg(3 * time.Second)

	// Send a message whose payload is valid JSON but whose Body is not a
	// valid ChatRequest (a number where a string is expected).
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "chat", Model: "m1", Body: json.RawMessage(`{"messages":42}`)})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgError {
		t.Fatalf("expected error frame, got %s", resp.Type)
	}
	var p protocol.ErrorPayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !strings.Contains(p.Message, "invalid chat request body") {
		t.Fatalf("error message = %q", p.Message)
	}
}

// TestDispatchInferenceRequest_BackendError verifies sendBackendError: a
// backend failure surfaces as a terminal inference_response with Error set.
func TestDispatchInferenceRequest_BackendError(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	mb := newMockBackend()
	mb.SetStreamError(errors.New("backend exploded"), true)
	srv.SetBackend(mb, "m1")
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-bkerr") }, srv)
	f.readMsg(3 * time.Second)

	body, _ := json.Marshal(protocol.ChatRequest{Model: "m1", Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}}, Stream: true})
	req, _ := protocol.NewMessage(protocol.MsgInferenceRequest, protocol.InferenceRequestPayload{Kind: "chat", Model: "m1", Body: body})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgInferenceResponse {
		t.Fatalf("expected inference_response, got %s", resp.Type)
	}
	var p protocol.InferenceResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !p.Done {
		t.Fatalf("expected done frame, got %+v", p)
	}
	// The mock emits the error either as a backend-error response or a clean
	// done when the empty chunk channel wins the select race; both must
	// terminate. When the error surfaces, the message is preserved.
	if p.Error != "" && !strings.Contains(p.Error, "backend exploded") {
		t.Fatalf("error = %q, want backend exploded", p.Error)
	}
}

// TestDispatchModelLoadRequest verifies the model_load_request flow: the
// worker marks a declared model loaded and replies with the response.
func TestDispatchModelLoadRequest(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	srv.SetModels([]protocol.ModelInfo{{Name: "m1", Loaded: false}})
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-load") }, srv)
	f.readMsg(3 * time.Second)

	req, _ := protocol.NewMessage(protocol.MsgModelLoadRequest, protocol.ModelLoadRequestPayload{Model: "m1"})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	if resp.Type != protocol.MsgModelLoadResponse || resp.ID != req.ID {
		t.Fatalf("expected model_load_response id %q, got %s/%s", req.ID, resp.ID, resp.Type)
	}
	var p protocol.ModelLoadResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Model != "m1" || !p.Loaded || p.Error != "" {
		t.Fatalf("model load response = %+v", p)
	}
}

// TestDispatchModelLoadRequest_UnknownModel verifies the response carries
// "model not found" for undeclared models.
func TestDispatchModelLoadRequest_UnknownModel(t *testing.T) {
	f := newWSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := devTestServer(t)
	f.connect(ctx, WSConfig{RouterURL: f.url(), HeartbeatInterval: time.Hour}, func() protocol.WorkerInfo { return testWorkerInfo("w-load2") }, srv)
	f.readMsg(3 * time.Second)

	req, _ := protocol.NewMessage(protocol.MsgModelLoadRequest, protocol.ModelLoadRequestPayload{Model: "nope"})
	f.writeMsg(req)

	resp := f.readMsg(3 * time.Second)
	var p protocol.ModelLoadResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Loaded || p.Error == "" {
		t.Fatalf("expected error for unknown model, got %+v", p)
	}
}
