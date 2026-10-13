package worker

import (
	"testing"
	"time"
)

// TestRecordFailure documents the current circuit-breaker failure-recording
// behaviour (issue #103 R6). Each call increments failureCount by exactly 1;
// the breaker trips to CircuitOpen once failureCount >= failureThreshold.
//
// Known limitation: because RecordFailure is invoked on every retried attempt
// inside retryWithBackoff, a single client request with maxRetries=3 can trip
// the breaker after ~2 real failures. This test documents the observed
// behaviour rather than asserting a "one failure = one trip" contract; fixing
// that is tracked as a separate issue.
func TestRecordFailure(t *testing.T) {
	cb := NewCircuitBreaker(5, 30*time.Second)
	if got := cb.failureCount; got != 0 {
		t.Fatalf("initial failureCount = %d, want 0", got)
	}
	if got := cb.GetState(); got != CircuitClosed {
		t.Fatalf("initial state = %s, want %s", got, CircuitClosed)
	}

	for i := 1; i < 5; i++ {
		cb.RecordFailure()
		if got := cb.failureCount; got != i {
			t.Errorf("after %d RecordFailure calls: failureCount = %d, want %d", i, got, i)
		}
		if got := cb.GetState(); got != CircuitClosed {
			t.Errorf("after %d failures (below threshold 5): state = %s, want %s", i, got, CircuitClosed)
		}
	}

	// The 5th failure trips the breaker.
	cb.RecordFailure()
	if got := cb.failureCount; got != 5 {
		t.Errorf("after 5 failures: failureCount = %d, want 5", got)
	}
	if got := cb.GetState(); got != CircuitOpen {
		t.Errorf("after threshold reached: state = %s, want %s", got, CircuitOpen)
	}

	// Additional failures keep the breaker open but keep counting.
	cb.RecordFailure()
	if got := cb.failureCount; got != 6 {
		t.Errorf("after 6 failures: failureCount = %d, want 6", got)
	}
	if got := cb.GetState(); got != CircuitOpen {
		t.Errorf("state after over-threshold failure = %s, want %s", got, CircuitOpen)
	}
}

// TestRecordFailureZeroThreshold documents the edge case where the threshold
// is 0: the very first failure trips the breaker immediately.
func TestRecordFailureZeroThreshold(t *testing.T) {
	cb := NewCircuitBreaker(0, 30*time.Second)
	cb.RecordFailure()
	if got := cb.GetState(); got != CircuitOpen {
		t.Errorf("state with threshold 0 after 1 failure = %s, want %s", got, CircuitOpen)
	}
}

// TestCanExecute documents the CanExecute state machine (issue #103 R6 fix).
//
//   - CircuitClosed -> true.
//   - CircuitOpen while cooldown has NOT elapsed -> false (fail fast).
//   - CircuitOpen after cooldown HAS elapsed -> transitions to CircuitHalfOpen
//     and returns true (one probe request allowed).
//   - CircuitHalfOpen -> true (probe request in flight).
//
// A successful probe (RecordSuccess in half-open) closes the breaker; a
// failed probe (RecordFailure in half-open) re-opens it.
func TestCanExecute(t *testing.T) {
	cooldown := 50 * time.Millisecond

	t.Run("closed allows execution", func(t *testing.T) {
		cb := NewCircuitBreaker(5, cooldown)
		if !cb.CanExecute() {
			t.Error("CanExecute() = false in CircuitClosed, want true")
		}
	})

	t.Run("open blocks execution before cooldown", func(t *testing.T) {
		cb := NewCircuitBreaker(1, cooldown)
		cb.RecordFailure() // trips to CircuitOpen
		if cb.GetState() != CircuitOpen {
			t.Fatalf("precondition: state = %s, want %s", cb.GetState(), CircuitOpen)
		}
		if got := cb.CanExecute(); got {
			t.Error("CanExecute() = true while open and within cooldown, want false")
		}
	})

	t.Run("open transitions to half-open after cooldown", func(t *testing.T) {
		cb := NewCircuitBreaker(1, cooldown)
		cb.RecordFailure() // trips to CircuitOpen
		if cb.GetState() != CircuitOpen {
			t.Fatalf("precondition: state = %s, want %s", cb.GetState(), CircuitOpen)
		}
		time.Sleep(cooldown + 10*time.Millisecond)
		if !cb.CanExecute() {
			t.Error("CanExecute() = false after cooldown, want true (probe request allowed)")
		}
		if cb.GetState() != CircuitHalfOpen {
			t.Errorf("state after cooldown probe = %s, want %s", cb.GetState(), CircuitHalfOpen)
		}
	})

	t.Run("half-open allows execution", func(t *testing.T) {
		// CircuitHalfOpen is reachable via the cooldown transition above;
		// assert the branch directly so the switch is exercised.
		cb := NewCircuitBreaker(5, cooldown)
		cb.state = CircuitHalfOpen
		if got := cb.CanExecute(); !got {
			t.Error("CanExecute() = false in CircuitHalfOpen, want true")
		}
	})

	t.Run("successful probe in half-open closes the breaker", func(t *testing.T) {
		cb := NewCircuitBreaker(1, cooldown)
		cb.RecordFailure() // trips to CircuitOpen
		time.Sleep(cooldown + 10*time.Millisecond)
		_ = cb.CanExecute() // transitions to half-open
		if cb.GetState() != CircuitHalfOpen {
			t.Fatalf("precondition: state = %s, want %s", cb.GetState(), CircuitHalfOpen)
		}
		cb.RecordSuccess()
		if cb.GetState() != CircuitClosed {
			t.Errorf("state after successful probe = %s, want %s", cb.GetState(), CircuitClosed)
		}
		if cb.failureCount != 0 {
			t.Errorf("failureCount after success = %d, want 0", cb.failureCount)
		}
	})

	t.Run("failed probe in half-open re-opens the breaker", func(t *testing.T) {
		cb := NewCircuitBreaker(1, cooldown)
		cb.RecordFailure() // trips to CircuitOpen
		time.Sleep(cooldown + 10*time.Millisecond)
		_ = cb.CanExecute() // transitions to half-open
		if cb.GetState() != CircuitHalfOpen {
			t.Fatalf("precondition: state = %s, want %s", cb.GetState(), CircuitHalfOpen)
		}
		cb.RecordFailure()
		if cb.GetState() != CircuitOpen {
			t.Errorf("state after failed probe = %s, want %s", cb.GetState(), CircuitOpen)
		}
	})
}

// TestGetMetrics documents that GetMetrics is a stub returning an empty
// Metrics value (issue #103). It is not wired to any real collection path;
// callers must not interpret the returned zero value as "no metrics".
func TestGetMetrics(t *testing.T) {
	b := NewOpenAICompatibleBackend("http://127.0.0.1:1234", "")
	m, err := b.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics() returned error: %v", err)
	}
	if m != (Metrics{}) {
		t.Errorf("GetMetrics() = %+v, want zero Metrics{}", m)
	}
}

// TestCircuitBreakerStateString documents the CircuitState.String() mapping
// used by logging and the queue-stats projection.
func TestCircuitBreakerStateString(t *testing.T) {
	cases := []struct {
		state CircuitState
		want  string
	}{
		{CircuitClosed, "closed"},
		{CircuitHalfOpen, "half-open"},
		{CircuitOpen, "open"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}
