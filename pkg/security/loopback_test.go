package security

import (
	"os"
	"testing"
)

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"empty", "", false},
		{"v4 loopback", "127.0.0.1:8081", true},
		{"v6 loopback", "[::1]:8081", true},
		{"empty host", ":8081", true},
		{"v6 any", "[::]:8081", true},
		{"v4 any", "0.0.0.0:8081", false},
		{"non-loopback ip", "192.168.1.1:8081", false},
		{"hostname localhost", "localhost:8081", true},
		{"invalid address", "not-an-address", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLoopbackAddr(tt.addr); got != tt.want {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestValidateTLSConfig_PEMBlockType(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA: %v", err)
	}

	// A valid cert/key pair.
	nodeDir := t.TempDir()
	certPath, keyPath, err := GenerateNodeCert(nodeDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert: %v", err)
	}

	// Point the CA at the key file (PEM "EC PRIVATE KEY" block) → wrong block type.
	if err := ValidateTLSConfig(certPath, keyPath, caKeyPath); err == nil {
		t.Fatal("ValidateTLSConfig with wrong CA PEM block type: expected error, got nil")
	}

	// Point the CA at a non-PEM file → decode failure.
	plainPath := nodeDir + "/plain.txt"
	if err := os.WriteFile(plainPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	if err := ValidateTLSConfig(certPath, keyPath, plainPath); err == nil {
		t.Fatal("ValidateTLSConfig with non-PEM CA file: expected error, got nil")
	}
}
