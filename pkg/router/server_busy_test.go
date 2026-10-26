package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)
func TestBusyStatus(t *testing.T) {
	tests := []struct {
		name   string
		cached protocol.WorkerStatus
		load   WorkerQueueStats
		want   protocol.WorkerStatus
	}{
		{
			name:   "available at/above 80% of cap is busy (default 4 -> 3 in-flight)",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 3, MaxInFlight: 4},
			want:   protocol.StatusBusy,
		},
		{
			name:   "available exactly at cap is busy",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 4, MaxInFlight: 4},
			want:   protocol.StatusBusy,
		},
		{
			name:   "available below 80% stays available",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 2, MaxInFlight: 4},
			want:   protocol.StatusAvailable,
		},
		{
			name:   "available with zero in-flight stays available",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 0, MaxInFlight: 4},
			want:   protocol.StatusAvailable,
		},
		{
			name:   "custom cap 10 -> busy at 8 in-flight",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 8, MaxInFlight: 10},
			want:   protocol.StatusBusy,
		},
		{
			name:   "custom cap 10 -> 7 in-flight stays available",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 7, MaxInFlight: 10},
			want:   protocol.StatusAvailable,
		},
		{
			name:   "unavailable is never downgraded/upgraded to busy",
			cached: protocol.StatusUnavailable,
			load:   WorkerQueueStats{InFlight: 4, MaxInFlight: 4},
			want:   protocol.StatusUnavailable,
		},
		{
			name:   "unassigned is never upgraded to busy",
			cached: protocol.StatusUnassigned,
			load:   WorkerQueueStats{InFlight: 4, MaxInFlight: 4},
			want:   protocol.StatusUnassigned,
		},
		{
			name:   "busy worker with no load entry keeps its status",
			cached: protocol.StatusBusy,
			load:   WorkerQueueStats{},
			want:   protocol.StatusBusy,
		},
		{
			name:   "available with unknown cap keeps its status",
			cached: protocol.StatusAvailable,
			load:   WorkerQueueStats{InFlight: 5, MaxInFlight: 0},
			want:   protocol.StatusAvailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := busyStatus(tt.cached, tt.load); got != tt.want {
				t.Errorf("busyStatus(%s, %+v) = %s, want %s", tt.cached, tt.load, got, tt.want)
			}
		})
	}
}
func TestWorkersListBusyStatus(t *testing.T) {
	busyWorker := busyWorkerTestWorker("busy-ws", protocol.StatusAvailable)
	idleWorker := busyWorkerTestWorker("idle-ws", protocol.StatusAvailable)
	unavailableWorker := busyWorkerTestWorker("unavail-ws", protocol.StatusUnavailable)

	tr, srv := busyTestServer(t, []protocol.WorkerInfo{busyWorker, idleWorker, unavailableWorker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Deterministic load: the busy worker is at the threshold (3 of 4, the
	// default cap — floor(0.80*4)=3, so 3 in-flight IS busy), the idle worker
	// has no load entry (0 in-flight), and the unavailable worker is
	// saturated (4 of 4) but must NOT be upgraded.
	srv.busyLoadSnapshot = func() map[string]WorkerQueueStats {
		return map[string]WorkerQueueStats{
			busyWorker.ID:        {WorkerID: busyWorker.ID, InFlight: 3, MaxInFlight: 4},
			idleWorker.ID:        {WorkerID: idleWorker.ID, InFlight: 0, MaxInFlight: 4},
			unavailableWorker.ID: {WorkerID: unavailableWorker.ID, InFlight: 4, MaxInFlight: 4},
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	w := httptest.NewRecorder()
	srv.handleWorkersList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	var resp WorkersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	statusByID := make(map[string]protocol.WorkerStatus, len(resp.Workers))
	for _, w := range resp.Workers {
		statusByID[w.ID] = w.Status
	}

	if got := statusByID[busyWorker.ID]; got != protocol.StatusBusy {
		t.Errorf("busy worker status = %s, want %s", got, protocol.StatusBusy)
	}
	if got := statusByID[idleWorker.ID]; got != protocol.StatusAvailable {
		t.Errorf("idle worker status = %s, want %s", got, protocol.StatusAvailable)
	}
	if got := statusByID[unavailableWorker.ID]; got != protocol.StatusUnavailable {
		t.Errorf("unavailable worker status = %s, want %s", got, protocol.StatusUnavailable)
	}
}
func TestWorkersListBusyStatusBelowThreshold(t *testing.T) {
	worker := busyWorkerTestWorker("below-ws", protocol.StatusAvailable)

	tr, srv := busyTestServer(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv.busyLoadSnapshot = func() map[string]WorkerQueueStats {
		return map[string]WorkerQueueStats{
			worker.ID: {WorkerID: worker.ID, InFlight: 1, MaxInFlight: 4},
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	w := httptest.NewRecorder()
	srv.handleWorkersList(w, req)

	var resp WorkersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(resp.Workers))
	}
	if got := resp.Workers[0].Status; got != protocol.StatusAvailable {
		t.Errorf("below-threshold worker status = %s, want %s", got, protocol.StatusAvailable)
	}
}
func TestWorkersListBusyStatusLiveWS(t *testing.T) {
	worker := busyWorkerTestWorker("live-ws", protocol.StatusAvailable)

	tr := testRegistry(t, []protocol.WorkerInfo{worker})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	hub := NewWSHub(tr.reg, testLogger())
	hub.SetMaxInFlight(1)
	hub.Start(context.Background())
	wsServer := httptest.NewServer(hub.Handler())
	defer wsServer.Close()

	srv := tr.Server()
	srv.hub = hub

	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	srv.cache.cache[worker.ID] = worker
	srv.cache.mu.Unlock()

	info := worker
	info.Transport = protocol.TransportWS
	w := dialFakeWorker(t, wsServer.URL+"/v1/connect")
	w.send(t, rawMsg(t, protocol.MsgRegister, protocol.RegisterPayload{Worker: info}))
	_ = w.recv(t, 3*time.Second) // welcome

	chatReq := protocol.ChatRequest{
		Model:    "live-model",
		Stream:   true,
		Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}},
	}
	body, _ := json.Marshal(chatReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	wrec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleChatCompletions(wrec, req)
	}()

	// Wait for the pending call to register (in_flight >= 1).
	deadline := time.Now().Add(3 * time.Second)
	inFlight := false
	for !inFlight {
		stats := hub.QueueStats()
		for _, ws := range stats.Workers {
			if ws.WorkerID == worker.ID && ws.InFlight >= 1 {
				inFlight = true
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("pending call not registered in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	w2 := httptest.NewRecorder()
	srv.handleWorkersList(w2, req2)

	var resp WorkersResponse
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(resp.Workers))
	}
	if got := resp.Workers[0].Status; got != protocol.StatusBusy {
		t.Errorf("live worker status = %s, want %s", got, protocol.StatusBusy)
	}

	cancel()
	<-done
}
func TestWorkersListNoHubKeepsStatus(t *testing.T) {
	available := busyWorkerTestWorker("no-hub-avail", protocol.StatusAvailable)
	unavailable := busyWorkerTestWorker("no-hub-unavail", protocol.StatusUnavailable)

	tr, srv := busyTestServer(t, []protocol.WorkerInfo{available, unavailable})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// No hub: srv.hub is set by NewServer but QueueStats() on a fresh hub
	// with no connections returns an empty worker list, so no busy overlay
	// is applied.
	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	w := httptest.NewRecorder()
	srv.handleWorkersList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	var resp WorkersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	statusByID := make(map[string]protocol.WorkerStatus, len(resp.Workers))
	for _, w := range resp.Workers {
		statusByID[w.ID] = w.Status
	}
	if got := statusByID[available.ID]; got != protocol.StatusAvailable {
		t.Errorf("available worker status = %s, want %s", got, protocol.StatusAvailable)
	}
	if got := statusByID[unavailable.ID]; got != protocol.StatusUnavailable {
		t.Errorf("unavailable worker status = %s, want %s", got, protocol.StatusUnavailable)
	}
}
