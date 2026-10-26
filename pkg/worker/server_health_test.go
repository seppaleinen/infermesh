package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestModelHealthTracker_EnabledDisabled(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		tracker := NewModelHealthTracker(true)
		tracker.SetEnabled(false)

		tracker.RecordHealthCheck(Healthy, ModelMetrics{})

		_, status, failures, _, enabled := tracker.Snapshot()
		if enabled {
			t.Error("expected tracker to be disabled")
		}
		if status != Unknown {
			t.Errorf("expected status Unknown, got %v", status)
		}
		if failures != 0 {
			t.Errorf("expected 0 consecutive failures, got %d", failures)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		tracker := NewModelHealthTracker(true)
		tracker.SetEnabled(true)

		tracker.RecordHealthCheck(Healthy, ModelMetrics{})

		_, status, failures, _, enabled := tracker.Snapshot()
		if !enabled {
			t.Error("expected tracker to be enabled")
		}
		if status != Healthy {
			t.Errorf("expected status Healthy, got %v", status)
		}
		if failures != 0 {
			t.Errorf("expected 0 consecutive failures, got %d", failures)
		}
	})
}
func TestModelHealthTracker_SetEnabled(t *testing.T) {
	tracker := NewModelHealthTracker(true)

	tracker.RecordHealthCheck(Healthy, ModelMetrics{})
	tracker.SetEnabled(false)
	tracker.RecordHealthCheck(Unhealthy, ModelMetrics{})
	tracker.SetEnabled(true)
	tracker.RecordHealthCheck(Unhealthy, ModelMetrics{})

	_, status, failures, _, _ := tracker.Snapshot()
	if status != Unhealthy {
		t.Errorf("expected status Unhealthy, got %v", status)
	}
	if failures != 1 {
		t.Errorf("expected 1 consecutive failure, got %d", failures)
	}
}
func TestServer_HealthCheckLoop_Enabled(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.StartHealthCheckLoop(ctx, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Healthy {
		t.Errorf("expected status Healthy, got %v", status)
	}
	if failures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", failures)
	}
}
func TestServer_HealthCheckLoop_NilBackend(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		server.StartHealthCheckLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Unknown {
		t.Errorf("expected status Unknown, got %v", status)
	}
	if failures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", failures)
	}

	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Error("health check loop did not stop after context cancellation")
	}
}
func TestServer_HealthCheckLoop_FailurePath(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	backend.SetHealthCheckError(errors.New("backend unavailable"))
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.StartHealthCheckLoop(ctx, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, status, failures, _, enabled := server.healthTracker.Snapshot()
	if !enabled {
		t.Error("expected tracker to be enabled")
	}
	if status != Unhealthy {
		t.Errorf("expected status Unhealthy, got %v", status)
	}
	if failures == 0 {
		t.Error("expected at least 1 consecutive failure to be recorded")
	}
}
func TestServer_HealthCheckLoop_Cancel(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: true})
	backend := newMockBackend()
	server.SetBackend(backend, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		server.StartHealthCheckLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — goroutine exited
	case <-time.After(2 * time.Second):
		t.Error("health check loop did not stop after context cancellation")
	}
}
func TestServer_NewServer_HealthTrackerInit(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		expected bool
	}{
		{
			name:     "enabled by default",
			enabled:  true,
			expected: true,
		},
		{
			name:     "disabled when configured",
			enabled:  false,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: tt.enabled})

			if server.healthTracker == nil {
				t.Fatal("expected health tracker to be initialized")
			}
			if server.healthTracker.Enabled != tt.expected {
				t.Errorf("expected Enabled=%v, got %v", tt.expected, server.healthTracker.Enabled)
			}
		})
	}
}
