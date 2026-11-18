// Package worker contains adversarial tests for the per-model concurrency
// estimator — the component that tracks 429s/latency per model and adjusts
// concurrency caps. These tests probe race conditions, boundary values,
// state corruption, persistence corruption, and resource leaks.
//
// Every test is designed to find a defect. If all pass, that is a negative
// finding — it means the probes did not break the implementation, not that
// the code is proven correct.
package worker

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// PROBE 1: Concurrent RecordRequest for different models triggers recompute
// while estimators map is growing — data race on estimators map
// =============================================================================

func TestPerModelEstimator_ConcurrentRecordRequestsRace(t *testing.T) {
	path := "/tmp/infermesh-adversarial-concurrency-race.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Start(ctx)

	const numModels = 20
	const numGoroutines = 50
	var wg sync.WaitGroup
	var panicCount int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt64(&panicCount, 1)
				}
				wg.Done()
			}()
			model := "model-" + string(rune('a'+n%numModels))
			status := 200
			if n%3 == 0 {
				status = 429
			}
			p.RecordRequest(time.Now(), status, model)
		}(i)
	}
	wg.Wait()

	if panicCount > 0 {
		t.Fatalf("panics during concurrent RecordRequest: %d", panicCount)
	}

	for i := 0; i < numModels; i++ {
		model := "model-" + string(rune('a'+i))
		cap := p.Cap(model)
		if cap < minCap {
			t.Errorf("model %s cap=%d below minCap=%d", model, cap, minCap)
		}
	}
}

// =============================================================================
// PROBE 2: RecordRequest + recomputeAll race
// =============================================================================

func TestPerModelEstimator_RecordAndRecomputeRace(t *testing.T) {
	path := "/tmp/infermesh-adversarial-record-recompute-race.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)
	for i := 0; i < 10; i++ {
		p.RecordRequest(time.Now(), 200, "model-race")
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Goroutine 1: record requests
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			p.RecordRequest(time.Now(), 429, "model-race")
		}
	}()

	// Goroutine 2: recomputeAll
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			p.recomputeAll()
		}
	}()

	wg.Wait()
	close(done)
	<-done
	// If we get here without panic, no deadlock occurred
}

// =============================================================================
// PROBE 3: Circuit breaker events fire while estimator is being created
// =============================================================================

func TestPerModelEstimator_CircuitBreakerVsRecordRace(t *testing.T) {
	path := "/tmp/infermesh-adversarial-circuit-vs-record.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)
	const numGoroutines = 30
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			model := "cb-model"
			switch n % 3 {
			case 0:
				p.RecordRequest(time.Now(), 200, model)
			case 1:
				p.RecordCircuitOpen(model)
			default:
				p.RecordCircuitClosed(model)
			}
		}(i)
	}
	wg.Wait()

	// Cap should be in valid range
	cap := p.Cap("cb-model")
	if cap < minCap {
		t.Errorf("cap=%d below minCap=%d after circuit breaker race", cap, minCap)
	}
}

// =============================================================================
// PROBE 4: Stop while timer fires — persistence corruption
// =============================================================================

func TestPerModelEstimator_StopDuringTimerFire(t *testing.T) {
	path := "/tmp/infermesh-adversarial-stop-timer.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)
	// Seed models
	for i := 0; i < 5; i++ {
		p.RecordRequest(time.Now(), 200, "model-"+string(rune('a'+i)))
	}

	// Force schedule persist
	p.schedulePersist()

	// Race Stop with the scheduled persist timer firing
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.Stop()
		close(done)
	}()

	// Also call Stop concurrently
	go p.Stop()

	<-done
	// Verify the persisted file is valid JSON
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persisted file missing after Stop: %v", err)
	}
	var caps map[string]int
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatalf("persisted file is not valid JSON after concurrent Stop: %s", string(data))
	}
}

// =============================================================================
// PROBE 5: Malformed persisted JSON — wrong types, missing keys
// =============================================================================

func TestPerModelEstimator_LoadMalformedPersistedJSON(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantCap  int // expected fallback cap for unknown model
		modelCap int // expected cap for model "test-model" if loaded
		modelOK  bool
	}{
		{
			name:     "valid JSON with int values",
			payload:  `{"test-model": 3, "other-model": 5}`,
			wantCap:  4,
			modelCap: 3,
			modelOK:  true,
		},
		{
			name:    "JSON with float value (should unmarshal to int 0)",
			payload: `{"test-model": 3.7}`,
			wantCap: 4,
			modelOK: false, // will be clamped to minCap=1
		},
		{
			name:    "JSON with string value (unmarshal error)",
			payload: `{"test-model": "three"}`,
			wantCap: 4,
			modelOK: false,
		},
		{
			name:     "JSON with null value (unmarshal to 0 -> clamped to minCap)",
			payload:  `{"test-model": null}`,
			wantCap:  4,
			modelCap: 1, // clamped to minCap
			modelOK:  true,
		},
		{
			name:    "truncated JSON",
			payload: `{"test-model": 3, "other-`,
			wantCap: 4,
			modelOK: false, // unmarshal fails, no models loaded
		},
		{
			name:    "empty object",
			payload: `{}`,
			wantCap: 4,
			modelOK: false,
		},
		{
			name:    "not JSON at all",
			payload: `hello world`,
			wantCap: 4,
			modelOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/tmp/infermesh-adversarial-malformed-" + tt.name + ".json"
			_ = os.Remove(path)
			defer func() { _ = os.Remove(path) }()

			// Write malformed payload
			_ = os.WriteFile(path, []byte(tt.payload), 0644)

			p := NewPerModelConcurrencyEstimator(path, 4)
			// Should not panic on load
			cap := p.Cap("unknown-model")
			if cap != tt.wantCap {
				t.Errorf("unknown model cap=%d, want %d", cap, tt.wantCap)
			}

			if tt.modelOK {
				cap := p.Cap("test-model")
				if tt.modelCap > 0 && cap != tt.modelCap {
					t.Errorf("test-model cap=%d, want %d", cap, tt.modelCap)
				}
				// At minimum, cap should not be negative
				if cap < minCap {
					t.Errorf("test-model cap=%d below minCap=%d", cap, minCap)
				}
			}
		})
	}
}

// =============================================================================
// PROBE 6: Negative cap from persistence
// =============================================================================

func TestPerModelEstimator_NegativePersistedCap(t *testing.T) {
	path := "/tmp/infermesh-adversarial-negative-cap.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	// Write a persisted file with negative value
	_ = os.WriteFile(path, []byte(`{"evil-model": -5}`), 0644)

	p := NewPerModelConcurrencyEstimator(path, 4)

	// loadPersisted should clamp negative to minCap
	cap := p.Cap("evil-model")
	if cap < minCap {
		t.Errorf("negative persisted cap should be clamped to minCap=%d, got %d", minCap, cap)
	}
	if cap != minCap {
		t.Errorf("negative persisted cap should equal minCap=%d, got %d", minCap, cap)
	}
}

// =============================================================================
// PROBE 7: Scheduler edge case — both per-model cap and worker-wide cap are 0
// =============================================================================

func TestPerModelEstimator_BothCapsZero(t *testing.T) {
	path := "/tmp/infermesh-adversarial-zero-cap.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 0) // zero default cap

	// Default cap should be clamped to minCap
	cap := p.Cap("any-model")
	if cap != minCap {
		t.Errorf("zero default cap should be clamped to minCap=%d, got %d", minCap, cap)
	}
}

// =============================================================================
// PROBE 8: Start called multiple times
// =============================================================================

func TestPerModelEstimator_StartCalledMultipleTimes(t *testing.T) {
	path := "/tmp/infermesh-adversarial-double-start.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	ctx3, cancel3 := context.WithCancel(context.Background())

	go p.Start(ctx1)
	go p.Start(ctx2)
	go p.Start(ctx3)

	// Give them a moment to run
	time.Sleep(50 * time.Millisecond)

	// Cancel one context — one of the Start goroutines should exit
	cancel1()
	time.Sleep(50 * time.Millisecond)

	// Cancel the rest
	cancel2()
	cancel3()

	// Stop should not deadlock
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop deadlocked after multiple Start calls")
	}
}

// =============================================================================
// PROBE 9: Memory leak — unbounded estimator growth with ephemeral models
// =============================================================================

func TestPerModelEstimator_UnboundedEstimatorGrowth(t *testing.T) {
	path := "/tmp/infermesh-adversarial-memory-leak.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Simulate many models being discovered (dynamic model discovery)
	// without any cleanup mechanism
	for i := 0; i < 1000; i++ {
		p.RecordRequest(time.Now(), 200, "ephemeral-model-"+string(rune(i%26))+string(rune('A'+(i/26)%26)))
	}

	p.mu.Lock()
	count := len(p.estimators)
	p.mu.Unlock()

	if count != 1000 {
		t.Logf("estimator count=%d (expected 1000 — no cleanup mechanism exists, this is informational)", count)
	}

	// The issue: there is no way to remove estimators for models that are
	// no longer relevant. The map grows unboundedly. This is not a crash
	// but a memory leak over time.
}

// =============================================================================
// PROBE 10: Timer leak — persistTimer not stopped properly
// =============================================================================

func TestPerModelEstimator_PersistTimerLeak(t *testing.T) {
	path := "/tmp/infermesh-adversarial-timer-leak.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Fire many schedulePersist calls to create multiple timers
	for i := 0; i < 50; i++ {
		p.RecordRequest(time.Now(), 200, "model-timer")
		p.RecordCircuitOpen("model-timer")
		p.RecordCircuitClosed("model-timer")
	}

	// Stop should clean up all timers
	p.Stop()

	// Verify file is valid
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persist failed: %v", err)
	}
	var caps map[string]int
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatalf("persisted data corrupted: %s", string(data))
	}
}

// =============================================================================
// PROBE 11: Single estimator — latency array unbounded growth
// =============================================================================

func TestConcurrencyEstimator_LatencyArrayGrowth(t *testing.T) {
	path := "/tmp/infermesh-adversarial-latency-growth.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)

	// Record many requests — latencies slice grows
	for i := 0; i < 500; i++ {
		e.RecordRequest(time.Now().Add(-time.Duration(i)*time.Millisecond), 200)
	}

	// recompute should prune (keeps last 200)
	e.recompute()

	e.mu.Lock()
	latLen := len(e.latencies)
	e.mu.Unlock()

	if latLen > 200 {
		t.Errorf("latencies array not pruned: len=%d (expected <=200)", latLen)
	}
}

// =============================================================================
// PROBE 12: Cap() always returns >= minCap even when internal cap goes negative
// =============================================================================

func TestConcurrencyEstimator_CapNeverBelowMinCap(t *testing.T) {
	path := "/tmp/infermesh-adversarial-cap-negative.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)

	// Force cap to 0 by calling recompute enough times with 429s
	e.mu.Lock()
	e.cap = 1 // at minCap
	e.window429 = append(e.window429, time.Now(), time.Now())
	e.mu.Unlock()

	// recompute should not go below minCap
	e.recompute()

	cap := e.Cap()
	if cap < minCap {
		t.Errorf("cap=%d below minCap=%d after aggressive 429 backoff", cap, minCap)
	}
}

// =============================================================================
// PROBE 13: 429 window time precision — boundary values
// =============================================================================

func TestConcurrencyEstimator_429WindowBoundary(t *testing.T) {
	path := "/tmp/infermesh-adversarial-429-boundary.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)

	// Record exactly 2 429s at the boundary of the window
	now := time.Now()
	// One just inside the window, one just outside
	e.mu.Lock()
	e.window429 = append(e.window429, now.Add(-windowDuration), now.Add(-windowDuration-1))
	e.cap = 4
	e.mu.Unlock()

	e.recompute()

	// Both should be pruned (outside window), so cap should not decrease
	e.mu.Lock()
	remaining429s := len(e.window429)
	e.mu.Unlock()

	if remaining429s != 0 {
		t.Errorf("expected 0 429s after window prune, got %d", remaining429s)
	}
}

// =============================================================================
// PROBE 14: Empty model name — estimator keyed by empty string
// =============================================================================

func TestPerModelEstimator_EmptyModelName(t *testing.T) {
	path := "/tmp/infermesh-adversarial-empty-model.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Record for empty model name
	p.RecordRequest(time.Now(), 200, "")

	cap := p.Cap("")
	if cap < minCap {
		t.Errorf("empty model name cap=%d below minCap", cap)
	}
	// The estimator for "" should exist
	p.mu.Lock()
	_, exists := p.estimators[""]
	p.mu.Unlock()
	if !exists {
		t.Error("expected estimator for empty model name to exist after RecordRequest")
	}
}

// =============================================================================
// PROBE 15: persist() called without holding p.mu in PerModelConcurrencyEstimator
// This is a lock ordering / safety check: the persist method is documented as
// requiring p.mu to be held. But schedulePersist acquires and releases p.mu
// inside its timer callback. If persist is called from Stop (which holds p.mu)
// and the timer fires at the same time, we get a double-lock on p.mu.
// =============================================================================

func TestPerModelEstimator_PersistDoubleLockSafety(t *testing.T) {
	path := "/tmp/infermesh-adversarial-double-lock.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)
	for i := 0; i < 5; i++ {
		p.RecordRequest(time.Now(), 200, "model-safety")
	}

	// Force a scheduled persist timer
	p.schedulePersist()

	// Race Stop (which acquires p.mu and calls persist) with the timer firing
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		p.Stop() // acquires p.mu, calls persist
	}()

	// The timer callback also tries to acquire p.mu and call persist
	// This should not deadlock (timer callback uses Lock/Unlock, not defer)
	select {
	case <-func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			wg.Wait()
			close(ch)
		}()
		return ch
	}():
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: p.Stop() and timer callback contend for p.mu")
	}
}

// =============================================================================
// PROBE 16: RecordCircuitOpen/Close when model has no estimator
// The implementation only acts if the model exists in the estimators map.
// RecordCircuitOpen for an unknown model should be a no-op (not crash).
// =============================================================================

func TestPerModelEstimator_CircuitOpenUnknownModel(t *testing.T) {
	path := "/tmp/infermesh-adversarial-circuit-unknown.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// RecordCircuitOpen for unknown model — should be no-op, not crash
	p.RecordCircuitOpen("unknown-model")
	p.RecordCircuitClosed("unknown-model")

	// Cap should still be default
	cap := p.Cap("unknown-model")
	if cap != 4 {
		t.Errorf("unknown model cap changed from default: got %d, want 4", cap)
	}
}

// =============================================================================
// PROBE 17: RecordRequest with statusCode=0 — what happens?
// The implementation records 429s but ignores other status codes.
// statusCode=0 should not cause a 429 hit.
// =============================================================================

func TestPerModelEstimator_ZeroStatusCodeNot429(t *testing.T) {
	path := "/tmp/infermesh-adversarial-zero-status.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Use exactly 4 requests so the latency-spike path (which needs >= 5
	// samples) does not trigger. This isolates the 429-specific check.
	for i := 0; i < 4; i++ {
		p.RecordRequest(time.Now(), 0, "zero-model")
	}
	p.recomputeAll()

	// Cap should remain unchanged since statusCode=0 is not 429
	cap := p.Cap("zero-model")
	if cap != 4 {
		t.Errorf("zero status code should not reduce cap: got %d, want 4", cap)
	}
}

// =============================================================================
// PROBE 18: Cap() internal cap < minCap guard — the Cap() method checks
// e.cap < minCap and returns minCap. But the internal e.cap field itself
// can go below minCap through recompute. Verify this doesn't cause
// incorrect recompute behavior on subsequent calls.
// =============================================================================

func TestConcurrencyEstimator_InternalCapBelowMinCapRecovery(t *testing.T) {
	path := "/tmp/infermesh-adversarial-internal-below-min.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)

	// Force internal cap to 0 (below minCap) and simulate recovery
	e.mu.Lock()
	e.cap = 0
	e.lastCapChanged = time.Now().Add(-62 * time.Second)
	e.window429 = nil // no recent 429s
	e.mu.Unlock()

	// recompute should recover (slow recovery path)
	e.recompute()

	// Cap() should return at least minCap
	cap := e.Cap()
	if cap < minCap {
		t.Errorf("cap=%d below minCap=%d after recovery", cap, minCap)
	}
}

// =============================================================================
// PROBE 19: Model name mismatch — case sensitivity
// The estimator is keyed by model name string. If a request comes in with
// a different case ("Llama3-8b" vs "llama3-8b"), the estimator creates
// a new entry instead of finding the existing one.
// =============================================================================

func TestPerModelEstimator_CaseSensitivityModelMismatch(t *testing.T) {
	path := "/tmp/infermesh-adversarial-case-mismatch.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Record for "llama3-8b"
	p.RecordRequest(time.Now(), 200, "llama3-8b")
	// Record for "Llama3-8b" (different case)
	p.RecordRequest(time.Now(), 200, "Llama3-8b")
	// Record for "LLAMA3-8B" (yet another case)
	p.RecordRequest(time.Now(), 200, "LLAMA3-8B")

	p.mu.Lock()
	count := len(p.estimators)
	p.mu.Unlock()

	if count != 3 {
		t.Errorf("expected 3 estimators (case-sensitive), got %d — case mismatch creates separate entries", count)
	}

	// Each case variant has its own cap
	for _, name := range []string{"llama3-8b", "Llama3-8b", "LLAMA3-8B"} {
		cap := p.Cap(name)
		if cap != 4 {
			t.Errorf("cap for %q = %d, expected 4 (default)", name, cap)
		}
	}
}

// =============================================================================
// PROBE 20: ConcurrencyEstimator Start called on already-running instance
// The Start method launches a goroutine with a ticker. Calling Start again
// creates another goroutine, leading to duplicate recompute cycles.
// =============================================================================

func TestConcurrencyEstimator_DoubleStart(t *testing.T) {
	path := "/tmp/infermesh-adversarial-estimator-double-start.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)

	// Record some 429s so recompute has something to do (statusCode only, model param for single estimator)
	e.RecordRequest(time.Now(), 429)
	e.RecordRequest(time.Now(), 429)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start twice — creates two goroutines both running the ticker loop
	go e.Start(ctx)
	go e.Start(ctx)

	// Wait for at least one tick
	time.Sleep(100 * time.Millisecond)

	// cancel should stop both goroutines
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Stop should not deadlock
	e.Stop()

	// Verify file is valid
	data, _ := os.ReadFile(path)
	if data != nil {
		var caps map[string]int
		if err := json.Unmarshal(data, &caps); err != nil {
			t.Fatalf("persisted data corrupted: %s", string(data))
		}
	}
}

// =============================================================================
// PROBE 21: Scheduler — InFlight > effectiveMaxInFlight (ratio > 1.0)
// Verify that the scheduler clamps inFlightRatio to 1.0 even when
// InFlight exceeds the effective cap.
// =============================================================================

func TestScheduler_InFlightExceedsCap(t *testing.T) {
	// This test is in the scheduler package — see concurrency_estimator_adversarial_test.go
	// in pkg/scheduler/ for the actual test. Here we just verify the worker side
	// can produce a cap < InFlight scenario.
	path := "/tmp/infermesh-adversarial-cap-exceed.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 1) // minCap

	// Cap should be 1
	if e.Cap() != 1 {
		t.Fatalf("expected cap=1, got %d", e.Cap())
	}

	// The scheduler would compute inFlightRatio = min(inFlight/1, 1.0)
	// If InFlight = 3, ratio = 1.0 (clamped) — this is correct.
	// If InFlight = 0, ratio = 0.0 — also correct.
	// The issue is if cap = 0 and MaxInFlight = 0, the condition
	// "effectiveMaxInFlight > 0" is false and the in-flight component
	// is skipped entirely (neutral). This is the desired fallback.
}

// =============================================================================
// PROBE 22: Persist file permissions and rename race
// =============================================================================

func TestConcurrencyEstimator_PersistFilePermissions(t *testing.T) {
	path := "/tmp/infermesh-adversarial-perm.json"
	_ = os.Remove(path)
	defer func() { _ = os.Remove(path) }()

	e := NewConcurrencyEstimator(path, 4)
	e.mu.Lock()
	e.persist()
	e.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("persist file missing: %v", err)
	}

	// File should have 0644 permissions
	perm := info.Mode().Perm()
	if perm != 0644 {
		t.Errorf("persist file permissions=%o, expected 0644", perm)
	}
}

// =============================================================================
// PROBE 23: Multiple estimators persisting to same directory
// When multiple workers share the same persist path (e.g. /tmp), concurrent
// writes from different processes could corrupt each other's files.
// Within a single process, the estimator uses its own path, so this is
// a process-level (not in-process) concern. We can test the atomic rename.
// =============================================================================

func TestConcurrencyEstimator_PersistAtomicity(t *testing.T) {
	path := "/tmp/infermesh-adversarial-atomic.json"
	_ = os.Remove(path)
	_ = os.Remove(path + ".tmp")
	defer func() {
		_ = os.Remove(path)
		_ = os.Remove(path + ".tmp")
	}()

	e := NewConcurrencyEstimator(path, 7)

	// Verify the .tmp file is renamed to the final path
	e.mu.Lock()
	e.persist()
	e.mu.Unlock()

	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error(".tmp file still exists after persist — rename may have failed")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persist file missing: %v", err)
	}

	// Read back and verify
	cap := e.LoadPersisted(path)
	if cap != 7 {
		t.Errorf("persisted cap=%d, expected 7", cap)
	}
}
