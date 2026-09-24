package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	reg, err := registry.New(registry.Defaults(), wsHubTestLogger())
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	hub := NewWSHub(reg, wsHubTestLogger())
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

// TestWSHubMaxInFlightEnforcesCap tests that addPending enforces the per-worker
// in-flight cap. When the cap is reached, addPending returns errRateLimited.
func TestWSHubMaxInFlightEnforcesCap(t *testing.T) {
	hub, server := testWSHub(t)
	hub.SetMaxInFlight(2)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-cap", Hostname: "worker-cap", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := hub.Client("ws-worker-cap")
	if client == nil {
		t.Fatal("hub.Client returned nil")
	}

	// First stream should succeed. Do NOT drain — the pending entry must stay
	// so the cap is reached on the third attempt.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	chunks1, _ := client.Stream(ctx1, info, "chat", []byte(`{"model":"m","messages":[]}`))
	req1 := w.recv(t, 3*time.Second)
	if req1.Type != protocol.MsgInferenceRequest {
		t.Fatalf("expected inference_request, got %s", req1.Type)
	}

	// Second stream should succeed (in_flight=1 < max=2). Still do NOT drain.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	chunks2, _ := client.Stream(ctx2, info, "chat", []byte(`{"model":"m","messages":[]}`))
	req2 := w.recv(t, 3*time.Second)
	if req2.Type != protocol.MsgInferenceRequest {
		t.Fatalf("expected inference_request, got %s", req2.Type)
	}

	// Third stream should be rejected (in_flight=2 == max=2) — the cap is
	// checked at registration time, before the request is sent.
	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	_, errs3 := client.Stream(ctx3, info, "chat", []byte(`{"model":"m","messages":[]}`))
	select {
	case err := <-errs3:
		if err == nil {
			t.Fatal("expected rate limit error, got nil")
		}
		if !errors.Is(err, errRateLimited) {
			t.Fatalf("expected errRateLimited, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for rate limit error")
	}

	// Drain the first two streams so they finish cleanly.
	w.send(t, replyTo(t, req1, protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind: "chat", Done: true,
	}))
	for ev := range chunks1 {
		_ = ev
	}
	w.send(t, replyTo(t, req2, protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind: "chat", Done: true,
	}))
	for ev := range chunks2 {
		_ = ev
	}
}

// TestWSHubMaxInFlightConfigurable tests that SetMaxInFlight changes the cap.
func TestWSHubMaxInFlightConfigurable(t *testing.T) {
	hub, server := testWSHub(t)
	hub.SetMaxInFlight(1)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-cap1", Hostname: "worker-cap1", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := hub.Client("ws-worker-cap1")
	if client == nil {
		t.Fatal("hub.Client returned nil")
	}

	// One stream should succeed. Do NOT drain — the pending entry must stay
	// so the cap is reached on the second attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chunks, _ := client.Stream(ctx, info, "chat", []byte(`{"model":"m","messages":[]}`))
	req := w.recv(t, 3*time.Second)
	if req.Type != protocol.MsgInferenceRequest {
		t.Fatalf("expected inference_request, got %s", req.Type)
	}

	// Second stream should be rejected (in_flight=1 == max=1) — the cap is
	// checked at registration time, before the request is sent.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, errs2 := client.Stream(ctx2, info, "chat", []byte(`{"model":"m","messages":[]}`))
	select {
	case err := <-errs2:
		if err == nil {
			t.Fatal("expected rate limit error, got nil")
		}
		if !errors.Is(err, errRateLimited) {
			t.Fatalf("expected errRateLimited, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for rate limit error")
	}

	// Drain the first stream.
	w.send(t, replyTo(t, req, protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind: "chat", Done: true,
	}))
	for ev := range chunks {
		_ = ev
	}
}

// TestWSHubQueueStats tracks queue stats on wsConnection.
func TestWSHubQueueStats(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "ws-worker-stats", Hostname: "worker-stats", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	client := hub.Client("ws-worker-stats")
	if client == nil {
		t.Fatal("hub.Client returned nil")
	}

	// Send a stream and verify queue stats are recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chunks, errs := client.Stream(ctx, info, "chat", []byte(`{"model":"m","messages":[]}`))
	req := w.recv(t, 3*time.Second)
	if req.Type != protocol.MsgInferenceRequest {
		t.Fatalf("expected inference_request, got %s", req.Type)
	}

	// Reply with two chunks then Done.
	w.send(t, replyTo(t, req, protocol.MsgInferenceChunk, protocol.InferenceChunkPayload{
		Kind: "chat", Chunk: json.RawMessage(`{"delta":"hi"}`),
	}))
	w.send(t, replyTo(t, req, protocol.MsgInferenceChunk, protocol.InferenceChunkPayload{
		Kind: "chat", Chunk: json.RawMessage(`{"delta":" there"}`),
	}))
	w.send(t, replyTo(t, req, protocol.MsgInferenceResponse, protocol.InferenceResponsePayload{
		Kind: "chat", Done: true,
	}))

	// Wait for stream to complete.
	var got []StreamEvent
	for len(got) < 3 {
		select {
		case ev, ok := <-chunks:
			if !ok {
				t.Fatalf("stream closed after %d events; expected 3", len(got))
			}
			got = append(got, ev)
		case err := <-errs:
			t.Fatalf("unexpected stream error: %v", err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for stream events; got %d", len(got))
		}
	}

	// Check queue stats on the hub.
	snap := hub.QueueStats()
	if len(snap.Workers) != 1 {
		t.Fatalf("expected 1 worker in stats, got %d", len(snap.Workers))
	}
	ws := snap.Workers[0]
	if ws.WorkerID != "ws-worker-stats" {
		t.Fatalf("expected worker_id ws-worker-stats, got %s", ws.WorkerID)
	}
	if ws.InFlight != 0 {
		t.Fatalf("expected in_flight=0 after completion, got %d", ws.InFlight)
	}
	if ws.Rejected429Total != 0 {
		t.Fatalf("expected rejected_429=0, got %d", ws.Rejected429Total)
	}
	if ws.AvgWaitMs < 0 {
		t.Fatalf("avg_wait_ms should be >= 0, got %f", ws.AvgWaitMs)
	}
}

// wsHubTestLogger returns a default test logger.
func wsHubTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

// TestWSHubConnectionCapEnforcesCap tests that the global WebSocket
// connection cap is enforced: the (cap+1)th distinct worker is rejected
// with a protocol error frame and its connection closed, while the first
// `cap` workers are welcomed normally.
func TestWSHubConnectionCapEnforcesCap(t *testing.T) {
	hub, server := testWSHub(t)
	hub.SetMaxConnections(2)

	// First two distinct workers should be accepted.
	for i := 0; i < 2; i++ {
		w := dialFakeWorker(t, server.URL+"/v1/connect")
		info := protocol.WorkerInfo{
			ID: fmt.Sprintf("cap-worker-%d", i), Hostname: "h", IP: "127.0.0.1", Port: 8081,
			Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
		}
		w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
		welcome := w.recv(t, 3*time.Second)
		if welcome.Type != protocol.MsgWelcome {
			t.Fatalf("worker %d: expected welcome, got %s", i, welcome.Type)
		}
	}

	// Third distinct worker must be rejected.
	w := dialFakeWorker(t, server.URL+"/v1/connect")
	info := protocol.WorkerInfo{
		ID: "cap-worker-overflow", Hostname: "h", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))

	// Expect either a MsgError frame or a closed connection (read error).
	_ = wsutil.SetReadDeadline(w.conn, time.Now().Add(3*time.Second))
	var msg protocol.Message
	err := wsutil.ReceiveJSON(w.conn, &msg)
	if err == nil {
		if msg.Type != protocol.MsgError {
			t.Errorf("expected MsgError rejection frame, got %s", msg.Type)
		}
	} else {
		// Connection closed without a frame is also an acceptable rejection.
		t.Logf("rejected worker connection closed: %v", err)
	}

	// The hub must hold exactly `cap` connections.
	hub.mu.RLock()
	n := len(hub.conns)
	hub.mu.RUnlock()
	if n != 2 {
		t.Errorf("expected 2 registered connections, got %d", n)
	}
}

// TestWSHubConnectionCapAllowsSupersede tests that re-registering an
// existing worker ID (same peer) is allowed even when the pool is at
// capacity, because supersede is a 1:1 replacement, not a new connection.
func TestWSHubConnectionCapAllowsSupersede(t *testing.T) {
	hub, server := testWSHub(t)
	hub.SetMaxConnections(1)

	info := protocol.WorkerInfo{
		ID: "sup-worker", Hostname: "h", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}

	// First registration succeeds.
	w1 := dialFakeWorker(t, server.URL+"/v1/connect")
	w1.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	if w1.recv(t, 3*time.Second).Type != protocol.MsgWelcome {
		t.Fatal("first registration should be welcomed")
	}

	// Re-register the same ID from the same peer: supersede, must be allowed.
	w2 := dialFakeWorker(t, server.URL+"/v1/connect")
	w2.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	if w2.recv(t, 3*time.Second).Type != protocol.MsgWelcome {
		t.Fatal("supersede re-registration should be welcomed")
	}

	// A distinct worker ID must be rejected (pool at cap, supersede is 1:1).
	w3 := dialFakeWorker(t, server.URL+"/v1/connect")
	info2 := info
	info2.ID = "other-worker"
	w3.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info2}))
	_ = wsutil.SetReadDeadline(w3.conn, time.Now().Add(3*time.Second))
	var msg protocol.Message
	if err := wsutil.ReceiveJSON(w3.conn, &msg); err == nil && msg.Type != protocol.MsgError {
		t.Errorf("expected MsgError rejection for distinct worker at cap, got %s", msg.Type)
	}
}

// TestWSHubDefaultConnectionCap verifies the default connection cap is seeded
// at construction so the pool is bounded even without an explicit
// --max-connections flag.
func TestWSHubDefaultConnectionCap(t *testing.T) {
	hub, _ := testWSHub(t)
	if hub.maxConnections != defaultMaxConnections {
		t.Errorf("expected default cap %d, got %d", defaultMaxConnections, hub.maxConnections)
	}
}

// TestWSHubRecordTimeoutBumps504Counter verifies that recordTimeout (direct)
// and recordRelayTimeout (relay) increment the per-worker 504 counters that
// /v1/queue/stats surfaces. The counters are declared and read back by the
// stats endpoint but were never incremented by any code path (issue #54).
func TestWSHubRecordTimeoutBumps504Counter(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "to-worker", Hostname: "h", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// Direct-connection 504 path.
	hub.recordTimeout("to-worker")
	// Relay-mode 504 path (worker has no direct connection in the map).
	hub.recordRelayTimeout("relay-to-worker")

	snap := hub.QueueStats()
	if len(snap.Workers) != 1 {
		t.Fatalf("expected 1 worker in stats, got %d", len(snap.Workers))
	}
	if snap.Workers[0].Rejected504Total != 1 {
		t.Errorf("expected direct rejected_504=1, got %d", snap.Workers[0].Rejected504Total)
	}
	if snap.Pool.TotalRejected504 != 2 {
		t.Errorf("expected pool rejected_504=2 (1 direct + 1 relay), got %d", snap.Pool.TotalRejected504)
	}
}

// TestWorkerClientRecordTimeout verifies that both WS client flavors forward
// RecordTimeout to the hub's per-worker 504 counter.
func TestWorkerClientRecordTimeout(t *testing.T) {
	hub, server := testWSHub(t)
	w := dialFakeWorker(t, server.URL+"/v1/connect")

	info := protocol.WorkerInfo{
		ID: "rt-worker", Hostname: "h", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
	}
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	direct := hub.Client("rt-worker")
	if direct == nil {
		t.Fatal("hub.Client returned nil")
	}
	direct.RecordTimeout("rt-worker")

	relay := newRelayWorkerClient(hub, wsHubTestLogger())
	relay.RecordTimeout("relay-rt-worker")

	snap := hub.QueueStats()
	if snap.Pool.TotalRejected504 != 2 {
		t.Errorf("expected pool rejected_504=2, got %d", snap.Pool.TotalRejected504)
	}
}
