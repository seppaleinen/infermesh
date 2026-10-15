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
