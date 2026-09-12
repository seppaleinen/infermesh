package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
	"golang.org/x/net/websocket"
)

// fakeWorker drives a client-side WebSocket connection acting as a worker.
type fakeWorker struct {
	t    *testing.T
	conn *websocket.Conn
}

func dialFakeWorker(t *testing.T, url string) *fakeWorker {
	t.Helper()
	// httptest servers expose http:// URLs; the websocket config needs ws://.
	wsURL := "ws://" + strings.TrimPrefix(url, "http://")
	conn, err := wsutil.Dial(context.Background(), wsURL, "")
	if err != nil {
		t.Fatalf("failed to dial hub: %v", err)
	}
	t.Cleanup(func() { _ = wsutil.Close(conn) })
	return &fakeWorker{t: t, conn: conn}
}

func (w *fakeWorker) send(t *testing.T, msg protocol.Message) {
	t.Helper()
	if err := wsutil.SendJSON(w.conn, msg); err != nil {
		t.Fatalf("fake worker send failed: %v", err)
	}
}

func (w *fakeWorker) recv(t *testing.T, timeout time.Duration) protocol.Message {
	t.Helper()
	if err := wsutil.SetReadDeadline(w.conn, time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var msg protocol.Message
	if err := wsutil.ReceiveJSON(w.conn, &msg); err != nil {
		t.Fatalf("fake worker receive failed: %v", err)
	}
	return msg
}

func rawMsg(t *testing.T, mt protocol.MessageType, payload interface{}) protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(mt, payload)
	if err != nil {
		t.Fatalf("build %s message: %v", mt, err)
	}
	return msg
}

// replyTo builds a response whose ID echoes the correlated request.
func replyTo(t *testing.T, req protocol.Message, mt protocol.MessageType, payload interface{}) protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(mt, payload)
	if err != nil {
		t.Fatalf("build %s message: %v", mt, err)
	}
	msg.ID = req.ID
	return msg
}

func testWSHub(t *testing.T) (*WSHub, *httptest.Server) {
	t.Helper()
	reg, err := registry.New(registry.Defaults(), testLogger())
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	hub := NewWSHub(reg, testLogger())
	hub.Start(context.Background())
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = reg.Stop()
	})
	return hub, server
}

func TestWSHubRegisterAndClient(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-1", Hostname: "worker-1", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))

	// Router greets with the registered ID.
	welcome := w.recv(t, 3*time.Second)
	if welcome.Type != protocol.MsgWelcome {
		t.Fatalf("expected welcome, got %s", welcome.Type)
	}
	var wp protocol.WelcomePayload
	if err := welcome.DecodePayload(&wp); err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if wp.WorkerID != "ws-worker-1" {
		t.Fatalf("expected worker id ws-worker-1, got %q", wp.WorkerID)
	}

	// Hub exposes a client for the connected worker.
	client := hub.Client("ws-worker-1")
	if client == nil {
		t.Fatal("hub.Client returned nil for connected worker")
	}
	if client.Transport() != protocol.TransportWS {
		t.Fatalf("expected transport ws, got %q", client.Transport())
	}

	// Registry saw the event through the bridge.
	got, ok := hub.reg.Get("ws-worker-1")
	if !ok {
		t.Fatal("worker missing from registry after register")
	}
	if got.Transport != protocol.TransportWS {
		t.Fatalf("expected registered transport ws, got %q", got.Transport)
	}
}

func TestWSHubHeartbeatBridgesUpdate(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-2", Hostname: "worker-2", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// Heartbeat with freshly loaded models.
	updated := info
	updated.Capabilities = protocol.Capabilities{
		Models: []protocol.ModelInfo{{Name: "m-1", Loaded: true}},
	}
	w.send(t, rawMsg(t, protocol.MsgHeartbeat, protocol.HeartbeatPayload{Worker: updated}))

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, ok := hub.reg.Get("ws-worker-2")
		if ok && len(got.Capabilities.Models) == 1 && got.Capabilities.Models[0].Name == "m-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry did not reflect heartbeat update: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWSHubStreamingCall(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-3", Hostname: "worker-3", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := hub.Client("ws-worker-3")
	if client == nil {
		t.Fatal("hub.Client returned nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chunks, errs := client.Stream(ctx, info, "chat", []byte(`{"model":"m-1","messages":[]}`))

	// Worker receives the request with a correlation id.
	req := w.recv(t, 3*time.Second)
	if req.Type != protocol.MsgInferenceRequest {
		t.Fatalf("expected inference_request, got %s", req.Type)
	}
	var ip protocol.InferenceRequestPayload
	if err := req.DecodePayload(&ip); err != nil {
		t.Fatalf("decode inference request: %v", err)
	}
	if ip.Kind != "chat" {
		t.Fatalf("expected kind chat, got %q", ip.Kind)
	}

	// Stream two chunks then a terminal done response.
	w.send(t, replyTo(t, req, protocol.MsgInferenceChunk, protocol.InferenceChunkPayload{
		Kind: "chat", Chunk: json.RawMessage(`{"delta":"hi"}`),
	}))
	w.send(t, replyTo(t, req, protocol.MsgInferenceChunk, protocol.InferenceChunkPayload{
		Kind: "chat", Chunk: json.RawMessage(`{"delta":" there"}`),
	}))
	w.send(t, replyTo(t, req, protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind: "chat", Done: true,
	}))

	var events []StreamEvent
	for len(events) < 3 {
		select {
		case ev, ok := <-chunks:
			if !ok {
				t.Fatalf("stream closed after %d events; expected 3", len(events))
			}
			events = append(events, ev)
		case err := <-errs:
			t.Fatalf("unexpected stream error: %v", err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for stream events; got %d", len(events))
		}
	}

	// Chunks arrive SSE-framed; the terminal event carries Done=true.
	wantFrames := []string{"data: {\"delta\":\"hi\"}\n\n", "data: {\"delta\":\" there\"}\n\n"}
	for i, want := range wantFrames {
		if string(events[i].Data) != want {
			t.Fatalf("event %d: got %q, want %q", i, string(events[i].Data), want)
		}
		if events[i].Done {
			t.Fatalf("event %d should not be done", i)
		}
	}
	if !events[2].Done {
		t.Fatalf("expected terminal [DONE] event, got %q", string(events[2].Data))
	}
	if string(events[2].Data) != "data: [DONE]\n\n" {
		t.Fatalf("terminal event: got %q, want %q", string(events[2].Data), "data: [DONE]\n\n")
	}

	// The chunk channel is closed once the stream completes.
	if _, ok := <-chunks; ok {
		t.Fatal("expected chunk channel to be closed after done")
	}
}

func TestWSHubLoadModel(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-4", Hostname: "worker-4", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := hub.Client("ws-worker-4")
	if client == nil {
		t.Fatal("hub.Client returned nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var loaded bool
	var loadErr error
	go func() {
		loaded, loadErr = client.LoadModel(ctx, info, "m-1")
		close(done)
	}()

	req := w.recv(t, 3*time.Second)
	if req.Type != protocol.MsgModelLoadRequest {
		t.Fatalf("expected model_load_request, got %s", req.Type)
	}
	var lp protocol.ModelLoadRequestPayload
	if err := req.DecodePayload(&lp); err != nil {
		t.Fatalf("decode model load request: %v", err)
	}
	if lp.Model != "m-1" {
		t.Fatalf("expected model m-1, got %q", lp.Model)
	}

	w.send(t, replyTo(t, req, protocol.MsgModelLoadResponse, protocol.ModelLoadResponsePayload{
		Model: "m-1", Loaded: true,
	}))

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for load model result")
	}
	if loadErr != nil {
		t.Fatalf("load model returned error: %v", loadErr)
	}
	if !loaded {
		t.Fatal("expected load model to succeed")
	}
}

// testLogger returns a default test logger.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}
