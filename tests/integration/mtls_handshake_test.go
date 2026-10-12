package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestMTLSGenerateSelfSignedCA verifies GenerateSelfSignedCA produces a valid,
// parseable CA certificate and key on disk.
func TestMTLSGenerateSelfSignedCA(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(dir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}
	if caCertPath != filepath.Join(dir, "ca.crt") {
		t.Errorf("CA cert path = %q, want %q", caCertPath, filepath.Join(dir, "ca.crt"))
	}
	if caKeyPath != filepath.Join(dir, "ca.key") {
		t.Errorf("CA key path = %q, want %q", caKeyPath, filepath.Join(dir, "ca.key"))
	}

	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read CA cert: %v", err)
	}
	block, _ := pem.Decode(caCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("CA cert file is not a PEM CERTIFICATE block")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA {
		t.Error("CA cert IsCA = false, want true")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert missing KeyUsageCertSign")
	}
	if caCert.NotAfter.Before(time.Now()) {
		t.Errorf("CA cert already expired: NotAfter = %v", caCert.NotAfter)
	}
	if caCert.Subject.CommonName != "InferMesh CA" {
		t.Errorf("CA cert CN = %q, want \"InferMesh CA\"", caCert.Subject.CommonName)
	}

	// Key file must be a parseable EC private key.
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		t.Fatalf("read CA key: %v", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("CA key file has no PEM block")
	}
	if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("parse CA key: %v", err)
	}
}

func extKeyUsageHas(ekus []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, e := range ekus {
		if e == want {
			return true
		}
	}
	return false
}

// TestMTLSGenerateNodeCertSignedByCA verifies GenerateNodeCert produces a cert
// signed by the given CA, with the requested CN, and that
// VerifyCertSignedByCA distinguishes a genuine signature from a wrong CA.
func TestMTLSGenerateNodeCertSignedByCA(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}

	nodeDir := t.TempDir()
	nodeCertPath, nodeKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert: %v", err)
	}

	// LoadTLSCertFromFile must accept the generated pair.
	tlsCert, err := security.LoadTLSCertFromFile(nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatalf("LoadTLSCertFromFile: %v", err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("loaded TLS cert has no certificate chain")
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse node leaf: %v", err)
	}
	if leaf.Subject.CommonName != "worker-1" {
		t.Errorf("node cert CN = %q, want worker-1", leaf.Subject.CommonName)
	}
	if leaf.IsCA {
		t.Error("node cert IsCA = true, want false (leaf, not CA)")
	}
	for _, ek := range leaf.ExtKeyUsage {
		if ek == x509.ExtKeyUsageClientAuth || ek == x509.ExtKeyUsageServerAuth {
			// both are expected
			_ = ek
		}
	}
	if !extKeyUsageHas(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) || !extKeyUsageHas(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Errorf("node cert ExtKeyUsage = %v, want client+server auth", leaf.ExtKeyUsage)
	}
	_ = leaf // leaf referenced below via VerifyCertSignedByCA
	// SANs include "localhost" (clients pin ServerName=localhost).
	found := false
	for _, d := range leaf.DNSNames {
		if d == "localhost" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("node cert DNSNames = %v, want to include \"localhost\"", leaf.DNSNames)
	}

	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read CA cert: %v", err)
	}
	caBlock, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !security.VerifyCertSignedByCA(leaf, caCert) {
		t.Error("VerifyCertSignedByCA(node, correct CA) = false, want true")
	}

	// Wrong CA: a second CA must NOT verify the node cert.
	otherCADir := t.TempDir()
	otherCaCertPath, _, err := security.GenerateSelfSignedCA(otherCADir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA (second): %v", err)
	}
	otherCAPEM, err := os.ReadFile(otherCaCertPath)
	if err != nil {
		t.Fatalf("read second CA cert: %v", err)
	}
	otherCABlock, _ := pem.Decode(otherCAPEM)
	otherCA, err := x509.ParseCertificate(otherCABlock.Bytes)
	if err != nil {
		t.Fatalf("parse second CA cert: %v", err)
	}
	if security.VerifyCertSignedByCA(leaf, otherCA) {
		t.Error("VerifyCertSignedByCA(node, wrong CA) = true, want false")
	}
}

// TestMTLSValidateTLSConfig verifies ValidateTLSConfig accepts a consistent
// generated set and rejects missing/broken files.
func TestMTLSValidateTLSConfig(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}
	nodeDir := t.TempDir()
	nodeCertPath, nodeKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert: %v", err)
	}

	if err := security.ValidateTLSConfig(nodeCertPath, nodeKeyPath, caCertPath); err != nil {
		t.Fatalf("ValidateTLSConfig(valid) = %v, want nil", err)
	}

	cases := []struct {
		name     string
		cert, key, ca string
	}{
		{"missing cert", "", nodeKeyPath, caCertPath},
		{"missing key", nodeCertPath, "", caCertPath},
		{"missing ca", nodeCertPath, nodeKeyPath, ""},
		{"nonexistent cert", filepath.Join(nodeDir, "nope.pem"), nodeKeyPath, caCertPath},
		{"corrupt ca", nodeCertPath, nodeKeyPath, filepath.Join(caDir, "bogus")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ca != "" {
				_ = os.WriteFile(tc.ca, []byte("not a cert"), 0o600)
			}
			if err := security.ValidateTLSConfig(tc.cert, tc.key, tc.ca); err == nil {
				t.Errorf("ValidateTLSConfig(%q,%q,%q) = nil, want error", tc.cert, tc.key, tc.ca)
			}
		})
	}
}

// generateExpiredNodeCert builds a leaf cert signed by the given CA whose
// validity window lies entirely in the past, plus its key, written as PEM.
func generateExpiredNodeCert(t *testing.T, dir, caCertPath, caKeyPath, commonName string) (certFile, keyFile string) {
	t.Helper()

	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read CA cert: %v", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		t.Fatalf("read CA key: %v", err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("random serial: %v", err)
	}

	leaf := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"InferMesh"}, CommonName: commonName},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-1 * time.Hour), // expired
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafBytes, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create expired leaf: %v", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	certFile = filepath.Join(dir, "expired-cert.pem")
	cf, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: leafBytes}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = cf.Close()

	keyBytes, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyFile = filepath.Join(dir, "expired-key.pem")
	kf, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = kf.Close()
	return certFile, keyFile
}

// TestMTLSHandshakeValidCert verifies an mTLS handshake succeeds end-to-end
// with certs generated by the production helpers: server presents a node cert,
// client presents a node cert, both verified against the generated CA.
func TestMTLSHandshakeValidCert(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}
	serverDir := t.TempDir()
	serverCertPath, serverKeyPath, err := security.GenerateNodeCert(serverDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert (server): %v", err)
	}
	clientDir := t.TempDir()
	clientCertPath, clientKeyPath, err := security.GenerateNodeCert(clientDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert (client): %v", err)
	}

	serverTLSCert, err := security.LoadTLSCertFromFile(serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	clientTLSCert, err := security.LoadTLSCertFromFile(clientCertPath, clientKeyPath)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA to pool")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{*serverTLSCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{*clientTLSCert},
				RootCAs:      caPool,
				ServerName:   "localhost",
			},
		},
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS request with valid cert failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("mTLS request with valid cert: status = %d, want 200", resp.StatusCode)
	}
}

// TestMTLSHandshakeWrongCert verifies that a client presenting a cert signed
// by a DIFFERENT CA is rejected (TLS handshake failure).
func TestMTLSHandshakeWrongCert(t *testing.T) {
	// Trusted CA (server side) and a rogue CA (client cert).
	trustedCADir := t.TempDir()
	trustedCaCertPath, trustedCaKeyPath, err := security.GenerateSelfSignedCA(trustedCADir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA (trusted): %v", err)
	}
	rogueCADir := t.TempDir()
	rogueCaCertPath, rogueCaKeyPath, err := security.GenerateSelfSignedCA(rogueCADir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA (rogue): %v", err)
	}

	serverDir := t.TempDir()
	serverCertPath, serverKeyPath, err := security.GenerateNodeCert(serverDir, trustedCaCertPath, trustedCaKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert (server): %v", err)
	}
	rogueClientDir := t.TempDir()
	rogueCertPath, rogueKeyPath, err := security.GenerateNodeCert(rogueClientDir, rogueCaCertPath, rogueCaKeyPath, "worker-rogue")
	if err != nil {
		t.Fatalf("GenerateNodeCert (rogue client): %v", err)
	}

	serverTLSCert, err := security.LoadTLSCertFromFile(serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	rogueTLSCert, err := security.LoadTLSCertFromFile(rogueCertPath, rogueKeyPath)
	if err != nil {
		t.Fatalf("load rogue client cert: %v", err)
	}
	caPEM, err := os.ReadFile(trustedCaCertPath)
	if err != nil {
		t.Fatalf("read trusted CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA to pool")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{*serverTLSCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{*rogueTLSCert},
				RootCAs:      caPool,
				ServerName:   "localhost",
			},
		},
	}
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("mTLS request with wrong-CA client cert succeeded (status %d), want handshake failure", resp.StatusCode)
	}
	if !isTLSError(err) {
		t.Errorf("expected TLS error, got: %v", err)
	}
}

// TestMTLSHandshakeExpiredCert verifies that a client whose (otherwise
// correctly signed) certificate has expired is rejected at the TLS handshake.
func TestMTLSHandshakeExpiredCert(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}
	serverDir := t.TempDir()
	serverCertPath, serverKeyPath, err := security.GenerateNodeCert(serverDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert (server): %v", err)
	}
	expiredDir := t.TempDir()
	expiredCertPath, expiredKeyPath := generateExpiredNodeCert(t, expiredDir, caCertPath, caKeyPath, "worker-expired")

	serverTLSCert, err := security.LoadTLSCertFromFile(serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	expiredTLSCert, err := security.LoadTLSCertFromFile(expiredCertPath, expiredKeyPath)
	if err != nil {
		t.Fatalf("load expired client cert: %v", err)
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA to pool")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{*serverTLSCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{*expiredTLSCert},
				RootCAs:      caPool,
				ServerName:   "localhost",
			},
		},
	}
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("mTLS request with expired client cert succeeded (status %d), want handshake failure", resp.StatusCode)
	}
	if !isTLSError(err) {
		t.Errorf("expected TLS error for expired cert, got: %v", err)
	}
	// The failure must be about expiry / cert verification, not a transport problem.
	if !strings.Contains(strings.ToLower(err.Error()), "expired") &&
		!strings.Contains(strings.ToLower(err.Error()), "certificate") &&
		!strings.Contains(strings.ToLower(err.Error()), "handshake") {
		t.Errorf("unexpected error for expired cert: %v", err)
	}
}

// isTLSError reports whether err is a TLS-level failure (handshake, cert, or
// any *tls.CertificateVerificationError / *tls.RecordLayerError).
func isTLSError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var recordErr *tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "tls") || strings.Contains(s, "handshake") ||
		strings.Contains(s, "certificate") || strings.Contains(s, "remote error") ||
		strings.Contains(s, "expired")
}

// TestMTLSNodeCertLoadableByTLSStack verifies the generated cert/key pair is
// consumable by the standard TLS stack (tls.LoadX509KeyPair) — the same call
// the router and worker make when starting in prod mode.
func TestMTLSNodeCertLoadableByTLSStack(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}
	nodeDir := t.TempDir()
	nodeCertPath, nodeKeyPath, err := security.GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(nodeCertPath, nodeKeyPath); err != nil {
		t.Errorf("tls.LoadX509KeyPair failed on generated cert: %v", err)
	}
	// Cert paths must exist and be non-empty.
	for _, p := range []string{nodeCertPath, nodeKeyPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("stat %s: %v", p, err)
		} else if info.Size() == 0 {
			t.Errorf("%s is empty", p)
		}
	}
	if !strings.HasPrefix(nodeCertPath, nodeDir) {
		t.Errorf("cert path %q not under requested dir %q", nodeCertPath, nodeDir)
	}
}
