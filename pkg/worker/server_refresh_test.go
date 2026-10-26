package worker

import (
	"context"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestRefreshModels_DiscoveryAfterStart(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: false})
	backend := newMockBackend()
	server.SetBackend(backend, "test-model")

	// Initially the backend advertises nothing.
	if got := len(server.GetModels()); got != 0 {
		t.Fatalf("expected 0 models at startup, got %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshModels(ctx, server, backend, 10*time.Millisecond)

	// Simulate the backend starting after the worker: advertise a model.
	backend.SetMockModels([]protocol.ModelInfo{
		{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
	})

	waitFor(t, 2*time.Second, func() bool { return len(server.GetModels()) == 1 })

	if got := server.GetModels()[0].Name; got != "llama-3-8b" {
		t.Errorf("expected llama-3-8b, got %s", got)
	}

	// The backend advertises a second model; the refresh merges it rather
	// than replacing the catalogue.
	backend.SetMockModels([]protocol.ModelInfo{
		{Name: "llama-3-8b", Quantization: "Q4_K_M", Loaded: true},
		{Name: "mistral-7b", Quantization: "Q5_K_M", Loaded: true},
	})

	waitFor(t, 2*time.Second, func() bool { return len(server.GetModels()) == 2 })
}
func TestRefreshModelsNilBackend(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true, EnableHealthChecks: false})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		refreshModels(ctx, server, nil, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshModels with nil backend did not exit after cancellation")
	}
}
func TestEffectiveModelRefreshInterval(t *testing.T) {
	if got := effectiveModelRefreshInterval(0); got != DefaultModelRefreshInterval {
		t.Errorf("zero interval: expected %v, got %v", DefaultModelRefreshInterval, got)
	}
	if got := effectiveModelRefreshInterval(-time.Second); got != DefaultModelRefreshInterval {
		t.Errorf("negative interval: expected %v, got %v", DefaultModelRefreshInterval, got)
	}
	const custom = 5 * time.Minute
	if got := effectiveModelRefreshInterval(custom); got != custom {
		t.Errorf("custom interval: expected %v, got %v", custom, got)
	}
}
