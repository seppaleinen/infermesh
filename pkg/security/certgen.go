package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateSelfSignedCA generates a self-signed CA certificate and key in dir.
// Returns certFile and keyFile paths.
func GenerateSelfSignedCA(dir string) (certFile, keyFile string, err error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create cert dir: %w", err)
	}

	ca := &x509.Certificate{
		SerialNumber: randomSerialNumber(),
		Subject: pkix.Name{
			Organization: []string{"InferMesh"},
			CommonName:   "InferMesh CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate CA key: %w", err)
	}

	caCertBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to create CA certificate: %w", err)
	}

	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	certOut, err := os.Create(certPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to write CA cert: %w", err)
	}
	defer func() { _ = certOut.Close() }()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: caCertBytes}); err != nil {
		return "", "", fmt.Errorf("failed to encode CA cert: %w", err)
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to write CA key: %w", err)
	}
	defer func() { _ = keyOut.Close() }()

	caKeyBytes, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal CA key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyBytes}); err != nil {
		return "", "", fmt.Errorf("failed to encode CA key: %w", err)
	}

	return certPath, keyPath, nil
}

// GenerateNodeCert generates a node certificate signed by the specified CA.
// CN is set to commonName; SANs include "localhost" and the machine's primary IPv4.
func GenerateNodeCert(dir, caCertPath, caKeyPath, commonName string) (certFile, keyFile string, err error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create cert dir: %w", err)
	}

	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read CA cert: %w", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return "", "", fmt.Errorf("failed to decode CA cert")
	}

	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse CA cert: %w", err)
	}

	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read CA key: %w", err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return "", "", fmt.Errorf("failed to decode CA key")
	}

	caPrivateKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse CA key: %w", err)
	}

	// Get primary IPv4
	ip := getPrimaryIPv4()

	node := &x509.Certificate{
		SerialNumber: randomSerialNumber(),
		Subject: pkix.Name{
			Organization: []string{"InferMesh"},
			CommonName:   commonName,
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{ip},
	}

	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate node key: %w", err)
	}

	nodeCertBytes, err := x509.CreateCertificate(rand.Reader, node, caCert, &nodeKey.PublicKey, caPrivateKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to create node certificate: %w", err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to write node cert: %w", err)
	}
	defer func() { _ = certOut.Close() }()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: nodeCertBytes}); err != nil {
		return "", "", fmt.Errorf("failed to encode node cert: %w", err)
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to write node key: %w", err)
	}
	defer func() { _ = keyOut.Close() }()

	nodeKeyBytes, err := x509.MarshalECPrivateKey(nodeKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal node key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: nodeKeyBytes}); err != nil {
		return "", "", fmt.Errorf("failed to encode node key: %w", err)
	}

	return certPath, keyPath, nil
}

// LoadTLSCertFromFile loads a TLS certificate from the given cert and key files.
func LoadTLSCertFromFile(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
	}
	return &cert, nil
}

// VerifyCertSignedByCA verifies that cert is signed by caCert.
func VerifyCertSignedByCA(cert *x509.Certificate, caCert *x509.Certificate) bool {
	err := cert.CheckSignatureFrom(caCert)
	return err == nil
}

// ValidateTLSConfig verifies that the mTLS cert, key, and CA files exist and
// are parseable before the server starts. It fails fast with a descriptive
// error so operators get a clear message instead of a crash inside
// ListenAndServeTLS with empty cert paths.
func ValidateTLSConfig(certFile, keyFile, caCertFile string) error {
	if certFile == "" {
		return fmt.Errorf("mtls-cert path is required in production mode")
	}
	if keyFile == "" {
		return fmt.Errorf("mtls-key path is required in production mode")
	}
	if caCertFile == "" {
		return fmt.Errorf("ca.crt path is required in production mode (use --cert-dir)")
	}

	if _, err := os.Stat(certFile); err != nil {
		return fmt.Errorf("mtls-cert %q: %w", certFile, err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		return fmt.Errorf("mtls-key %q: %w", keyFile, err)
	}
	if _, err := os.Stat(caCertFile); err != nil {
		return fmt.Errorf("ca.crt %q: %w", caCertFile, err)
	}

	// Verify the cert/key pair loads and the CA parses.
	if _, err := LoadTLSCertFromFile(certFile, keyFile); err != nil {
		return fmt.Errorf("failed to load mTLS certificate: %w", err)
	}

	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return fmt.Errorf("failed to read ca.crt: %w", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return fmt.Errorf("ca.crt %q is not a PEM-encoded certificate", caCertFile)
	}
	if block.Type != "CERTIFICATE" {
		return fmt.Errorf("ca.crt %q is a %q block, want CERTIFICATE", caCertFile, block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("failed to parse ca.crt: %w", err)
	}
	return nil
}

// LoadCACertPool loads the CA certificate from the given PEM file into an
// x509.CertPool. Returns an empty pool on error; callers should check the
// return value or call ValidateTLSConfig first.
func LoadCACertPool(caCertFile string) *x509.CertPool {
	pool := x509.NewCertPool()
	if caCertFile == "" {
		return pool
	}
	data, err := os.ReadFile(caCertFile)
	if err != nil {
		return pool
	}
	_ = pool.AppendCertsFromPEM(data)
	return pool
}

func getPrimaryIPv4() net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return net.ParseIP("127.0.0.1")
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP
		}
	}
	return net.ParseIP("127.0.0.1")
}

// randomSerialNumber generates a random 128-bit serial number for certificates.
func randomSerialNumber() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// Fallback to a timestamp-based serial if random generation fails
		return big.NewInt(time.Now().UnixNano())
	}
	return serial
}