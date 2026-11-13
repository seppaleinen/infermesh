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
	go RegisterLoop(ctx, router.URL+"/", info, 50*time.Millisecond, testLogger(), "")

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

	go RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger(), "")

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
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger(), "")
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
		RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger(), "")
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

	go RegisterLoop(ctx, router.URL, info, 10*time.Second, testLogger(), "") // long interval

	// Give enough time for the immediate post, but not enough for the 10s ticker
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if callCount.Load() < 1 {
		t.Errorf("expected at least 1 immediate call, got %d", callCount.Load())
	}
}

// TestRegisterLoopSendsAPIKey verifies the HTTP dev-registration fallback
// authenticates against a key-gated router (issue #202). Without the fix,
// postRegister never sent X-API-Key and every fallback registration was
// rejected with 401, so the worker never joined the pool.
func TestRegisterLoopSendsAPIKey(t *testing.T) {
	// gateKey is the key the test router enforces. Empty means the router
	// admits everything (the pre-existing behavior for unkeyed routers).
	const gateKey = "router-secret"

	tests := []struct {
		name       string
		sentKey    string // what the worker is configured to send
		gateKey    string // what the router enforces ("" = no gate)
		wantStatus int    // status the router must return
		wantHeader bool   // whether the worker must send X-API-Key
	}{
		{
			name:       "no key configured sends no header, unkeyed router admits",
			sentKey:    "",
			gateKey:    "",
			wantStatus: http.StatusOK,
			wantHeader: false,
		},
		{
			name:       "matching key sends header, key-gated router admits",
			sentKey:    gateKey,
			gateKey:    gateKey,
			wantStatus: http.StatusOK,
			wantHeader: true,
		},
		{
			name:       "wrong key sends header, key-gated router rejects with 401",
			sentKey:    "worker-wrong",
			gateKey:    gateKey,
			wantStatus: http.StatusUnauthorized,
			wantHeader: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotKey string
			var gotHeader bool
			var routerStatus int
			var callCount atomic.Int64

			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-API-Key")
				status := http.StatusOK
				if tt.gateKey != "" && key != tt.gateKey {
					status = http.StatusUnauthorized
				}
				mu.Lock()
				gotKey = key
				gotHeader = key != ""
				routerStatus = status
				mu.Unlock()
				callCount.Add(1)
				w.WriteHeader(status)
				if status != http.StatusOK {
					_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
				}
			}))
			defer router.Close()

			info := protocol.WorkerInfo{
				ID:   "key-worker",
				IP:   "127.0.0.1",
				Port: 8086,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go RegisterLoop(ctx, router.URL, info, 50*time.Millisecond, testLogger(), tt.sentKey)

			// Wait for at least one call to land and settle.
			waitFor(t, 2*time.Second, func() bool {
				return callCount.Load() >= 2
			})
			cancel()
			time.Sleep(50 * time.Millisecond)

			if callCount.Load() == 0 {
				t.Fatal("expected at least one registration call")
			}

			mu.Lock()
			got := gotKey
			headerSent := gotHeader
			status := routerStatus
			calls := callCount.Load()
			mu.Unlock()

			if headerSent != tt.wantHeader {
				t.Errorf("X-API-Key header presence = %v, want %v (sent %q)", headerSent, tt.wantHeader, tt.sentKey)
			}
			if tt.wantHeader && got != tt.sentKey {
				t.Errorf("X-API-Key = %q, want %q", got, tt.sentKey)
			}
			if status != tt.wantStatus {
				t.Errorf("router status = %d, want %d", status, tt.wantStatus)
			}
			// The wrong-key case additionally asserts the worker keeps
			// retrying (calls >= 2) rather than crashing — the loop must
			// not terminate on a 401.
			if calls < 2 {
				t.Errorf("expected at least 2 registration calls, got %d", calls)
			}
		})
	}
}
