package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func TestChatCompletionsTimeoutRecords504(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	hub := NewWSHub(tr.reg, testLogger())
	hub.SetMaxInFlight(10)
	hub.Start(context.Background())
	wsServer := httptest.NewServer(hub.Handler())
	defer wsServer.Close()

	srv := tr.Server()
	srv.hub = hub
	srv.SetProxyTimeout(200 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "to-worker", Hostname: "to-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1", Transport: protocol.TransportWS,
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "to-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
	// Register the worker in the hub so recordTimeout has a connection to
	// bump the 504 counter against.
	w := dialFakeWorker(t, wsServer.URL+"/v1/connect")
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	// A fake client that never produces a chunk and surfaces a timeout error
	// once the context is cancelled — this is exactly the path a stalled
	// worker takes to reach the 504 branch in proxyStream. Its RecordTimeout
	// forwards to the hub's per-worker 504 counter.
	fc := &timeoutClient{workerID: info.ID, hub: hub}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()
	srv.proxyStream(wrec, req, fc, info, "chat", []byte(`{"model":"to-model","messages":[]}`), "")

	if wrec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusGatewayTimeout, wrec.Code, wrec.Body.String())
	}

	snap := hub.QueueStats()
	if len(snap.Workers) != 1 {
		t.Fatalf("expected 1 worker in stats, got %d", len(snap.Workers))
	}
	if snap.Workers[0].Rejected504Total != 1 {
		t.Errorf("expected rejected_504_total=1, got %d", snap.Workers[0].Rejected504Total)
	}
	if snap.Pool.TotalRejected504 != 1 {
		t.Errorf("expected pool rejected_504=1, got %d", snap.Pool.TotalRejected504)
	}
}
func TestProxyStreamContextTimeout(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.SetProxyTimeout(50 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "ctx-timeout-worker", Hostname: "ctx-timeout-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "ctx-timeout-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	// A client that never sends chunks or errors — simulates a worker that
	// is completely silent (e.g., a proxy or network stall that never
	// surfaces ctx cancellation). proxyStream must handle ctx.Done() itself.
	stalledClient := &stalledClient{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()

	start := time.Now()
	srv.proxyStream(wrec, req, stalledClient, info, "chat", []byte(`{"model":"ctx-timeout-model","messages":[]}`), "")
	elapsed := time.Since(start)

	if wrec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status %d, got %d, body: %s", http.StatusGatewayTimeout, wrec.Code, wrec.Body.String())
	}
	if elapsed < 40*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Errorf("proxyStream returned too quickly (%.2fms) — ctx.Done() branch may not have fired", float64(elapsed.Milliseconds()))
	}
}
func TestProxyStreamIdleTimeoutSlowActive(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.SetProxyTimeout(300 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "idle-active-worker", Hostname: "idle-active-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "idle-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	// 15 chunks every 50ms = 700ms+ total, well beyond the 300ms idle timeout.
	client := &activeStreamClient{tickMs: 50, chunks: 15}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()

	start := time.Now()
	srv.proxyStream(wrec, req, client, info, "chat", []byte(`{"model":"idle-model","messages":[]}`), "")
	elapsed := time.Since(start)

	body := wrec.Body.String()
	// All 15 chunks plus the Done sentinel must have been relayed.
	for i := 0; i < 15; i++ {
		if !strings.Contains(body, fmt.Sprintf("data: chunk %d\n\n", i)) {
			t.Errorf("expected chunk %d in body, got: %s", i, body)
			break
		}
	}
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Errorf("expected final [DONE] marker, got: %s", body)
	}
	if wrec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected SSE content type, got %q", wrec.Header().Get("Content-Type"))
	}
	// The stream must have taken the full ~700ms — the 300ms idle timeout
	// must not have cut it off.
	if elapsed < 600*time.Millisecond {
		t.Errorf("stream ended after %.0fms — idle timer may not be resetting on activity", float64(elapsed.Milliseconds()))
	}
}
func TestProxyStreamIdleTimeoutSilentMidStream(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.SetProxyTimeout(300 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "idle-silent-worker", Hostname: "idle-silent-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "idle-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	client := &silentAfterClient{silenceMs: 2000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()

	start := time.Now()
	srv.proxyStream(wrec, req, client, info, "chat", []byte(`{"model":"idle-model","messages":[]}`), "")
	elapsed := time.Since(start)

	body := wrec.Body.String()
	if wrec.Code != http.StatusOK {
		t.Errorf("expected status 200 (stream already started), got %d, body: %s", wrec.Code, body)
	}
	if !strings.Contains(body, "data: first\n\n") {
		t.Errorf("expected first chunk in body, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Errorf("expected clean [DONE] termination after mid-stream silence, got: %s", body)
	}
	// Should have waited for the idle timeout (~300ms) before terminating,
	// not returned immediately and not hung.
	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("proxyStream took %.0fms — expected ~300ms idle wait", float64(elapsed.Milliseconds()))
	}
}
func TestProxyStreamIdleTimeoutSilentFromStart(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.SetProxyTimeout(200 * time.Millisecond)

	info := protocol.WorkerInfo{
		ID: "idle-cold-worker", Hostname: "idle-cold-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "idle-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	// stalledClient never produces chunks or errors: the idle timer is the
	// only way out.
	stalled := &stalledClient{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()

	start := time.Now()
	srv.proxyStream(wrec, req, stalled, info, "chat", []byte(`{"model":"idle-model","messages":[]}`), "")
	elapsed := time.Since(start)

	if wrec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status 504, got %d, body: %s", wrec.Code, wrec.Body.String())
	}
	if ct := wrec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json before first chunk, got %q", ct)
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("proxyStream took %.0fms — expected ~200ms idle wait", float64(elapsed.Milliseconds()))
	}
}
func TestProxyStreamClientCancellation(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.SetProxyTimeout(10 * time.Second) // long enough that only the client cancels

	info := protocol.WorkerInfo{
		ID: "idle-cancel-worker", Hostname: "idle-cancel-worker", IP: "127.0.0.1", Port: 8081,
		Status: protocol.StatusAvailable, Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "idle-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}

	stalled := &stalledClient{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	wrec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.proxyStream(wrec, req, stalled, info, "chat", []byte(`{"model":"idle-model","messages":[]}`), "")
	}()

	// Give proxyStream time to enter its select loop, then cancel the client.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyStream did not return after client cancellation")
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("proxyStream took %.0fms after client cancel — expected prompt return", float64(elapsed.Milliseconds()))
	}
}
