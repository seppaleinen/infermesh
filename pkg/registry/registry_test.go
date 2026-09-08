package registry

import (
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"
	"context"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func sampleWorker() protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "host1",
		IP:       "192.168.1.100",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.UnavailableTTL != 60*time.Second {
		t.Errorf("UnavailableTTL: got %v, want 60s", cfg.UnavailableTTL)
	}
	if cfg.RemoveTTL != 120*time.Second {
		t.Errorf("RemoveTTL: got %v, want 120s", cfg.RemoveTTL)
	}
	if cfg.CheckInterval != 5*time.Second {
		t.Errorf("CheckInterval: got %v, want 5s", cfg.CheckInterval)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid config",
			cfg:     Defaults(),
			wantErr: false,
		},
		{
			name:    "short unavailable ttl",
			cfg:     Config{UnavailableTTL: 100 * time.Millisecond, RemoveTTL: 120 * time.Second, CheckInterval: 5 * time.Second},
			wantErr: true,
		},
		{
			name:    "remove ttl < unavailable ttl",
			cfg:     Config{UnavailableTTL: 60 * time.Second, RemoveTTL: 10 * time.Second, CheckInterval: 5 * time.Second},
			wantErr: true,
		},
		{
			name:    "short check interval",
			cfg:     Config{UnavailableTTL: 60 * time.Second, RemoveTTL: 120 * time.Second, CheckInterval: 100 * time.Millisecond},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestNewRegistry(t *testing.T) {
	log := testLogger()
	cfg := Defaults()

	reg, err := New(cfg, log)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

func TestNewRegistryInvalidConfig(t *testing.T) {
	log := testLogger()
	cfg := Config{UnavailableTTL: 100 * time.Millisecond}
	_, err := New(cfg, log)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestRegistryHandleEventAdded(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	event := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}

	if err := reg.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Worker should be in registry
	found, ok := reg.Get(worker.ID)
	if !ok {
		t.Fatal("worker not found in registry")
	}
	if found.ID != worker.ID {
		t.Errorf("ID mismatch: got %s, want %s", found.ID, worker.ID)
	}

	// Should be available
	available := reg.ListAvailable()
	if len(available) != 1 {
		t.Errorf("expected 1 available worker, got %d", len(available))
	}
}

func TestRegistryHandleEventUpdated(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	// Add first
	event1 := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event1); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Update with new port
	updated := worker
	updated.Port = 9090
	event2 := protocol.DiscoveryEvent{
		Type:   protocol.EventUpdated,
		Worker: updated,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event2); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	found, _ := reg.Get(worker.ID)
	if found.Port != 9090 {
		t.Errorf("expected port 9090, got %d", found.Port)
	}
}

func TestRegistryHandleEventRemoved(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	event1 := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event1); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	event2 := protocol.DiscoveryEvent{
		Type:   protocol.EventRemoved,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event2); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	_, ok := reg.Get(worker.ID)
	if ok {
		t.Error("worker should be removed from registry")
	}
}

func TestRegistryHandleEventExpired(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	// Add first
	event1 := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event1); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Expire it
	event2 := protocol.DiscoveryEvent{
		Type:   protocol.EventExpired,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event2); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	found, _ := reg.Get(worker.ID)
	if found.Status != protocol.StatusUnavailable {
		t.Errorf("expected status unavailable, got %s", found.Status)
	}
}

func TestRegistryList(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	workers := []protocol.WorkerInfo{
		{ID: "w1", Port: 8081},
		{ID: "w2", Port: 8082},
		{ID: "w3", Port: 8083},
	}

	for _, w := range workers {
		event := protocol.DiscoveryEvent{
			Type:   protocol.EventAdded,
			Worker: w,
			Time:   time.Now(),
		}
		if err := reg.HandleEvent(event); err != nil {
			t.Fatalf("HandleEvent failed: %v", err)
		}
	}

	all := reg.List()
	if len(all) != 3 {
		t.Errorf("expected 3 workers, got %d", len(all))
	}
}

func TestRegistryListAvailable(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	workers := []protocol.WorkerInfo{
		{ID: "w1", Port: 8081},
		{ID: "w2", Port: 8082},
	}

	for _, w := range workers {
		event := protocol.DiscoveryEvent{
			Type:   protocol.EventAdded,
			Worker: w,
			Time:   time.Now(),
		}
		if err := reg.HandleEvent(event); err != nil {
			t.Fatalf("HandleEvent failed: %v", err)
		}
	}

	available := reg.ListAvailable()
	if len(available) != 2 {
		t.Errorf("expected 2 available workers, got %d", len(available))
	}
}

func TestRegistryGet(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	event := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	found, ok := reg.Get(worker.ID)
	if !ok {
		t.Fatal("worker not found")
	}
	if found.ID != worker.ID {
		t.Errorf("ID mismatch: got %s, want %s", found.ID, worker.ID)
	}
}

func TestRegistryGetNotFound(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestRegistrySubscribe(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	sub := reg.Subscribe()
	worker := sampleWorker()
	event := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Subscribe should receive the event
	select {
	case ev := <-sub:
		if ev.Type != RegisterAdded {
			t.Errorf("expected event type RegisterAdded, got %s", ev.Type)
		}
		if ev.Worker.ID != worker.ID {
			t.Errorf("expected worker ID %s, got %s", worker.ID, ev.Worker.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestRegistryStop(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	if err := reg.Stop(); err != nil {
		t.Errorf("Stop returned error: %v", err)
	}
}

func TestRegistrySweepMarksUnavailable(t *testing.T) {
	log := testLogger()
	cfg := Defaults()
	cfg.UnavailableTTL = 1 * time.Second
	cfg.RemoveTTL = 2 * time.Second
	cfg.CheckInterval = 1 * time.Second

	reg, err := New(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	event := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Wait for sweep to mark unavailable
	time.Sleep(1500 * time.Millisecond)

	found, _ := reg.Get(worker.ID)
	if found.Status != protocol.StatusUnavailable {
		t.Errorf("expected worker to be unavailable, got %s", found.Status)
	}
}

func TestRegistrySweepRemoves(t *testing.T) {
	log := testLogger()
	cfg := Defaults()
	cfg.UnavailableTTL = 1 * time.Second
	cfg.RemoveTTL = 2 * time.Second
	cfg.CheckInterval = 1 * time.Second

	reg, err := New(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()
	event := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Wait for sweep to remove
	time.Sleep(2500 * time.Millisecond)

	_, ok := reg.Get(worker.ID)
	if ok {
		t.Error("worker should be removed from registry")
	}
}

func TestRegistryReRegisterAvailable(t *testing.T) {
	log := testLogger()
	reg, err := New(Defaults(), log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	defer reg.Stop()

	ctx := testContext()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("expected no error starting: %v", err)
	}

	worker := sampleWorker()

	// Add
	event1 := protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event1); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	// Mark unavailable manually by setting lastSeen to past
	// Access internal state via type assertion
	memReg, ok := reg.(*memoryRegistry)
	if !ok {
		t.Fatal("expected memoryRegistry")
	}
	memReg.mu.Lock()
	state := memReg.workers[worker.ID]
	state.lastSeen = time.Now().Add(-120 * time.Second)
	state.available = false
	state.info.Status = protocol.StatusUnavailable
	memReg.mu.Unlock()

	// Re-register
	event2 := protocol.DiscoveryEvent{
		Type:   protocol.EventUpdated,
		Worker: worker,
		Time:   time.Now(),
	}
	if err := reg.HandleEvent(event2); err != nil {
		t.Fatalf("HandleEvent failed: %v", err)
	}

	found, _ := reg.Get(worker.ID)
	if found.Status != protocol.StatusAvailable {
		t.Errorf("expected worker to be available after re-registration, got %s", found.Status)
	}
}

// testContext creates a context that doesn't cancel for testing purposes.
func testContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 30 seconds to prevent goroutine leaks
	go func() {
		time.Sleep(30 * time.Second)
		cancel()
	}()
	return ctx
}

// Ensure errors variable is used
var _ = errors.New