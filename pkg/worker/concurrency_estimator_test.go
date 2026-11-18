package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPersistDir creates an in-memory temp directory for persistence tests
// and returns its path. The caller must clean it up with os.RemoveAll.
func testPersistDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "infermesh-test-*")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConcurrencyEstimator_BackoffOn429(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-concurrency-429.json")
	e := NewConcurrencyEstimator(path, defaultInitCap)
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
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-concurrency-recover.json")
	e := NewConcurrencyEstimator(path, defaultInitCap)
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
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-concurrency-persist.json")
	e1 := NewConcurrencyEstimator(path, defaultInitCap)
	e1.mu.Lock()
	e1.cap = 3
	e1.persist()
	e1.mu.Unlock()
	e2 := NewConcurrencyEstimator(path, defaultInitCap)
	if e2.Cap() != 3 {
		t.Fatalf("expected loaded cap 3, got %d", e2.Cap())
	}
}

// TestConcurrencyEstimator_InitCapFromThreads verifies that the estimator
// can be seeded with the backend's thread count (issue #139). The seed is
// bounded to [minCap, maxCap] and a persisted cap from a previous run overrides it.
func TestConcurrencyEstimator_InitCapFromThreads(t *testing.T) {
	t.Run("seed with thread count", func(t *testing.T) {
		dir := testPersistDir(t)
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "test-concurrency-init.json")
		e := NewConcurrencyEstimator(path, 8)
		if got := e.Cap(); got != 8 {
			t.Errorf("expected initial cap 8, got %d", got)
		}
	})

	t.Run("seed bounded to minCap", func(t *testing.T) {
		dir := testPersistDir(t)
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "test-concurrency-init-zero.json")
		e := NewConcurrencyEstimator(path, 0)
		if got := e.Cap(); got != minCap {
			t.Errorf("expected initial cap %d, got %d", minCap, got)
		}
	})

	t.Run("seed bounded to maxCap", func(t *testing.T) {
		dir := testPersistDir(t)
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "test-concurrency-init-max.json")
		e := NewConcurrencyEstimator(path, 2000)
		if got := e.Cap(); got != maxCap {
			t.Errorf("expected initial cap %d, got %d", maxCap, got)
		}
	})

	t.Run("persisted cap overrides seed", func(t *testing.T) {
		dir := testPersistDir(t)
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "test-concurrency-init-persist.json")
		e1 := NewConcurrencyEstimator(path, 8)
		e1.mu.Lock()
		e1.cap = 3
		e1.persist()
		e1.mu.Unlock()

		e2 := NewConcurrencyEstimator(path, 8)
		if got := e2.Cap(); got != 3 {
			t.Errorf("expected persisted cap 3, got %d", got)
		}
	})
}

func TestConcurrencyEstimator_CircuitOpen(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-concurrency-circuit.json")
	e := NewConcurrencyEstimator(path, defaultInitCap)
	e.RecordCircuitOpen()
	if e.Cap() != 1 {
		t.Fatalf("expected cap 1 after circuit open, got %d", e.Cap())
	}
	e.RecordCircuitClosed()
	if e.Cap() != defaultInitCap {
		t.Fatalf("expected cap to recover to %d after circuit closed, got %d", defaultInitCap, e.Cap())
	}
}

func TestPerModelConcurrencyEstimator_Basic(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-per-model.json")

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Initially all models return default cap
	if p.Cap("model-a") != 4 {
		t.Fatalf("expected default cap 4 for unknown model, got %d", p.Cap("model-a"))
	}

	// Record 429s for model-a
	now := time.Now()
	p.RecordRequest(now.Add(-1*time.Second), 429, "model-a")
	p.RecordRequest(now.Add(-500*time.Millisecond), 429, "model-a")
	p.recomputeAll()

	// model-a should have reduced cap
	if p.Cap("model-a") >= 4 {
		t.Fatalf("expected model-a cap to decrease, got %d", p.Cap("model-a"))
	}

	// model-b should still have default cap
	if p.Cap("model-b") != 4 {
		t.Fatalf("expected model-b cap to stay at default, got %d", p.Cap("model-b"))
	}
}

func TestPerModelConcurrencyEstimator_PersistAndLoad(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-per-model-persist.json")

	p1 := NewPerModelConcurrencyEstimator(path, 4)
	p1.RecordRequest(time.Now(), 429, "model-a")
	p1.RecordRequest(time.Now(), 429, "model-a")
	p1.recomputeAll()
	p1.Stop() // triggers persist

	p2 := NewPerModelConcurrencyEstimator(path, 4)

	// model-a should have reduced cap
	if p2.Cap("model-a") >= 4 {
		t.Fatalf("expected model-a cap to be loaded from persist, got %d", p2.Cap("model-a"))
	}

	// model-b should still have default cap
	if p2.Cap("model-b") != 4 {
		t.Fatalf("expected model-b cap to stay at default, got %d", p2.Cap("model-b"))
	}
}

// TestPerModelConcurrencyEstimator_MultipleModelsPersist verifies that
// the per-model estimator correctly persists and restores caps for
// multiple distinct models in a single JSON file.
func TestPerModelConcurrencyEstimator_MultipleModelsPersist(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-per-model-multi.json")

	p1 := NewPerModelConcurrencyEstimator(path, 4)

	// model-a: receives 429s -> cap drops (one drop per recompute when threshold exceeded)
	for i := 0; i < 4; i++ {
		p1.RecordRequest(time.Now(), 429, "model-a")
	}
	// Check cap before recompute
	t.Logf("model-a cap before recompute: %d", p1.Cap("model-a"))
	p1.recomputeAll() // cap drops from 4 to 3
	t.Logf("model-a cap after recompute: %d", p1.Cap("model-a"))

	// model-b: no 429s -> stays at default 4

	// model-c: first create estimator with a request, then circuit open
	p1.RecordRequest(time.Now(), 200, "model-c")
	p1.RecordCircuitOpen("model-c")
	t.Logf("model-c cap after circuit open: %d", p1.Cap("model-c"))
	// Second recompute: model-a drops again (429s still in window) from 3 to 2
	p1.recomputeAll()
	t.Logf("After second recompute: model-a cap=%d", p1.Cap("model-a"))

	p1.Stop()

	// Restore
	p2 := NewPerModelConcurrencyEstimator(path, 4)

	t.Logf("model-a cap after restore: %d", p2.Cap("model-a"))
	t.Logf("model-b cap after restore: %d", p2.Cap("model-b"))
	t.Logf("model-c cap after restore: %d", p2.Cap("model-c"))

	// model-a: cap dropped twice (4->3->2) because two recomputes with 429s in window
	if p2.Cap("model-a") != 2 {
		t.Errorf("expected model-a cap 2 from persist, got %d", p2.Cap("model-a"))
	}
	// model-b: default cap
	if p2.Cap("model-b") != 4 {
		t.Errorf("expected model-b cap 4 (default), got %d", p2.Cap("model-b"))
	}
	// model-c: circuit open forces cap to 1
	if p2.Cap("model-c") != 1 {
		t.Errorf("expected model-c cap 1 from circuit open, got %d", p2.Cap("model-c"))
	}
}

// TestPerModelConcurrencyEstimator_DefaultCapFallback verifies that
// unknown models fall back to the default cap, and that the default
// cap is respected even when not persisted.
func TestPerModelConcurrencyEstimator_DefaultCapFallback(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-per-model-fallback.json")

	// Custom default cap of 8
	p := NewPerModelConcurrencyEstimator(path, 8)

	// Unknown model should return default
	if p.Cap("unknown-model") != 8 {
		t.Errorf("expected default cap 8 for unknown model, got %d", p.Cap("unknown-model"))
	}

	// After persisting and restoring, unknown model should still get default
	p.Stop()
	p2 := NewPerModelConcurrencyEstimator(path, 8)
	if p2.Cap("another-unknown") != 8 {
		t.Errorf("expected default cap 8 after restore, got %d", p2.Cap("another-unknown"))
	}

	// Persisted model should use persisted value.
	// With 4 429s and one recompute, cap drops from 8 to 7 (one drop per recompute).
	p1 := NewPerModelConcurrencyEstimator(path, 8)
	p1.RecordRequest(time.Now(), 429, "persisted-model")
	p1.RecordRequest(time.Now(), 429, "persisted-model")
	p1.RecordRequest(time.Now(), 429, "persisted-model")
	p1.RecordRequest(time.Now(), 429, "persisted-model")
	p1.recomputeAll()
	p1.Stop()

	p3 := NewPerModelConcurrencyEstimator(path, 8)
	if p3.Cap("persisted-model") != 7 {
		t.Errorf("expected persisted model cap 7 (one drop from 8), got %d", p3.Cap("persisted-model"))
	}
	if p3.Cap("unknown-model") != 8 {
		t.Errorf("expected unknown model to get default 8, got %d", p3.Cap("unknown-model"))
	}
}

// TestPerModelConcurrencyEstimator_ConcurrencyIsolation verifies that
// recording requests for one model does not affect another model's cap.
func TestPerModelConcurrencyEstimator_ConcurrencyIsolation(t *testing.T) {
	dir := testPersistDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "test-per-model-isolation.json")

	p := NewPerModelConcurrencyEstimator(path, 4)

	// Hammer model-a with 429s
	for i := 0; i < 10; i++ {
		p.RecordRequest(time.Now(), 429, "model-a")
	}
	p.recomputeAll()

	// model-b untouched
	p.RecordRequest(time.Now(), 200, "model-b")
	p.recomputeAll()

	if p.Cap("model-a") >= 4 {
		t.Errorf("expected model-a cap to drop from 429s, got %d", p.Cap("model-a"))
	}
	if p.Cap("model-b") != 4 {
		t.Errorf("expected model-b cap to stay at 4, got %d", p.Cap("model-b"))
	}
}

// TestPerModelConcurrencyEstimator_PersistPathEmpty verifies that
// an empty persist path does not cause panics and works in-memory only.
func TestPerModelConcurrencyEstimator_PersistPathEmpty(t *testing.T) {
	p := NewPerModelConcurrencyEstimator("", 4)

	p.RecordRequest(time.Now(), 429, "model-a")
	p.RecordRequest(time.Now(), 429, "model-a")
	p.recomputeAll()

	if p.Cap("model-a") >= 4 {
		t.Errorf("expected model-a cap to drop, got %d", p.Cap("model-a"))
	}

	// Stop should not panic with empty path
	p.Stop()

	// Cap should still work after Stop
	if p.Cap("model-a") >= 4 {
		t.Errorf("expected cap to persist after Stop, got %d", p.Cap("model-a"))
	}
}
