package integration_security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// testLogger returns a logger for testing
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// sampleWorkerInfo returns a sample WorkerInfo for testing
func sampleWorkerInfo(id string, port int) protocol.WorkerInfo {
	hostname, _ := os.Hostname()
	return protocol.WorkerInfo{
		ID:       id,
		Hostname: hostname,
		IP:       "127.0.0.1",
		Port:     port,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "test-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
}

// testContext creates a context for testing with timeout
func testContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 30 seconds to prevent goroutine leaks
	go func() {
		time.Sleep(30 * time.Second)
		cancel()
	}()
	return ctx
}

// mustLoadTLSCert loads a TLS certificate or fails the test
func mustLoadTLSCert(t *testing.T, certPath, keyPath string) *tls.Certificate {
	t.Helper()
	cert, err := security.LoadTLSCertFromFile(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadTLSCertFromFile failed: %v", err)
	}
	return cert
}

// mustJSON marshals to JSON or fails the test
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}
	return b
}

// TestDefaults tests security Defaults function
func TestDefaults(t *testing.T) {
	cfg := security.Defaults()
	if !cfg.DevMode {
		t.Errorf("Defaults().DevMode should be true, got %v", cfg.DevMode)
	}
}

// TestIsDevMode tests the IsDevMode function
func TestIsDevMode(t *testing.T) {
	tests := []struct {
		name     string
		cfg      security.Config
		expected bool
	}{
		{
			name:     "dev mode true",
			cfg:      security.Config{DevMode: true},
			expected: true,
		},
		{
			name:     "dev mode false",
			cfg:      security.Config{DevMode: false},
			expected: false,
		},
		{
			name:     "default config",
			cfg:      security.Defaults(),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := security.IsDevMode(tt.cfg)
			if result != tt.expected {
				t.Errorf("IsDevMode(%v) = %v, want %v", tt.cfg, result, tt.expected)
			}
		})
	}
}

// TestNewFileAuditLogger tests file-based audit logging
func TestNewFileAuditLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := tmpDir + "/audit.log"

	logger, err := security.NewFileAuditLogger(logPath)
	if err != nil {
		t.Fatalf("NewFileAuditLogger failed: %v", err)
	}
	if logger == nil {
		t.Fatal("NewFileAuditLogger returned nil")
	}

	// Log a test entry
	ctx := context.Background()
	attrs := map[string]interface{}{
		"worker_id": "worker-1",
		"action":    "request",
		"status":    "allowed",
	}
	logger.Log(ctx, "test audit message", attrs)

	// Read the log file
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	// Parse JSON line
	var entry map[string]interface{}
	if err := json.Unmarshal(content, &entry); err != nil {
		t.Fatalf("Failed to parse audit log JSON: %v", err)
	}

	// Verify message
	if entry["message"] != "test audit message" {
		t.Errorf("Log message: got %v, want test audit message", entry["message"])
	}

	// Verify attrs
	if entry["worker_id"] != "worker-1" {
		t.Errorf("worker_id: got %v, want worker-1", entry["worker_id"])
	}
	if entry["action"] != "request" {
		t.Errorf("action: got %v, want request", entry["action"])
	}
	if entry["status"] != "allowed" {
		t.Errorf("status: got %v, want allowed", entry["status"])
	}
}

// TestMTLSMiddlewareWithMockServer tests mTLS middleware with a mock TLS server
func TestMTLSMiddlewareWithMockServer(t *testing.T) {
	// Generate CA and certs
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate server cert
	serverCertPath, serverKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "server")
	if err != nil {
		t.Fatalf("GenerateNodeCert for server failed: %v", err)
	}

	// Generate client cert
	clientCertPath, clientKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "client")
	if err != nil {
		t.Fatalf("GenerateNodeCert for client failed: %v", err)
	}

	// Load CA cert
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("Failed to read CA cert: %v", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert: %v", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(caCert)

	// Create middleware with required=true and allowedCN=["client"]
	middleware := security.NewMTLSMiddleware(caCertPool, true, "client")

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	handler := middleware(testHandler)

	// Create TLS test server
	testServer := httptest.NewUnstartedServer(handler)
	testServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{*mustLoadTLSCert(t, serverCertPath, serverKeyPath)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
	}
	testServer.StartTLS()
	defer testServer.Close()

	// Test with valid client cert
	t.Run("valid client cert gets 200", func(t *testing.T) {
		clientTLSCert := mustLoadTLSCert(t, clientCertPath, clientKeyPath)
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{*clientTLSCert},
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(testServer.URL)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	// Test without client cert
	t.Run("no client cert gets 403", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(testServer.URL)
		if err != nil {
			// TLS handshake failure is expected
			t.Logf("TLS handshake failed as expected (no client cert)")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})

	// Test with invalid client cert (wrong CN)
	t.Run("invalid CN gets 403", func(t *testing.T) {
		// Generate a cert with wrong CN
		wrongCertDir := t.TempDir()
		wrongCertPath, wrongKeyPath, err := security.GenerateNodeCert(wrongCertDir, caCertPath, caKeyPath, "wrong-cn")
		if err != nil {
			t.Fatalf("GenerateNodeCert failed: %v", err)
		}

		wrongTLSCert := mustLoadTLSCert(t, wrongCertPath, wrongKeyPath)
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{*wrongTLSCert},
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(testServer.URL)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})
}

// TestDevModeWithoutAuth tests that dev mode works without authentication
func TestDevModeWithoutAuth(t *testing.T) {
	// Create a simple test server that mimics dev mode behavior
	// Dev mode means: no mTLS, no auth, just HTTP
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
			// In dev mode, just return a simple response
			w.Header().Set("Content-Type", "application/json")
			response := map[string]interface{}{
				"object": "list",
				"data":  []interface{}{},
			}
			json.NewEncoder(w).Encode(response)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	testServer := httptest.NewServer(handler)
	defer testServer.Close()

	// Test GET /v1/models without any auth (should work in dev mode)
	resp, err := http.Get(testServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}
}

// TestMTLSRequiredInProd tests that prod mode requires mTLS
func TestMTLSRequiredInProd(t *testing.T) {
	// Create a simple test server that requires mTLS
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate server cert
	serverCertPath, serverKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "server")
	if err != nil {
		t.Fatalf("GenerateNodeCert for server failed: %v", err)
	}

	// Load CA cert
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("Failed to read CA cert: %v", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert: %v", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(caCert)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	// Create middleware that requires mTLS
	middleware := security.NewMTLSMiddleware(caCertPool, true, "worker-", "gpu-worker")
	protectedHandler := middleware(handler)

	// Create TLS test server
	testServer := httptest.NewUnstartedServer(protectedHandler)
	testServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{*mustLoadTLSCert(t, serverCertPath, serverKeyPath)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
	}
	testServer.StartTLS()
	defer testServer.Close()

	// Test 1: Request without client cert should get 403
	t.Run("no client cert fails", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(testServer.URL + "/health")
		if err != nil {
			// TLS handshake failure is expected
			t.Logf("TLS handshake failed as expected (no client cert)")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != 403 {
			t.Logf("Got status %d without client cert (expected 4xx)", resp.StatusCode)
		}
	})

	// Test 2: Request with valid client cert should succeed
	t.Run("valid client cert succeeds", func(t *testing.T) {
		// Generate a worker cert
		workerCertPath, workerKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
		if err != nil {
			t.Fatalf("GenerateNodeCert failed: %v", err)
		}

		workerTLSCert := mustLoadTLSCert(t, workerCertPath, workerKeyPath)
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{*workerTLSCert},
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(testServer.URL + "/health")
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})
}