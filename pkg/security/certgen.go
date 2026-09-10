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