package worker

import (
	"os"
	"testing"
	"time"
)

func TestConcurrencyEstimator_BackoffOn429(t *testing.T) {
	e := NewConcurrencyEstimator("/tmp/infermesh-test-concurrency-429.json")
	defer func() { _ = os.Remove("/tmp/infermesh-test-concurrency-429.json") }()
	if e.Cap() != defaultInitCap {
		t.Fatalf("expected initial cap %d, got %d", defaultInitCap, e.Cap())
	}
	now := time.Now()
	e.RecordRequest(now.Add(-1*time.Second), 429)
	e.RecordRequest(now.Add(-500*time.Millisecond), 429)
	// recompute manually
	e.recompute()
	if e.Cap() >= defaultInitCap {
		t.Fatalf("expected cap to decrease after 429s, got %d", e.Cap())
	}
}

func TestConcurrencyEstimator_RecoverAfterWindow(t *testing.T) {
	e := NewConcurrencyEstimator("/tmp/infermesh-test-concurrency-recover.json")
	defer func() { _ = os.Remove("/tmp/infermesh-test-concurrency-recover.json") }()
	// Force backoff
	e.mu.Lock()
	e.cap = 2
	e.lastCapChanged = time.Now().Add(-61 * time.Second)
	e.mu.Unlock()
	// No 429s in window
	e.recompute()
	if e.Cap() <= 2 {
		t.Fatalf("expected cap to recover, got %d", e.Cap())
	}
}

func TestConcurrencyEstimator_PersistAndLoad(t *testing.T) {
	path := "/tmp/infermesh-test-concurrency-persist.json"
	defer func() { _ = os.Remove(path) }()
	e1 := NewConcurrencyEstimator(path)
	e1.mu.Lock()
	e1.cap = 3
	e1.persist()
	e1.mu.Unlock()
	e2 := NewConcurrencyEstimator(path)
	if e2.Cap() != 3 {
		t.Fatalf("expected loaded cap 3, got %d", e2.Cap())
	}
}

// TestConcurrencyEstimator_InitCapFromThreads verifies that the estimator
// can be seeded with the backend's thread count (issue #139). The seed is
// bounded to ≥ minCap and a persisted cap from a previous run overrides it.
func TestConcurrencyEstimator_InitCapFromThreads(t *testing.T) {
	t.Run("seed with thread count", func(t *testing.T) {
		path := "/tmp/infermesh-test-concurrency-init.json"
		defer func() { _ = os.Remove(path) }()
		e := NewConcurrencyEstimatorWithInit(path, 8)
		if got := e.Cap(); got != 8 {
			t.Errorf("expected initial cap 8, got %d", got)
		}
	})

	t.Run("seed bounded to minCap", func(t *testing.T) {
		path := "/tmp/infermesh-test-concurrency-init-zero.json"
		defer func() { _ = os.Remove(path) }()
		e := NewConcurrencyEstimatorWithInit(path, 0)
		if got := e.Cap(); got != minCap {
			t.Errorf("expected initial cap %d, got %d", minCap, got)
		}
	})

	t.Run("persisted cap overrides seed", func(t *testing.T) {
		path := "/tmp/infermesh-test-concurrency-init-persist.json"
		defer func() { _ = os.Remove(path) }()
		e1 := NewConcurrencyEstimatorWithInit(path, 8)
		e1.mu.Lock()
		e1.cap = 3
		e1.persist()
		e1.mu.Unlock()

		e2 := NewConcurrencyEstimatorWithInit(path, 8)
		if got := e2.Cap(); got != 3 {
			t.Errorf("expected persisted cap 3, got %d", got)
		}
	})
}

func TestConcurrencyEstimator_CircuitOpen(t *testing.T) {
	e := NewConcurrencyEstimator("/tmp/infermesh-test-concurrency-circuit.json")
	defer func() { _ = os.Remove("/tmp/infermesh-test-concurrency-circuit.json") }()
	e.RecordCircuitOpen()
	if e.Cap() != 1 {
		t.Fatalf("expected cap 1 after circuit open, got %d", e.Cap())
	}
	e.RecordCircuitClosed()
	if e.Cap() != defaultInitCap {
		t.Fatalf("expected cap to recover to %d after circuit closed, got %d", defaultInitCap, e.Cap())
	}
}
