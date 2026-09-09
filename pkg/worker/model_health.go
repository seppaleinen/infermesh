package worker

import (
	"sync"
	"time"
)

// HealthStatus represents the health state of a backend.
type HealthStatus int

const (
	Healthy HealthStatus = iota
	Degraded
	Unhealthy
	Unknown
)

// ModelMetrics contains operational metrics for a model.
type ModelMetrics struct {
	LoadCount     int
	UnloadCount   int
	TotalLatency  time.Duration
	VramUsageMB   int
	GpuUsagePct   int
	RequestCount  int
	ErrorCount    int
}

// ModelHealthTracker tracks the health and metrics of a model.
type ModelHealthTracker struct {
	mu sync.Mutex

	LastHealthCheck   time.Time
	HealthStatus      HealthStatus
	ConsecutiveFailures int
	Metrics           ModelMetrics
	Enabled           bool
}

// NewModelHealthTracker creates a new model health tracker with health checks enabled by default.
func NewModelHealthTracker(enabled bool) *ModelHealthTracker {
	return &ModelHealthTracker{
		HealthStatus: Unknown,
		Enabled:      enabled,
	}
}

// GetEnabled returns whether health check recording is enabled.
// Thread-safe: acquires the internal mutex.
func (t *ModelHealthTracker) GetEnabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Enabled
}

// SetEnabled toggles whether health checks are recorded by this tracker.
func (t *ModelHealthTracker) SetEnabled(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Enabled = enabled
}

// RecordHealthCheck records the result of a health check.
func (t *ModelHealthTracker) RecordHealthCheck(status HealthStatus, metrics ModelMetrics) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.Enabled {
		return
	}

	t.LastHealthCheck = time.Now()
	t.HealthStatus = status
	if status == Healthy {
		t.ConsecutiveFailures = 0
	} else {
		t.ConsecutiveFailures++
	}
	t.Metrics = metrics
}

// Snapshot returns a copy of the current health state.
//
// Note: This is a 5-tuple return signature (breaking change from a struct-based
// return). Since Snapshot() is only used internally (per grep), this is acceptable
// for MVP. External consumers should expect this signature may change.
func (t *ModelHealthTracker) Snapshot() (time.Time, HealthStatus, int, ModelMetrics, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.LastHealthCheck, t.HealthStatus, t.ConsecutiveFailures, t.Metrics, t.Enabled
}
