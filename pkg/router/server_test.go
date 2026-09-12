package router

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestProdModeNonLoopbackPanics verifies that starting the router in
// production mode with a non-loopback bind address triggers a panic.
// This guards against accidentally exposing the router on a public interface
// in production (mTLS-only) deployments.
func TestProdModeNonLoopbackPanics(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	reg, err := registry.New(registry.Defaults(), log)
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry.Start: %v", err)
	}
	defer func() { _ = reg.Stop() }()

	// Production mode (DevMode == false) with a non-loopback address.
	cfg := security.Config{DevMode: false}
	srv := NewServer(reg, log, "0.0.0.0:8080", cfg)

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for non-loopback address in prod mode, got none")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = srv.Start(ctx)
}
