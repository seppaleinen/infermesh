package router

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestConnStateCounter(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	server := tr.Server()

	// Attach the same ConnState hook Start() installs so the test drives the
	// exact production code path without binding a socket.
	server.server = &http.Server{}
	server.server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			server.connections.Add(1)
		case http.StateClosed:
			server.connections.Add(-1)
		}
	}

	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected initial count 0, got %d", got)
	}

	server.server.ConnState(nil, http.StateNew)
	server.server.ConnState(nil, http.StateNew)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count 2 after two opens, got %d", got)
	}

	// Keep-alive and hijack transitions must not change the count.
	server.server.ConnState(nil, http.StateIdle)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count unchanged (2) after StateIdle, got %d", got)
	}
	server.server.ConnState(nil, http.StateHijacked)
	if got := server.connections.Load(); got != 2 {
		t.Errorf("expected count unchanged (2) after StateHijacked, got %d", got)
	}

	server.server.ConnState(nil, http.StateClosed)
	if got := server.connections.Load(); got != 1 {
		t.Errorf("expected count 1 after one close, got %d", got)
	}

	server.server.ConnState(nil, http.StateClosed)
	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected count 0 after closing remaining connection, got %d", got)
	}
}
func TestConnectionCountEndpointIntegration(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	server := tr.Server()

	// Build a mux with the production handler. mTLS/API-key wrapping is dev
	// mode-only in this test, so a plain mux mirrors the behaviour exercised
	// by the real server.
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/connections/count", server.handleConnectionCount)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	server.server = &http.Server{Handler: mux}
	server.server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			server.connections.Add(1)
		case http.StateClosed:
			server.connections.Add(-1)
		}
	}

	go func() { _ = server.server.Serve(ln) }()
	defer func() { _ = server.server.Close() }()

	addr := ln.Addr().String()
	url := "http://" + addr + "/meta/connections/count"

	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected initial connection count 0, got %d", got)
	}

	// GET should return 200 with JSON payload "clients": >=0.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /meta/connections/count: %v", err)
	}
	var body struct {
		Connections int `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if body.Connections < 0 {
		t.Errorf("expected connections >= 0, got %d", body.Connections)
	}

	// After a request with keep-alive the server should have an idle connection.
	if got := server.connections.Load(); got != 1 {
		t.Errorf("expected connection count 1 after request (keep-alive), got %d", got)
	}

	// POST method must be rejected with 405 Method Not Allowed.
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new POST request: %v", err)
	}
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /meta/connections/count: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for POST, got %d", resp2.StatusCode)
	}

	// Close idle connections so StateClosed callbacks fire.
	if trpt, ok := http.DefaultTransport.(*http.Transport); ok {
		trpt.CloseIdleConnections()
	}

	// Poll until the counter returns to zero or timeout to avoid flakiness.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.connections.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.connections.Load(); got != 0 {
		t.Errorf("expected connection count 0 after closing idle connections, got %d", got)
	}
}
func inflightBody(t *testing.T, srv *Server, method string) (*http.Response, InflightRequestsResponse) {
	t.Helper()
	req := httptest.NewRequest(method, "/meta/requests/inflight", nil)
	w := httptest.NewRecorder()
	srv.handleInflightRequests(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var traced InflightRequestsResponse
	if err := json.Unmarshal(body, &traced); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, traced
}
func TestInflightRequestsEndpoint(t *testing.T) {
	workers := []protocol.WorkerInfo{mkWorker("worker-a", "trace-model")}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`),
	}

	// No requests yet: empty, non-null slice.
	resp, empty := inflightBody(t, srv, http.MethodGet)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if empty.Requests == nil {
		t.Fatal("requests is nil, want empty non-null slice")
	}
	if len(empty.Requests) != 0 {
		t.Errorf("len(requests) = %d, want 0", len(empty.Requests))
	}

	// Dispatch a non-streaming request; the tracker must record it as done.
	b := []byte(`{"model":"trace-model","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := srv.dispatchWithFailover(context.Background(), "trace-model", b, "chat", 1, 10*time.Second); err != nil {
		t.Fatalf("dispatchWithFailover: %v", err)
	}

	resp, traced := inflightBody(t, srv, http.MethodGet)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(traced.Requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(traced.Requests))
	}
	r := traced.Requests[0]
	if r.Model != "trace-model" || r.WorkerID != "worker-a" || r.Kind != "chat" {
		t.Errorf("record = %+v", r)
	}
	if r.Status != StatusDone {
		t.Errorf("status = %q, want %q", r.Status, StatusDone)
	}
	if r.DurationMs < 0 {
		t.Errorf("duration_ms = %d, want >= 0", r.DurationMs)
	}
	if r.FinishedAt.IsZero() {
		t.Error("finished_at is zero")
	}
}
func TestInflightRequestsMethodNotAllowed(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})
	req := httptest.NewRequest(http.MethodPost, "/meta/requests/inflight", nil)
	w := httptest.NewRecorder()
	srv.handleInflightRequests(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
func TestInflightRequestsDispatchHookOrdering(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})
	srv.tracker = NewRequestTracker(0, 0)

	// A mock worker client whose Complete() blocks for 2s, so we can observe
	// the trace while the request is in flight. The dispatch budget (5s) is
	// wider than the block, so the request still completes and is marked done.
	fc := newMockWorkerClient()
	fc.results["worker-1"] = mockResult{body: []byte(`{"ok":true}`), block: 2 * time.Second}
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }

	// A registry with one worker advertising the model, plus a capability
	// cache populated so selectWorker() finds it (mirrors failoverTestServer).
	tr := testRegistry(t, []protocol.WorkerInfo{mkWorker("worker-1", "qwen3-8b")})
	srv.reg = tr.reg
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	srv.cache.cache["worker-1"] = protocol.WorkerInfo{ID: "worker-1", Hostname: "host-1"}
	srv.cache.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.dispatchWithFailover(context.Background(), "qwen3-8b", []byte(`{}`), "chat", 1, 5*time.Second)
	}()

	// Give the dispatch goroutine time to enter Complete() but not for the
	// 2s block to elapse.
	time.Sleep(100 * time.Millisecond)

	// The request must be visible as in-flight BEFORE Complete() returns.
	snap := srv.tracker.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 in-flight record, got %d: %+v", len(snap), snap)
	}
	if snap[0].Status != StatusInflight {
		t.Errorf("status = %q, want %q (request should be in-flight, not done)", snap[0].Status, StatusInflight)
	}
	if snap[0].WorkerHost != "host-1" {
		t.Errorf("worker_host = %q, want %q", snap[0].WorkerHost, "host-1")
	}

	// Wait for the dispatch to finish and confirm it transitions to "done".
	<-done
	snap = srv.tracker.Snapshot()
	if len(snap) != 1 || snap[0].Status != StatusDone {
		t.Errorf("after completion: %+v", snap)
	}
	tr.cancel()
}
func TestInflightRequestsBounded(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})
	srv.tracker = NewRequestTracker(5, 1*time.Hour)
	for i := 0; i < 50; i++ {
		id := srv.tracker.BeginSimple("m", "w", "chat")
		srv.tracker.Finish(id, StatusDone)
	}
	if got := len(srv.tracker.Snapshot()); got != 5 {
		t.Errorf("snapshot len = %d, want 5 (bounded)", got)
	}
}
