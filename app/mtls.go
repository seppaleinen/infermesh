package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// buildTLSClient builds an mTLS http.Client from the supervisor's minted
// client cert and CA cert dir. Returns nil when no client cert is minted
// (dev mode or no CA key). The client verifies the router's server
// certificate against the CA pool — no InsecureSkipVerify (issue #103 R1).
func (s *Supervisor) buildTLSClient() *http.Client {
	certPath, keyPath := s.GetClientCertPaths()
	caCertDir := s.GetCACertDir()
	if certPath == "" || keyPath == "" || caCertDir == "" {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil
	}
	caCertFile := filepath.Join(caCertDir, "ca.crt")
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   10 * time.Second,
	}
}

// updateRouterClientTransport injects the Supervisor's mTLS transport into
// the desktop RouterClient when the router is running in prod mode, or
// clears it when the router stops. This is called from StartRouter/StopRouter
// so the UI's live poll always talks to the router over the right transport
// (issue #103 R1).
func (s *Supervisor) updateRouterClientTransport() {
	if s.routerClient == nil {
		return
	}
	s.routerClient.SetTransport(s.buildTLSClient())
}

// writeCertDir creates a temp directory containing ca.crt from the
// mtls/cert secret. Returns the dir path (empty if no CA cert provisioned).
// The router reads ca.crt from --cert-dir to verify client certificates.
func (s *Supervisor) writeCertDir() (string, error) {
	caCert, err := s.resolveSecret(secretRefMTLSCACert)
	if err != nil {
		return "", err
	}
	if caCert == "" {
		return "", nil
	}
	if err := os.MkdirAll(s.logsDir, 0o755); err != nil {
		return "", fmt.Errorf("ensure log dir: %w", err)
	}
	// Create a temp directory and write ca.crt into it so the router can
	// find it under --cert-dir/ca.crt.
	dir, err := os.MkdirTemp(s.logsDir, "infermesh-ca-dir-")
	if err != nil {
		return "", fmt.Errorf("create CA dir: %w", err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte(caCert), 0o600); err != nil {
		return "", fmt.Errorf("write ca.crt: %w", err)
	}
	s.mu.Lock()
	s.tempFiles = append(s.tempFiles, dir)
	s.caCertDir = dir
	s.mu.Unlock()
	return dir, nil
}

// writeClientCert mints a desktop client certificate signed by the CA key
// (mtls/ca-key) using security.GenerateNodeCert. Returns empty paths when
// the CA key is absent. The cert/key PEM paths are stored in
// s.clientCertPaths so RouterClient can build a TLS transport.
func (s *Supervisor) writeClientCert(settings Settings, caCertPath string) (certPath, keyPath string, err error) {
	caKey, err := s.resolveSecret(secretRefMTLSCAKey)
	if err != nil {
		return "", "", err
	}
	if caKey == "" {
		return "", "", nil
	}
	if err := os.MkdirAll(s.logsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("ensure log dir: %w", err)
	}
	// GenerateNodeCert reads the CA key from disk; write it to a temp PEM.
	caKeyPath, err := writeTempFile(s.logsDir, "infermesh-ca-key-*.pem", caKey)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.tempFiles = append(s.tempFiles, caKeyPath)
	s.mu.Unlock()

	clientDir, err := os.MkdirTemp(s.logsDir, "infermesh-client-cert-")
	if err != nil {
		return "", "", fmt.Errorf("create client cert dir: %w", err)
	}
	certPath, keyPath, err = security.GenerateNodeCert(clientDir, caCertPath, caKeyPath, settings.ClientCertCN)
	if err != nil {
		return "", "", fmt.Errorf("generate client cert: %w", err)
	}
	s.mu.Lock()
	s.tempFiles = append(s.tempFiles, certPath, keyPath)
	s.clientCertPaths[0] = certPath
	s.clientCertPaths[1] = keyPath
	s.mu.Unlock()
	return certPath, keyPath, nil
}

// GetClientCertPaths returns the minted desktop client cert/key PEM paths.
// Empty strings mean no client cert was minted (dev mode or no CA key).
func (s *Supervisor) GetClientCertPaths() (certPath, keyPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientCertPaths[0], s.clientCertPaths[1]
}

// clearClientCertPaths resets the stored client cert paths (called on StopRouter).
func (s *Supervisor) clearClientCertPaths() {
	s.mu.Lock()
	s.clientCertPaths[0] = ""
	s.clientCertPaths[1] = ""
	s.mu.Unlock()
}

// GetCACertDir returns the temporary directory containing ca.crt for the
// router to verify client certificates. Empty string means no CA cert
// provisioned (dev mode).
func (s *Supervisor) GetCACertDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caCertDir
}

// clearCACertDir resets the stored CA cert dir (called on StopRouter).
func (s *Supervisor) clearCACertDir() {
	s.mu.Lock()
	s.caCertDir = ""
	s.mu.Unlock()
}

// writeMTLSFiles resolves the mtls/cert and mtls/key secrets from the keyring
// into temp files (mode 0600) and records them for cleanup. Returns empty
// paths when the secrets are absent. The file contents are never logged.
func (s *Supervisor) writeMTLSFiles() (certPath, keyPath string, err error) {
	cert, err := s.resolveSecret(secretRefMTLSCert)
	if err != nil {
		return "", "", err
	}
	key, err := s.resolveSecret(secretRefMTLSKey)
	if err != nil {
		return "", "", err
	}
	if cert == "" && key == "" {
		return "", "", nil
	}
	if err := os.MkdirAll(s.logsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("ensure log dir: %w", err)
	}
	if cert != "" {
		if certPath, err = writeTempFile(s.logsDir, "infermesh-mtls-cert-*.pem", cert); err != nil {
			return "", "", err
		}
		s.mu.Lock()
		s.tempFiles = append(s.tempFiles, certPath)
		s.mu.Unlock()
	}
	if key != "" {
		if keyPath, err = writeTempFile(s.logsDir, "infermesh-mtls-key-*.pem", key); err != nil {
			return "", "", err
		}
		s.mu.Lock()
		s.tempFiles = append(s.tempFiles, keyPath)
		s.mu.Unlock()
	}
	return certPath, keyPath, nil
}

// cleanupTempFiles removes any temp files created for mTLS secrets.
func (s *Supervisor) cleanupTempFiles() {
	s.mu.Lock()
	files := s.tempFiles
	s.tempFiles = nil
	s.mu.Unlock()
	for _, f := range files {
		// Use RemoveAll to clean up both files and directories (MkdirTemp dirs).
		_ = os.RemoveAll(f)
	}
}

// writeTempFile writes data to a 0600 temp file in dir with the given glob
// pattern and returns the path.
func writeTempFile(dir, pattern, data string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	name := f.Name()
	if _, err := io.WriteString(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}