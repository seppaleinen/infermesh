package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestServer_ProdModeRejectsInvalidTLS(t *testing.T) {
	tests := []struct {
		name           string
		cfg            security.Config
		addr           string
		expectError    bool
		errorContains  string
	}{
		// Fallback cases: all three mTLS fields empty -> plain HTTP on loopback
		{"missing cert on loopback - fallback to plain HTTP", security.Config{DevMode: false, MTLSCert: "", MTLSKey: "", CertDir: ""}, "127.0.0.1:0", false, ""},
		{"missing key on loopback - fallback to plain HTTP", security.Config{DevMode: false, MTLSCert: "", MTLSKey: "", CertDir: ""}, "127.0.0.1:0", false, ""},
		{"missing CA on loopback - fallback to plain HTTP", security.Config{DevMode: false, MTLSCert: "", MTLSKey: "", CertDir: ""}, "127.0.0.1:0", false, ""},
		// Partial configs (some fields set) should fail validation even on loopback
		{"partial config (cert only) on loopback", security.Config{DevMode: false, MTLSCert: "/nonexistent/cert.pem", MTLSKey: "", CertDir: ""}, "127.0.0.1:0", true, "invalid TLS configuration"},
		{"partial config (cert+key) on loopback", security.Config{DevMode: false, MTLSCert: "/nonexistent/cert.pem", MTLSKey: "/nonexistent/key.pem", CertDir: ""}, "127.0.0.1:0", true, "invalid TLS configuration"},
		{"partial config (cert+key+bad CA) on loopback", security.Config{DevMode: false, MTLSCert: "/nonexistent/cert.pem", MTLSKey: "/nonexistent/key.pem", CertDir: "/nonexistent"}, "127.0.0.1:0", true, "invalid TLS configuration"},
		// Non-loopback addresses should still reject invalid TLS config
		{"missing cert on non-loopback", security.Config{DevMode: false, MTLSCert: "", MTLSKey: "", CertDir: ""}, "0.0.0.0:0", true, "invalid TLS configuration"},
		{"partial config (cert only) on non-loopback", security.Config{DevMode: false, MTLSCert: "/nonexistent/cert.pem", MTLSKey: "", CertDir: ""}, "0.0.0.0:0", true, "invalid TLS configuration"},
		{"partial config (cert+key+bad CA) on non-loopback", security.Config{DevMode: false, MTLSCert: "/nonexistent/cert.pem", MTLSKey: "/nonexistent/key.pem", CertDir: "/nonexistent"}, "0.0.0.0:0", true, "invalid TLS configuration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := NewServer(testLogger(), tt.addr, tt.cfg)
			errChan := make(chan error, 1)
			go func() {
				errChan <- server.Start(ctx)
			}()

			// Give the server a moment to start or fail
			select {
			case err := <-errChan:
				if tt.expectError {
					if err == nil {
						t.Fatal("expected Start() to fail with invalid TLS config")
					}
					if !strings.Contains(err.Error(), tt.errorContains) {
						t.Errorf("expected error to contain %q, got: %v", tt.errorContains, err)
					}
				} else {
					if err != nil {
						t.Fatalf("expected Start() to succeed (loopback fallback), got error: %v", err)
					}
				}
			case <-time.After(2 * time.Second):
				// Server started successfully (loopback fallback) - clean up
				if !tt.expectError {
					cancel()
					// Wait for server to shut down
					select {
					case err := <-errChan:
						if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
							t.Errorf("server returned error on shutdown: %v", err)
						}
					case <-time.After(1 * time.Second):
						t.Log("server shutdown timed out")
					}
				} else {
					t.Fatal("expected Start() to fail quickly, but it's still running")
				}
			}
		})
	}
}
func TestServer_ProdModeEnforcesMTLS(t *testing.T) {
	caDir := t.TempDir()
	routerDir := t.TempDir()
	workerDir := t.TempDir()

	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate a router/client cert (CN "router-1").
	routerCertPath, routerKeyPath, err := security.GenerateNodeCert(routerDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert for router failed: %v", err)
	}

	// Generate a worker cert (CN "worker-1") for the server itself.
	workerCertPath, workerKeyPath, err := security.GenerateNodeCert(workerDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert for worker failed: %v", err)
	}

	// Start the worker server in prod mode on a free port.
	port := freeTestPort(t)
	cfg := security.Config{
		DevMode:    false,
		MTLSCert:   workerCertPath,
		MTLSKey:    workerKeyPath,
		CertDir:    caDir,
		APIKey:     "test-api-key",
		TrustedCNs: "router-1",
	}
	server := NewServer(testLogger(), fmt.Sprintf("127.0.0.1:%d", port), cfg)
	server.SetBackend(newMockBackend(), "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Wait for the server to come up.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker server did not come up in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Run("no client cert gets 403", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/health", port))
		if err != nil {
			t.Logf("TLS handshake failed as expected (no client cert)")
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})

	t.Run("valid client cert with wrong CN gets 403", func(t *testing.T) {
		// Generate a client cert with CN "worker-1" (not in trusted CNs).
		wrongCNPath, wrongKeyPath, err := security.GenerateNodeCert(workerDir, caCertPath, caKeyPath, "worker-1")
		if err != nil {
			t.Fatalf("GenerateNodeCert failed: %v", err)
		}
		wrongCert, err := security.LoadTLSCertFromFile(wrongCNPath, wrongKeyPath)
		if err != nil {
			t.Fatalf("LoadTLSCertFromFile failed: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{*wrongCert},
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/health", port))
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})

	t.Run("valid client cert with correct CN gets 200", func(t *testing.T) {
		routerCert, err := security.LoadTLSCertFromFile(routerCertPath, routerKeyPath)
		if err != nil {
			t.Fatalf("LoadTLSCertFromFile failed: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{*routerCert},
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/health", port))
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	cancel()
	if err := <-errChan; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("server Start returned error: %v", err)
	}
}
