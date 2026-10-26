package worker

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestServerShutdown(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	_ = capabilities.Defaults()

	tests := []struct {
		name string
	}{
		{
			name: "graceful shutdown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			errChan := make(chan error, 1)
			go func() {
				errChan <- server.Start(ctx)
			}()

			time.Sleep(100 * time.Millisecond)

			cancel()

			select {
			case err := <-errChan:
				if err != nil && err.Error() != "http: Server closed" && err.Error() != "http: Server closed\n" {
					t.Logf("Server shutdown error (expected): %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("Server did not shutdown in time")
			}
		})
	}
}
func TestServer_BindFailsLoudlyWhenPortIsTaken(t *testing.T) {
	// Reserve a port by binding a throwaway listener on tcp4.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	taken := ln.Addr().String()
	defer func() { _ = ln.Close() }()

	server := NewServer(testLogger(), taken, security.Config{DevMode: true})
	_ = capabilities.Defaults()

	err = server.Start(context.Background())
	if err == nil {
		t.Fatalf("expected Start() to fail when port %s is held, got nil", taken)
	}
	if !strings.Contains(err.Error(), "worker failed to bind") {
		t.Errorf("expected bind error, got: %v", err)
	}
}
func TestServer_BindsTcp4(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	server := NewServer(testLogger(), addr, security.Config{DevMode: true})
	_ = capabilities.Defaults()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() { errChan <- server.Start(ctx) }()

	// Wait for the server to begin accepting connections before probing.
	if !waitForHTTP(t, "http://"+addr+"/health", 5*time.Second) {
		t.Fatalf("worker did not start on %s", addr)
	}

	// The server must answer on the exact tcp4 address it was given.
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("worker did not serve on %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	cancel()
	select {
	case err := <-errChan:
		if err != nil && err.Error() != "http: Server closed" {
			t.Logf("shutdown error (expected): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("server did not shut down in time")
	}
}
func waitForHTTP(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
