package security

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testLogger returns a logger for testing
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// TestDefaults tests that Defaults returns Config{DevMode: true}
func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if !cfg.DevMode {
		t.Errorf("Defaults().DevMode should be true, got %v", cfg.DevMode)
	}
	if cfg.MTLSCert != "" {
		t.Errorf("Defaults().MTLSCert should be empty, got %q", cfg.MTLSCert)
	}
	if cfg.MTLSKey != "" {
		t.Errorf("Defaults().MTLSKey should be empty, got %q", cfg.MTLSKey)
	}
	if cfg.CertDir != "" {
		t.Errorf("Defaults().CertDir should be empty, got %q", cfg.CertDir)
	}
}

// TestIsDevMode tests the IsDevMode function
func TestIsDevMode(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		expected bool
	}{
		{
			name:     "dev mode true",
			cfg:      Config{DevMode: true},
			expected: true,
		},
		{
			name:     "dev mode false",
			cfg:      Config{DevMode: false},
			expected: false,
		},
		{
			name:     "default config",
			cfg:      Defaults(),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsDevMode(tt.cfg)
			if result != tt.expected {
				t.Errorf("IsDevMode(%v) = %v, want %v", tt.cfg, result, tt.expected)
			}
		})
	}
}

// TestGenerateSelfSignedCA tests CA certificate generation
func TestGenerateSelfSignedCA(t *testing.T) {
	tmpDir := t.TempDir()

	certFile, keyFile, err := GenerateSelfSignedCA(tmpDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Verify files exist
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		t.Errorf("CA cert file does not exist: %s", certFile)
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		t.Errorf("CA key file does not exist: %s", keyFile)
	}

	// Verify file names
	if filepath.Base(certFile) != "ca.crt" {
		t.Errorf("CA cert file name: got %q, want 'ca.crt'", filepath.Base(certFile))
	}
	if filepath.Base(keyFile) != "ca.key" {
		t.Errorf("CA key file name: got %q, want 'ca.key'", filepath.Base(keyFile))
	}

	// Parse the certificate
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("Failed to read CA cert: %v", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatal("Failed to decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert: %v", err)
	}

	// Verify it's self-signed (can verify itself)
	err = caCert.CheckSignatureFrom(caCert)
	if err != nil {
		t.Errorf("CA cert is not self-signed: %v", err)
	}

	// Verify IsCA is true
	if !caCert.IsCA {
		t.Errorf("CA cert IsCA should be true, got false")
	}

	// Verify basic constraints
	if !caCert.BasicConstraintsValid {
		t.Errorf("CA cert BasicConstraintsValid should be true")
	}

	// Verify key usage includes CertSign and CRLSign
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA cert should have KeyUsageCertSign")
	}
	if caCert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Errorf("CA cert should have KeyUsageCRLSign")
	}

	// Verify validity period is ~10 years
	expectedDuration := 10 * 365 * 24 * time.Hour
	actualDuration := caCert.NotAfter.Sub(caCert.NotBefore)
	tolerance := 24 * time.Hour // 1 day tolerance
	if actualDuration < expectedDuration-tolerance || actualDuration > expectedDuration+tolerance {
		t.Errorf("CA cert validity period: got %v, want ~%v", actualDuration, expectedDuration)
	}

	// Verify subject
	if caCert.Subject.CommonName != "InferMesh CA" {
		t.Errorf("CA cert CN: got %q, want %q", caCert.Subject.CommonName, "InferMesh CA")
	}
	if len(caCert.Subject.Organization) != 1 || caCert.Subject.Organization[0] != "InferMesh" {
		t.Errorf("CA cert Organization: got %v, want [InferMesh]", caCert.Subject.Organization)
	}
}

// TestGenerateNodeCert tests node certificate generation signed by CA
func TestGenerateNodeCert(t *testing.T) {
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	// Generate CA first
	caCertPath, caKeyPath, err := GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate node cert
	certFile, keyFile, err := GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Verify files exist
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		t.Errorf("Node cert file does not exist: %s", certFile)
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		t.Errorf("Node key file does not exist: %s", keyFile)
	}

	// Verify file names
	if filepath.Base(certFile) != "cert.pem" {
		t.Errorf("Node cert file name: got %q, want 'cert.pem'", filepath.Base(certFile))
	}
	if filepath.Base(keyFile) != "key.pem" {
		t.Errorf("Node key file name: got %q, want 'key.pem'", filepath.Base(keyFile))
	}

	// Parse the node certificate
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("Failed to read node cert: %v", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatal("Failed to decode node cert PEM")
	}
	nodeCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse node cert: %v", err)
	}

	// Parse CA certificate
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("Failed to read CA cert: %v", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		t.Fatal("Failed to decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert: %v", err)
	}

	// Verify node cert is signed by CA
	err = nodeCert.CheckSignatureFrom(caCert)
	if err != nil {
		t.Errorf("Node cert is not signed by CA: %v", err)
	}

	// Verify CommonName is "worker-1"
	if nodeCert.Subject.CommonName != "worker-1" {
		t.Errorf("Node cert CN: got %q, want %q", nodeCert.Subject.CommonName, "worker-1")
	}

	// Verify SANs include "localhost"
	foundLocalhost := false
	for _, dns := range nodeCert.DNSNames {
		if dns == "localhost" {
			foundLocalhost = true
			break
		}
	}
	if !foundLocalhost {
		t.Errorf("Node cert DNSNames should include 'localhost', got %v", nodeCert.DNSNames)
	}

	// Verify ExtKeyUsage includes ClientAuth and ServerAuth
	hasClientAuth := false
	hasServerAuth := false
	for _, eku := range nodeCert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasClientAuth {
		t.Errorf("Node cert should have ExtKeyUsageClientAuth")
	}
	if !hasServerAuth {
		t.Errorf("Node cert should have ExtKeyUsageServerAuth")
	}

	// Verify KeyUsage includes DigitalSignature and KeyEncipherment
	if nodeCert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("Node cert should have KeyUsageDigitalSignature")
	}
	if nodeCert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Errorf("Node cert should have KeyUsageKeyEncipherment")
	}

	// Verify validity period is ~10 years
	expectedDuration := 10 * 365 * 24 * time.Hour
	actualDuration := nodeCert.NotAfter.Sub(nodeCert.NotBefore)
	tolerance := 24 * time.Hour
	if actualDuration < expectedDuration-tolerance || actualDuration > expectedDuration+tolerance {
		t.Errorf("Node cert validity period: got %v, want ~%v", actualDuration, expectedDuration)
	}

	// Verify IPAddresses contains an IP (should be primary IPv4 or 127.0.0.1)
	if len(nodeCert.IPAddresses) == 0 {
		t.Errorf("Node cert should have at least one IP address in SANs")
	}

	// Verify organization
	if len(nodeCert.Subject.Organization) != 1 || nodeCert.Subject.Organization[0] != "InferMesh" {
		t.Errorf("Node cert Organization: got %v, want [InferMesh]", nodeCert.Subject.Organization)
	}
}

// TestLoadTLSCertFromFile tests loading TLS certificate from files
func TestLoadTLSCertFromFile(t *testing.T) {
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	// Generate CA
	caCertPath, caKeyPath, err := GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate node cert
	certPath, keyPath, err := GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Load the TLS certificate
	tlsCert, err := LoadTLSCertFromFile(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadTLSCertFromFile failed: %v", err)
	}

	if tlsCert == nil {
		t.Fatal("LoadTLSCertFromFile returned nil certificate")
	}

	// Verify the certificate chain is valid
	if len(tlsCert.Certificate) == 0 {
		t.Errorf("TLS certificate chain is empty")
	}

	// Parse the leaf certificate from the chain
	leafCert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse leaf certificate from chain: %v", err)
	}

	// Verify it's the correct certificate
	if leafCert.Subject.CommonName != "worker-1" {
		t.Errorf("Loaded cert CN: got %q, want %q", leafCert.Subject.CommonName, "worker-1")
	}

	// Verify the private key is loaded
	if tlsCert.PrivateKey == nil {
		t.Errorf("Private key not loaded in TLS certificate")
	}
}

// TestLoadTLSCertFromFileMissingFiles tests loading non-existent cert files
func TestLoadTLSCertFromFileMissingFiles(t *testing.T) {
	_, err := LoadTLSCertFromFile("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Error("LoadTLSCertFromFile should fail for missing files")
	}
}

// TestVerifyCertSignedByCA tests certificate verification against CA
func TestVerifyCertSignedByCA(t *testing.T) {
	caDir1 := t.TempDir()
	caDir2 := t.TempDir()
	nodeDir := t.TempDir()

	// Generate first CA
	caCertPath1, caKeyPath1, err := GenerateSelfSignedCA(caDir1)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA (1) failed: %v", err)
	}

	// Generate second CA
	caCertPath2, _, err := GenerateSelfSignedCA(caDir2)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA (2) failed: %v", err)
	}

	// Generate node cert signed by first CA
	certPath, _, err := GenerateNodeCert(nodeDir, caCertPath1, caKeyPath1, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Parse node cert
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("Failed to read node cert: %v", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	nodeCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse node cert: %v", err)
	}

	// Parse first CA cert
	caCertPEM1, err := os.ReadFile(caCertPath1)
	if err != nil {
		t.Fatalf("Failed to read CA cert 1: %v", err)
	}
	caCertBlock1, _ := pem.Decode(caCertPEM1)
	caCert1, err := x509.ParseCertificate(caCertBlock1.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert 1: %v", err)
	}

	// Parse second CA cert
	caCertPEM2, err := os.ReadFile(caCertPath2)
	if err != nil {
		t.Fatalf("Failed to read CA cert 2: %v", err)
	}
	caCertBlock2, _ := pem.Decode(caCertPEM2)
	caCert2, err := x509.ParseCertificate(caCertBlock2.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA cert 2: %v", err)
	}

	// Verify node cert signed by first CA
	if !VerifyCertSignedByCA(nodeCert, caCert1) {
		t.Errorf("VerifyCertSignedByCA(nodeCert, caCert1) should return true")
	}

	// Verify node cert NOT signed by second CA
	if VerifyCertSignedByCA(nodeCert, caCert2) {
		t.Errorf("VerifyCertSignedByCA(nodeCert, caCert2) should return false")
	}
}

// TestNewMTLSMiddleware tests the mTLS middleware using httptest.NewRecorder
// with manually constructed TLS states.
func TestNewMTLSMiddleware(t *testing.T) {
	// Generate CA and certs for testing
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	caCertPath, caKeyPath, err := GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate a valid worker cert
	workerCertPath, _, err := GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Parse worker cert for TLS state construction
	workerCertPEM, err := os.ReadFile(workerCertPath)
	if err != nil {
		t.Fatalf("Failed to read worker cert: %v", err)
	}
	workerCertBlock, _ := pem.Decode(workerCertPEM)
	workerCert, err := x509.ParseCertificate(workerCertBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse worker cert: %v", err)
	}

	// Load CA cert for server
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

	// Create a self-signed cert for invalid cert tests
	selfSignedCert := createSelfSignedCert(t)
	selfSignedX509 := selfSignedCert.Leaf

	tests := []struct {
		name           string
		required       bool
		allowedCN      []string
		tlsState       *tls.ConnectionState
		expectedStatus int
	}{
		{
			name:           "dev mode (required=false) - no client cert",
			required:       false,
			allowedCN:      []string{},
			tlsState:       nil,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "dev mode (required=false) - with client cert (still allowed)",
			required:       false,
			allowedCN:      []string{},
			tlsState:       &tls.ConnectionState{PeerCertificates: []*x509.Certificate{workerCert}},
			expectedStatus: http.StatusOK,
		},
		{
			name:           "prod mode - no client cert",
			required:       true,
			allowedCN:      []string{},
			tlsState:       nil,
			expectedStatus: http.StatusForbidden,
		},
		{
			name:  "prod mode - valid client cert",
			required:       true,
			allowedCN:      []string{},
			tlsState:       &tls.ConnectionState{PeerCertificates: []*x509.Certificate{workerCert}},
			expectedStatus: http.StatusOK,
		},
		{
			name:  "prod mode - invalid client cert (not signed by CA)",
			required:       true,
			allowedCN:      []string{},
			tlsState:       &tls.ConnectionState{PeerCertificates: []*x509.Certificate{selfSignedX509}},
			expectedStatus: http.StatusForbidden,
		},
		{
			name:  "prod mode - allowedCN matches",
			required:       true,
			allowedCN:      []string{"worker-1"},
			tlsState:       &tls.ConnectionState{PeerCertificates: []*x509.Certificate{workerCert}},
			expectedStatus: http.StatusOK,
		},
		{
			name:  "prod mode - allowedCN does not match",
			required:       true,
			allowedCN:      []string{"router"},
			tlsState:       &tls.ConnectionState{PeerCertificates: []*x509.Certificate{workerCert}},
			expectedStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			middleware := NewMTLSMiddleware(caCertPool, tt.required, tt.allowedCN...)

			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("OK"))
			})

			handler := middleware(testHandler)

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.tlsState != nil {
				req.TLS = tt.tlsState
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d, body: %s", tt.expectedStatus, w.Code, w.Body.String())
			}
		})
	}
}

// TestNewMTLSMiddlewareWithRealTLS tests the middleware with actual TLS connections
func TestNewMTLSMiddlewareWithRealTLS(t *testing.T) {
	caDir := t.TempDir()
	nodeDir := t.TempDir()

	caCertPath, caKeyPath, err := GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}

	// Generate worker cert with CN "worker-1"
	workerCertPath, workerKetPath, err := GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Load CA cert for server
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

	// Create the middleware with required=true and allowedCN=["worker-1"]
	middleware := NewMTLSMiddleware(caCertPool, true, "worker-1")

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	handler := middleware(testHandler)

	// Create a TLS test server
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  caCertPool,
	}
	server.StartTLS()
	defer server.Close()

	// Load worker cert for client
	workerTLSCert, err := LoadTLSCertFromFile(workerCertPath, workerKetPath)
	if err != nil {
		t.Fatalf("LoadTLSCertFromFile failed: %v", err)
	}

	// Test with valid client cert
	t.Run("valid client cert gets 200", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{*workerTLSCert},
					// Don't verify server cert in test
					InsecureSkipVerify: true,
				},
			},
		}
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
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
		resp, err := client.Get(server.URL)
		if err != nil {
			// TLS handshake failure is expected when client cert is required
			if resp == nil {
				t.Logf("TLS handshake failed as expected (no client cert)")
				return
			}
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})
}

// TestNewFileAuditLogger tests the file-based audit logger
func TestNewFileAuditLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "audit.log")

	logger, err := NewFileAuditLogger(logPath)
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

// TestNewFileAuditLoggerMultipleEntries tests logging multiple entries
func TestNewFileAuditLoggerMultipleEntries(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "audit.log")

	logger, err := NewFileAuditLogger(logPath)
	if err != nil {
		t.Fatalf("NewFileAuditLogger failed: %v", err)
	}
	if logger == nil {
		t.Fatal("NewFileAuditLogger returned nil")
	}

	// Log multiple entries
	logger.Log(context.Background(), "first message", map[string]interface{}{"seq": 1})
	logger.Log(context.Background(), "second message", map[string]interface{}{"seq": 2})

	// Read the log file
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	lines := splitLines(string(content))
	if len(lines) < 2 {
		t.Fatalf("Expected at least 2 log lines, got %d", len(lines))
	}

	// Parse and verify first entry
	var firstEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &firstEntry); err != nil {
		t.Fatalf("Failed to parse first log entry: %v", err)
	}
	if firstEntry["message"] != "first message" {
		t.Errorf("First message: got %v, want 'first message'", firstEntry["message"])
	}
	if firstEntry["seq"] != float64(1) {
		t.Errorf("First seq: got %v, want 1", firstEntry["seq"])
	}

	// Parse and verify second entry
	var secondEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &secondEntry); err != nil {
		t.Fatalf("Failed to parse second log entry: %v", err)
	}
	if secondEntry["message"] != "second message" {
		t.Errorf("Second message: got %v, want 'second message'", secondEntry["message"])
	}
	if secondEntry["seq"] != float64(2) {
		t.Errorf("Second seq: got %v, want 2", secondEntry["seq"])
	}
}

// TestNewFileAuditLoggerNoOp tests that logger handles missing file gracefully
func TestNewFileAuditLoggerNoOp(t *testing.T) {
	// Use a path in a non-existent directory that we can't create
	_, err := NewFileAuditLogger("/nonexistent/path/audit.log")
	if err == nil {
		t.Fatal("Expected NewFileAuditLogger to return error for invalid path")
	}
}

// Helper to create a self-signed cert for testing invalid cert scenarios
func createSelfSignedCert(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "self-signed",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("Failed to create cert: %v", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{certBytes},
		PrivateKey:  key,
		Leaf:        cert,
	}
}

// splitLines splits content into lines, handling \r\n and \n
func splitLines(content string) []string {
	var lines []string
	current := ""
	for _, c := range content {
		if c == '\n' {
			if len(current) > 0 {
				// Trim trailing \r
				if len(current) > 0 && current[len(current)-1] == '\r' {
					current = current[:len(current)-1]
				}
				lines = append(lines, current)
				current = ""
			}
		} else {
			current += string(c)
		}
	}
	if len(current) > 0 {
		lines = append(lines, current)
	}
	return lines
}

// Ensure unused import doesn't cause issues
var _ = io.Discard
var _ = testLogger
