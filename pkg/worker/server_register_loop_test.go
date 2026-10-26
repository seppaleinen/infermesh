package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func TestRegisterURL(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		expected string
	}{
		{
			name:     "no trailing slash",
			base:     "http://127.0.0.1:8080",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "single trailing slash",
			base:     "http://127.0.0.1:8080/",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "multiple trailing slashes",
			base:     "http://127.0.0.1:8080///",
			expected: "http://127.0.0.1:8080/v1/dev/register",
		},
		{
			name:     "path base without trailing slash",
			base:     "http://127.0.0.1:8080/api/v1",
			expected: "http://127.0.0.1:8080/api/v1/v1/dev/register",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registerURL(tt.base)
			if got != tt.expected {
				t.Errorf("registerURL(%q) = %q, want %q", tt.base, got, tt.expected)
			}
		})
	}
}
func TestRegisterLoopTrailingSlash(t *testing.T) {
	var mu sync.Mutex
	var receivedPath string
	var callCount atomic.Int64

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "trailing-slash-worker",
		IP:   "127.0.0.1",
		Port: 8085,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Intentionally append a trailing slash
	go RegisterLoop(ctx, router.URL+"/", info, 50*time.Millisecond, testLogger())

	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if callCount.Load() < 2 {
		t.Errorf("expected at least 2 calls, got %d", callCount.Load())
	}
	mu.Lock()
	path := receivedPath
	mu.Unlock()
	if path != "/v1/dev/register" {
		t.Errorf("expected path '/v1/dev/register', got '%s'", path)
	}
}
func TestRegisterLoopSendsPost(t *testing.T) {
	var mu sync.Mutex
	var receivedBody protocol.WorkerInfo
	var receivedMethod string
	var receivedPath string
	var callCount atomic.Int64

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		path := r.URL.Path
		var body protocol.WorkerInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		receivedMethod = method
		receivedPath = path
		receivedBody = body
		mu.Unlock()
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:     "loop-worker-1",
		IP:     "127.0.0.1",
		Port:   8081,
		Status: protocol.StatusUnassigned, // should be sent as-is; router decides final status
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())

	// Wait for at least 2 calls (initial + one ticker)
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond) // let goroutine exit

	mu.Lock()
	method := receivedMethod
	path := receivedPath
	body := receivedBody
	mu.Unlock()

	if callCount.Load() < 2 {
		t.Errorf("expected at least 2 calls, got %d", callCount.Load())
	}
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if path != "/v1/dev/register" {
		t.Errorf("expected path '/v1/dev/register', got '%s'", path)
	}
	if body.ID != "loop-worker-1" {
		t.Errorf("expected worker ID 'loop-worker-1', got '%s'", body.ID)
	}
	if body.IP != "127.0.0.1" {
		t.Errorf("expected IP '127.0.0.1', got '%s'", body.IP)
	}
	if body.Port != 8081 {
		t.Errorf("expected port 8081, got %d", body.Port)
	}
}
func TestRegisterLoopContextCancellation(t *testing.T) {
	var callCount atomic.Int64

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "cancel-worker",
		IP:   "127.0.0.1",
		Port: 8082,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())
		close(done)
	}()

	// Let a couple of heartbeats fire
	time.Sleep(150 * time.Millisecond)
	countBeforeCancel := callCount.Load()

	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterLoop did not stop after context cancellation")
	}

	// After cancel, the loop must stop issuing NEW calls. A call already
	// dequeued from the ticker before cancel() can still complete after
	// cancel returns (network round-trip), so the honest bound is "no
	// growth", not "zero growth". Verify that by sampling twice with a gap
	// longer than the tick interval: the count must hold steady.
	time.Sleep(200 * time.Millisecond)
	countAfterCancel := callCount.Load()
	time.Sleep(100 * time.Millisecond)
	countSteady := callCount.Load()

	if countSteady != countAfterCancel {
		t.Errorf("expected no new calls after cancel, got %d then %d", countAfterCancel, countSteady)
	}
	_ = countBeforeCancel
}
func TestRegisterLoopNon200DoesNotCrash(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"something went wrong"}`))
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "error-worker",
		IP:   "127.0.0.1",
		Port: 8083,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger())
		close(done)
	}()

	// Let a few calls happen with 500 responses
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — didn't crash
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterLoop crashed on non-200 response")
	}
}
func TestRegisterLoopImmediatePost(t *testing.T) {
	var callCount atomic.Int64

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	info := protocol.WorkerInfo{
		ID:   "immediate-worker",
		IP:   "127.0.0.1",
		Port: 8084,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RegisterLoop(ctx, router.URL, info, 10*time.Second, testLogger()) // long interval

	// Give enough time for the immediate post, but not enough for the 10s ticker
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if callCount.Load() < 1 {
		t.Errorf("expected at least 1 immediate call, got %d", callCount.Load())
	}
}
