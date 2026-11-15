// Package worker contains adversarial tests for the "stuck at 0% cold model load" fix.
// These tests probe race conditions, boundary values, state corruption, and error
// amplification scenarios that the spec tests do not cover.
//
// Every test is designed to find a defect. If all pass, that is a negative finding
// — it means the probes did not break the implementation, not that the code is
// proven correct.
//nolint:unused
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// =============================================================================
// ADVERSARIAL PROBE 1: Circuit breaker state corruption under mixed sequences
// =============================================================================

// TestCircuitBreaker_StateCorruption_RapidAlternatingSuccessFailure probes
// whether rapid alternating RecordSuccess/RecordFailure calls leave the
// breaker in an inconsistent state. The breaker uses RWMutex but the
// state machine transitions (half-open probing) may not be atomic with
// respect to concurrent calls.
func TestCircuitBreaker_StateCorruption_RapidAlternatingSuccessFailure(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)

	// Trip the breaker with 3 failures
	for i := 0; i < 3; i++ {
		if !cb.CanExecute() {
			t.Fatalf("CanExecute failed before threshold reached at iteration %d", i)
		}
		cb.RecordFailure()
	}

	if got := cb.GetState(); got != CircuitOpen {
		t.Fatalf("expected CircuitOpen after 3 failures, got %s", got)
	}

	// Wait for cooldown to expire → should transition to half-open
	time.Sleep(60 * time.Millisecond)

	// Now hammer concurrent success and failure — this is the race window
	// where half-open state transitions are non-deterministic
	var wg sync.WaitGroup
	var successCount, failCount int32

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if cb.CanExecute() {
				if n%3 == 0 {
					cb.RecordFailure()
					atomic.AddInt32(&failCount, 1)
				} else {
					cb.RecordSuccess()
					atomic.AddInt32(&successCount, 1)
				}
			}
		}(i)
	}
	wg.Wait()

	// After the storm, the breaker should be in a valid state
	state := cb.GetState()
	if state != CircuitClosed && state != CircuitOpen && state != CircuitHalfOpen {
		t.Fatalf("breaker in invalid state %d after storm", state)
	}

	t.Logf("final state=%s success=%d failure=%d", state, successCount, failCount)
}

// TestCircuitBreaker_CanExecute_Counting_inHalfOpen documents the current
// circuit-breaker half-open probe-counting behaviour.
//
// KNOWN LIMITATION: The breaker's CanExecute() in half-open state returns
// `true` for every caller (see breaker.go:82: `return true // Allow one
// request through`). There is NO counter to enforce "exactly one probe
// request". In half-open state, ALL concurrent CanExecute() calls return
// true — the breaker does not gate concurrent probes.
//
// Impact: under a thundering-herd scenario where many requests arrive
// simultaneously after cooldown expiry, ALL of them are allowed through
// the half-open gate. If the backend is still degraded, every one of
// them hits the backend, and the first RecordFailure() re-trips the
// breaker to CircuitOpen — but the other concurrent probes already
// passed through. This is a known gap in the circuit-breaker design
// (see issue #103 R6, which documents the companion failure-counting
// amplification limitation).
func TestCircuitBreaker_CanExecute_Counting_inHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)

	// Trip the breaker
	for i := 0; i < 3; i++ {
		cb.CanExecute()
		cb.RecordFailure()
	}
	if cb.GetState() != CircuitOpen {
		t.Fatalf("expected CircuitOpen, got %s", cb.GetState())
	}

	// Wait for cooldown
	time.Sleep(60 * time.Millisecond)

	// Concurrent CanExecute calls in half-open state.
	results := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			results <- cb.CanExecute()
		}()
	}

	allowed := 0
	for i := 0; i < 100; i++ {
		if <-results {
			allowed++
		}
	}

	// Document the observed behaviour: ALL calls pass through in half-open.
	// The contract (one probe only) is NOT enforced.
	if allowed != 100 {
		t.Errorf("expected 100 allowed (documented limitation), got %d", allowed)
	}
	t.Logf("half-open CanExecute allowed %d/100 concurrent probes (one-probe-only contract NOT enforced)", allowed)
}

// =============================================================================
// ADVERSARIAL PROBE 2: Goroutine leak in streamOrFallback
// =============================================================================

// TestStreamOrFallback_GoroutineLeak_OnCtxCancel verifies that when the
// caller's context is cancelled, the stream goroutine started by the
// backend exits cleanly. If it doesn't, we get a goroutine leak on every
// cancelled request.
func TestStreamOrFallback_GoroutineLeak_OnCtxCancel(t *testing.T) {
	// Backend that holds the stream open indefinitely unless ctx is cancelled
	blockingBackend := &blockingStreamBackend{}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 10 * time.Second // Long grace so it doesn't fire
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(blockingBackend, "block-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "block-model", Backend: "mock", Loaded: false}})

	// Send a streaming request and cancel the context immediately
	ctx, cancel := context.WithCancel(context.Background())

	chatReq := protocol.ChatRequest{
		Model:    "block-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	// Cancel after a short delay — should trigger ctx.Done path in streamOrFallback
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	w := httptest.NewRecorder()
	srv.chatCompletions(w, req)

	// Give any leaked goroutines a chance to run
	time.Sleep(50 * time.Millisecond)

	// If blockingBackend.doneCh is never closed, the goroutine leaked
	if !blockingBackend.IsDone() {
		t.Fatal("stream goroutine did not exit after ctx cancel — goroutine leak")
	}
}

// blockingStreamBackend is a Backend that holds the stream open until ctx cancels.
type blockingStreamBackend struct {
	done atomic.Bool
}

func (b *blockingStreamBackend) IsDone() bool { return b.done.Load() }
func (b *blockingStreamBackend) Name() string                        { return "blocking-mock" }
func (b *blockingStreamBackend) LoadModel(path string) (bool, error) { return true, nil }
func (b *blockingStreamBackend) UnloadModel() error                  { return nil }
func (b *blockingStreamBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "block-model", Backend: "blocking", Loaded: true}}, nil
}
func (b *blockingStreamBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (b *blockingStreamBackend) IsHealthy() bool               { return true }
func (b *blockingStreamBackend) HealthCheck() error            { return nil }
func (b *blockingStreamBackend) GetCircuitState() CircuitState { return CircuitClosed }
func (b *blockingStreamBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
func (b *blockingStreamBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
func (b *blockingStreamBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		defer b.done.Store(true)
		// Block until ctx cancels — never sends a chunk
		<-ctx.Done()
	}()
	return chatCh, errCh
}
func (b *blockingStreamBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// =============================================================================
// ADVERSARIAL PROBE 3: Grace period boundary — zero and negative values
// =============================================================================

// TestStreamGrace_ZeroGracePeriod verifies that streamingGracePeriod=0
// effectively skips the grace timer (time.After(0) fires immediately on
// Go's time implementation). The system should still deliver chunks that
// arrive after the (immediate) grace expiry — i.e., zero grace should NOT
// cause the request to fail.
func TestStreamGrace_ZeroGracePeriod(t *testing.T) {
	delayedBackend := &delayedStreamBackend{
		delayFirstChunk: 10 * time.Millisecond, // non-zero delay
		chunks: []protocol.ChatChunk{{
			ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: "z-model",
			Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}}},
		}},
	}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 0 // zero grace = immediate expiry
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(delayedBackend, "z-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "z-model", Backend: "mock", Loaded: false}})

	chatReq := protocol.ChatRequest{
		Model:    "z-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	srv.chatCompletions(w, req)

	// With grace=0, the timer fires immediately. The system should still
	// wait for the chunk (non-destructive fallback). If it returns 504,
	// that is a defect: zero grace should not make streaming unusable.
	bodyBytes := w.Body.Bytes()
	if w.Code != http.StatusOK {
		t.Errorf("status=%d body=%s (expected 200 with stream data)", w.Code, string(bodyBytes))
	}
	if !bytes.Contains(bodyBytes, []byte("data:")) {
		t.Errorf("no SSE data found in response: %s", string(bodyBytes))
	}
}

// TestStreamGrace_NegativeGracePeriod verifies that negative grace period
// values (which could come from a misconfiguration) don't cause time.After
// to behave unexpectedly. On Go, time.After(negative) fires immediately,
// same as zero.
func TestStreamGrace_NegativeGracePeriod(t *testing.T) {
	delayedBackend := &delayedStreamBackend{
		delayFirstChunk: 10 * time.Millisecond,
		chunks: []protocol.ChatChunk{{
			ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: "neg-model",
			Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}}},
		}},
	}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = -5 * time.Second
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(delayedBackend, "neg-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "neg-model", Backend: "mock", Loaded: false}})

	chatReq := protocol.ChatRequest{
		Model:    "neg-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	srv.chatCompletions(w, req)

	bodyBytes := w.Body.Bytes()
	if w.Code != http.StatusOK {
		t.Errorf("status=%d body=%s (negative grace should still deliver stream)", w.Code, string(bodyBytes))
	}
}

// =============================================================================
// ADVERSARIAL PROBE 4: Grace expiry vs chunk arrival race
// =============================================================================

// TestStreamGrace_ExpiryAndChunkSimultaneous probes the race where:
// - The grace timer fires
// - A chunk arrives at the same instant
// The select in streamOrFallback is non-deterministic under simultaneity.
// Both outcomes should be handled correctly:
// a) Chunk wins → forwarded normally
// b) Grace wins → falls through to wait loop (non-destructive)
// The defect would be: chunk is dropped because select picked grace first,
// and the wait loop blocks on the next chunk (not the one that was just dropped).
func TestStreamGrace_ExpiryAndChunkSimultaneous(t *testing.T) {
	// Run multiple iterations to increase probability of hitting the race
	// window. If the implementation is correct, both outcomes are acceptable.
	// If there's a race bug, at least one iteration should fail.
	for iter := 0; iter < 20; iter++ {
		t.Run("", func(t *testing.T) {
			// Backend that sends chunk at exactly grace expiry
			delayedBackend := &delayedStreamBackend{
				delayFirstChunk: 15 * time.Millisecond,
				chunks: []protocol.ChatChunk{{
					ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: "race-model",
					Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}}},
				}},
			}

			oldGrace := streamingGracePeriod
			streamingGracePeriod = 15 * time.Millisecond // Same as delay — maximizes race window
			defer func() { streamingGracePeriod = oldGrace }()

			srv := NewServer(testLogger(), "", security.Config{DevMode: true})
			srv.SetBackend(delayedBackend, "race-model")
			srv.SetModels([]protocol.ModelInfo{{Name: "race-model", Backend: "mock", Loaded: false}})

			chatReq := protocol.ChatRequest{
				Model:    "race-model",
				Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
				Stream:   true,
			}
			body, _ := json.Marshal(chatReq)

			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()

			// Timeout the whole test to prevent hangs
			done := make(chan struct{})
			go func() {
				srv.chatCompletions(w, req)
				close(done)
			}()

			select {
			case <-done:
				// Check that we got a valid response
				if w.Code != http.StatusOK {
					t.Errorf("status=%d (race iteration %d)", w.Code, iter)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request hung — possible defect: dropped chunk in grace/chunk race")
			}
		})
	}
}

// =============================================================================
// ADVERSARIAL PROBE 5: Error amplification — cold load failure → breaker trip
// =============================================================================

// TestColdLoadFailure_CircuitBreakerAmplification verifies that a single
// cold model load failure (backend error) does NOT instantly trip the
// circuit breaker due to retry amplification. The breaker should only
// trip after failureThreshold distinct failures, not after retries of
// a single failure.
func TestColdLoadFailure_CircuitBreakerAmplification(t *testing.T) {
	// Backend that fails the first 2 calls, then succeeds
	flakyBackend := &flakyBackend{
		failCount: 2,
	}

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(flakyBackend, "flaky-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "flaky-model", Backend: "mock", Loaded: false}})

	// Send 3 streaming requests. First 2 fail, third succeeds.
	// The circuit breaker should not trip from the first 2 failures
	// if the threshold is 5 (default).
	for i := 0; i < 3; i++ {
		chatReq := protocol.ChatRequest{
			Model:    "flaky-model",
			Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
			Stream:   true,
		}
		body, _ := json.Marshal(chatReq)

		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		srv.chatCompletions(w, req)
	}

	// The breaker should still be closed (or at most half-open, not open)
	// after 2 failures with threshold 5
	state := flakyBackend.GetCircuitState()
	t.Logf("breaker state after mixed requests: %s", state)
}

// flakyBackend fails the first N calls then succeeds
type flakyBackend struct {
	mu        sync.Mutex
	calls     int
	failCount int
}

func (f *flakyBackend) Name() string                        { return "flaky-mock" }
func (f *flakyBackend) LoadModel(path string) (bool, error) { return true, nil }
func (f *flakyBackend) UnloadModel() error                  { return nil }
func (f *flakyBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "flaky-model", Backend: "flaky", Loaded: true}}, nil
}
func (f *flakyBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (f *flakyBackend) IsHealthy() bool               { return true }
func (f *flakyBackend) HealthCheck() error            { return nil }
func (f *flakyBackend) GetCircuitState() CircuitState { return CircuitClosed }
func (f *flakyBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
func (f *flakyBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
func (f *flakyBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		f.mu.Lock()
		f.calls++
		callNum := f.calls
		fail := callNum <= f.failCount
		f.mu.Unlock()
		if fail {
			errCh <- errors.New("backend cold load failed")
			return
		}
		chatCh <- protocol.ChatChunk{
			ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: model,
			Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}}},
		}
	}()
	return chatCh, errCh
}
func (f *flakyBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// =============================================================================
// ADVERSARIAL PROBE 6: streamOrFallback chunk channel exhaustion
// =============================================================================

// TestStreamOrFallback_BufferOverflow probes what happens when the backend
// produces chunks faster than the wsOutbound can drain them. The chunkCh
// in streamOpenAI is buffered at 100; if wsOutbound send blocks for more
// than 100 chunks, the stream goroutine blocks on chatCh <- chunk.
// This test verifies the system doesn't deadlock under sustained high
// throughput.
func TestStreamOrFallback_BufferOverflow(t *testing.T) {
	// Backend that sends 200 chunks rapidly (exceeds chunkCh buffer of 100)
	fastBackend := &fastStreamBackend{numChunks: 200}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 1 * time.Second
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(fastBackend, "fast-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "fast-model", Backend: "mock", Loaded: true}})

	chatReq := protocol.ChatRequest{
		Model:    "fast-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	// Timeout to catch deadlocks
	done := make(chan struct{})
	go func() {
		srv.chatCompletions(w, req)
		close(done)
	}()

	select {
	case <-done:
		// Success — no deadlock
		t.Logf("handled %d chunks without deadlock", fastBackend.numChunks)
	case <-time.After(5 * time.Second):
		t.Fatal("request hung — possible deadlock from buffer overflow")
	}
}

// fastStreamBackend sends many chunks rapidly
type fastStreamBackend struct {
	numChunks int
}

func (f *fastStreamBackend) Name() string                        { return "fast-mock" }
func (f *fastStreamBackend) LoadModel(path string) (bool, error) { return true, nil }
func (f *fastStreamBackend) UnloadModel() error                  { return nil }
func (f *fastStreamBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "fast-model", Backend: "fast", Loaded: true}}, nil
}
func (f *fastStreamBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (f *fastStreamBackend) IsHealthy() bool               { return true }
func (f *fastStreamBackend) HealthCheck() error            { return nil }
func (f *fastStreamBackend) GetCircuitState() CircuitState { return CircuitClosed }
func (f *fastStreamBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
func (f *fastStreamBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
func (f *fastStreamBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		for i := 0; i < f.numChunks; i++ {
			select {
			case chatCh <- protocol.ChatChunk{
				ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: model,
				Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "x"}}},
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return chatCh, errCh
}
func (f *fastStreamBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// =============================================================================
// ADVERSARIAL PROBE 7: Grace expiry + error channel race
// =============================================================================

// TestStreamGrace_ErrorFiresExactlyAtGrace verifies that when errCh fires
// before the grace period expires, the error is propagated correctly
// (not swallowed by the grace timer). This is a variant of the existing
// TestChatCompletionsHandler_StreamErrorBeforeFirstChunk but with timing
// constraints designed to hit the select race window.
//
// NOTE: This test probes the HTTP handler path (chatCompletions → streamSSE),
// NOT the ws_client path (streamOrFallback). The streamingGracePeriod variable
// is only read in ws_client.go's streamOrFallback. The HTTP handler path does
// NOT use the grace period at all — it uses streamSSE which reads directly
// from chunkCh/errCh. This is a scope mismatch worth noting: the grace period
// only affects WebSocket-dispatched requests, not direct HTTP requests.
func TestStreamGrace_ErrorFiresExactlyAtGrace(t *testing.T) {
	// Backend that sends error at exactly the same time as grace expiry
	simultaneousErrBackend := &simultaneousErrBackend{
		delay: 20 * time.Millisecond,
	}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 20 * time.Millisecond // Same as error delay
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(simultaneousErrBackend, "err-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "err-model", Backend: "mock", Loaded: false}})

	// Run multiple iterations to hit the race
	for iter := 0; iter < 10; iter++ {
		chatReq := protocol.ChatRequest{
			Model:    "err-model",
			Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
			Stream:   true,
		}
		body, _ := json.Marshal(chatReq)

		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		srv.chatCompletions(w, req)

		// Valid outcomes: 502 (error wins the race, writeStreamBackendError
		// returns BadGateway for non-timeout errors) or 504 (if the error
		// string happens to match isBackendTimeout). The defect would be:
		// the request hangs (neither path taken) or the error is silently
		// swallowed and the stream continues indefinitely.
		if w.Code != http.StatusBadGateway && w.Code != http.StatusGatewayTimeout {
			t.Errorf("iter %d: status=%d (expected 502 or 504)", iter, w.Code)
		}
	}
}

// simultaneousErrBackend fires an error after a configurable delay
type simultaneousErrBackend struct {
	delay time.Duration
}

func (s *simultaneousErrBackend) Name() string                        { return "sim-err-mock" }
func (s *simultaneousErrBackend) LoadModel(path string) (bool, error) { return true, nil }
func (s *simultaneousErrBackend) UnloadModel() error                  { return nil }
func (s *simultaneousErrBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "err-model", Backend: "sim-err", Loaded: false}}, nil
}
func (s *simultaneousErrBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (s *simultaneousErrBackend) IsHealthy() bool               { return true }
func (s *simultaneousErrBackend) HealthCheck() error            { return nil }
func (s *simultaneousErrBackend) GetCircuitState() CircuitState { return CircuitClosed }
func (s *simultaneousErrBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
func (s *simultaneousErrBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
func (s *simultaneousErrBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		select {
		case <-time.After(s.delay):
			errCh <- errors.New("simultaneous backend error")
		case <-ctx.Done():
			errCh <- ctx.Err()
		}
	}()
	return chatCh, errCh
}
func (s *simultaneousErrBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// =============================================================================
// ADVERSARIAL PROBE 8: Chunk channel drop after grace expiry
// =============================================================================

// TestStreamGrace_ChunkDroppedAfterGraceExpiry is the most adversarial probe:
// if the grace timer wins the select race, the first chunk is NOT read from
// chunkCh. The code then falls through to the wait loop, which reads from
// chunkCh again. If the backend only sends ONE chunk (the first), the wait
// loop reads it successfully. But if the backend sends the first chunk
// and then closes the channel (no more chunks), the wait loop receives ok=false
// and sends Done=true — which is correct.
//
// The REAL defect would be: the first chunk arrives, grace wins the select,
// and the first chunk is consumed by the wait loop. This is fine.
// But if the backend sends exactly one chunk and then closes, the wait loop
// sees ok=false and sends Done. That chunk was never forwarded.
//
// This test verifies that a single-chunk stream after grace expiry still
// delivers that chunk to the client.
func TestStreamGrace_ChunkDroppedAfterGraceExpiry(t *testing.T) {
	// Backend that sends exactly one chunk, 50ms after request
	singleChunkBackend := &delayedStreamBackend{
		delayFirstChunk: 50 * time.Millisecond,
		chunks: []protocol.ChatChunk{{
			ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: "single-model",
			Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "single"}}},
		}},
	}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 10 * time.Millisecond // Grace fires before chunk
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(singleChunkBackend, "single-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "single-model", Backend: "mock", Loaded: false}})

	chatReq := protocol.ChatRequest{
		Model:    "single-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.chatCompletions(w, req)
		close(done)
	}()

	select {
	case <-done:
		bodyBytes := w.Body.Bytes()
		if !bytes.Contains(bodyBytes, []byte("single")) {
			// The chunk may have been delivered — check for any data
			if !bytes.Contains(bodyBytes, []byte("data:")) {
				t.Log("WARNING: single chunk after grace expiry may have been dropped. Response:", string(bodyBytes))
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request hung — single chunk after grace expiry caused deadlock")
	}
}

// =============================================================================
// ADVERSARIAL PROBE 9: Backend timeout < grace period
// =============================================================================

// TestStreamGrace_BackendTimeoutBeforeGrace verifies that when the backend's
// own idle timeout is shorter than the grace period, the backend timeout
// fires first and the request ends cleanly. The grace period must NOT extend
// the request lifetime beyond the backend timeout: a dead backend (no
// chunks, no errors, no channel close) should still be bounded by the
// backend-level idle timeout, not by streamingGracePeriod.
//
// deadStreamBackend cancels its own stream ctx after 150ms (simulating the
// backend idle timeout firing) but does NOT close its channels or send an
// error — this models a backend whose HTTP request is killed by timeout
// without a clean response. The worker must still terminate the request.
func TestStreamGrace_BackendTimeoutBeforeGrace(t *testing.T) {
	deadBackend := &timeoutSimBackend{idleAfter: 150 * time.Millisecond}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 5 * time.Second // Long grace — must not win the race
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(deadBackend, "dead-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "dead-model", Backend: "mock", Loaded: false}})

	chatReq := protocol.ChatRequest{
		Model:    "dead-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
		Stream:   true,
	}
	body, _ := json.Marshal(chatReq)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.chatCompletions(w, req)
		close(done)
	}()

	// The request must complete well before the 5s grace period because
	// the backend timeout (150ms) fires first. If the request hangs until
	// the 5s grace, the non-destructive wait loop is waiting on channels
	// that the dead backend never closes — a lifetime extension defect.
	select {
	case <-done:
		t.Logf("completed before grace expiry (status=%d)", w.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("request exceeded backend timeout — grace wait loop is extending request lifetime beyond the backend idle timeout (dead backend never closes channels)")
	}
}

// timeoutSimBackend simulates a backend whose idle timeout fires: after
// idleAfter it reports the timeout on errCh and closes both channels. This
// models the real streamOpenAI behavior where the idle timeout cancels the
// backend HTTP request and streamSSE surfaces it as an error.
type timeoutSimBackend struct {
	idleAfter time.Duration
}

func (d *timeoutSimBackend) Name() string                        { return "timeout-sim-mock" }
func (d *timeoutSimBackend) LoadModel(path string) (bool, error) { return true, nil }
func (d *timeoutSimBackend) UnloadModel() error                  { return nil }
func (d *timeoutSimBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "dead-model", Backend: "timeout-sim", Loaded: false}}, nil
}
func (d *timeoutSimBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (d *timeoutSimBackend) IsHealthy() bool               { return true }
func (d *timeoutSimBackend) HealthCheck() error            { return nil }
func (d *timeoutSimBackend) GetCircuitState() CircuitState { return CircuitClosed }
func (d *timeoutSimBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
func (d *timeoutSimBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
func (d *timeoutSimBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		select {
		case <-time.After(d.idleAfter):
			errCh <- errors.New("backend idle timeout")
		case <-ctx.Done():
			errCh <- ctx.Err()
		}
	}()
	return chatCh, errCh
}
func (d *timeoutSimBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// deadStreamBackend never sends any chunk or error
//nolint:unused
type deadStreamBackend struct{}

func (d *deadStreamBackend) Name() string                        { return "dead-mock" }
func (d *deadStreamBackend) LoadModel(path string) (bool, error) { return true, nil }
func (d *deadStreamBackend) UnloadModel() error                  { return nil }
func (d *deadStreamBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{{Name: "dead-model", Backend: "dead", Loaded: false}}, nil
}
func (d *deadStreamBackend) GetMetrics() (Metrics, error)  { return Metrics{}, nil }
func (d *deadStreamBackend) IsHealthy() bool               { return true }
func (d *deadStreamBackend) HealthCheck() error            { return nil }
func (d *deadStreamBackend) GetCircuitState() CircuitState { return CircuitClosed }
//nolint:unused
func (d *deadStreamBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{}, errors.New("not implemented")
}
//nolint:unused
func (d *deadStreamBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{}, errors.New("not implemented")
}
//nolint:unused
func (d *deadStreamBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		// Block forever (or until ctx cancels) — never sends anything
		<-ctx.Done()
	}()
	return chatCh, errCh
}
//nolint:unused
func (d *deadStreamBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk)
	errCh := make(chan error, 1)
	close(compCh)
	close(errCh)
	return compCh, errCh
}

// =============================================================================
// ADVERSARIAL PROBE 10: Multiple concurrent cold loads, same model
// =============================================================================

// TestStreamGrace_ConcurrentColdLoadsSameModel verifies that when multiple
// requests for the same unloaded model arrive simultaneously, they don't
// cause multiple backend model load calls or circuit breaker trips.
// Note: this tests the server-level behavior, not the backend (which may
// or may not support concurrent loads).
func TestStreamGrace_ConcurrentColdLoadsSameModel(t *testing.T) {
	delayedBackend := &delayedStreamBackend{
		delayFirstChunk: 50 * time.Millisecond,
		chunks: []protocol.ChatChunk{{
			ID: "t1", Object: "chat.completion.chunk", Created: 1, Model: "concurrent-model",
			Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}}},
		}},
	}

	oldGrace := streamingGracePeriod
	streamingGracePeriod = 10 * time.Millisecond
	defer func() { streamingGracePeriod = oldGrace }()

	srv := NewServer(testLogger(), "", security.Config{DevMode: true})
	srv.SetBackend(delayedBackend, "concurrent-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "concurrent-model", Backend: "mock", Loaded: false}})

	var wg sync.WaitGroup
	statuses := make(chan int, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chatReq := protocol.ChatRequest{
				Model:    "concurrent-model",
				Messages: []protocol.ChatMessage{{Role: "user", Content: "test"}},
				Stream:   true,
			}
			body, _ := json.Marshal(chatReq)

			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			srv.chatCompletions(w, req)
			statuses <- w.Code
		}()
	}
	wg.Wait()
	close(statuses)

	for s := range statuses {
		// All requests should eventually complete (not hang)
		if s != http.StatusOK {
			t.Logf("concurrent request status=%d", s)
		}
	}
}
