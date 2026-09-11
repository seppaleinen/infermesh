package main

import (
	"testing"
)

func TestParseRouterFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		f, err := parseRouterFlags(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Addr != ":8080" {
			t.Errorf("default addr = %q, want :8080", f.Addr)
		}
		if f.Port != 8081 {
			t.Errorf("default port = %d, want 8081", f.Port)
		}
		if f.Backend != "llama-cpp" {
			t.Errorf("default backend = %q, want llama-cpp", f.Backend)
		}
		if !f.EnableHealthChecks {
			t.Error("default enable-health-checks = false, want true")
		}
		if f.Worker || f.DevMode || f.ProdMode {
			t.Error("boolean flags should default to false")
		}
	})

	t.Run("all flags", func(t *testing.T) {
		f, err := parseRouterFlags([]string{
			"--dev-mode",
			"--mtls-cert", "/tmp/cert.pem",
			"--mtls-key", "/tmp/key.pem",
			"--cert-dir", "/tmp/certs",
			"--api-key", "secret",
			"--addr", "127.0.0.1:9000",
			"--worker",
			"--port", "9001",
			"--backend", "lmstudio",
			"--model-path", "/tmp/model.bin",
			"--enable-health-checks=false",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.DevMode {
			t.Error("dev-mode = false, want true")
		}
		if f.MTLSCert != "/tmp/cert.pem" || f.MTLSKey != "/tmp/key.pem" || f.CertDir != "/tmp/certs" || f.APIKey != "secret" {
			t.Errorf("mTLS/API key flags not parsed: %+v", f)
		}
		if f.Addr != "127.0.0.1:9000" {
			t.Errorf("addr = %q, want 127.0.0.1:9000", f.Addr)
		}
		if !f.Worker || f.Port != 9001 || f.Backend != "lmstudio" || f.ModelPath != "/tmp/model.bin" {
			t.Errorf("worker flags not parsed: %+v", f)
		}
		if f.EnableHealthChecks {
			t.Error("enable-health-checks = true, want false")
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		if _, err := parseRouterFlags([]string{"--nope"}); err == nil {
			t.Error("expected error for unknown flag")
		}
	})
}

func TestParseWorkerFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		f, err := parseWorkerFlags(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Port != 8081 {
			t.Errorf("default port = %d, want 8081", f.Port)
		}
		if f.Backend != "llama-cpp" {
			t.Errorf("default backend = %q, want llama-cpp", f.Backend)
		}
		if !f.EnableHealthChecks {
			t.Error("default enable-health-checks = false, want true")
		}
		if f.Capabilities || f.DevMode || f.ProdMode {
			t.Error("boolean flags should default to false")
		}
	})

	t.Run("all flags", func(t *testing.T) {
		f, err := parseWorkerFlags([]string{
			"--dev-mode",
			"--mtls-cert", "/tmp/cert.pem",
			"--mtls-key", "/tmp/key.pem",
			"--port", "9001",
			"--cert-dir", "/tmp/certs",
			"--model-path", "/tmp/model.bin",
			"--backend", "ollama",
			"--router", "http://127.0.0.1:8080",
			"--enable-health-checks=false",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.DevMode || f.MTLSCert != "/tmp/cert.pem" || f.MTLSKey != "/tmp/key.pem" || f.CertDir != "/tmp/certs" {
			t.Errorf("flags not parsed: %+v", f)
		}
		if f.Port != 9001 || f.ModelPath != "/tmp/model.bin" || f.Backend != "ollama" || f.Router != "http://127.0.0.1:8080" {
			t.Errorf("flags not parsed: %+v", f)
		}
		if f.EnableHealthChecks {
			t.Error("enable-health-checks = true, want false")
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		if _, err := parseWorkerFlags([]string{"--nope"}); err == nil {
			t.Error("expected error for unknown flag")
		}
	})
}

func TestListenPort(t *testing.T) {
	tests := []struct {
		addr    string
		want    int
		wantErr bool
	}{
		{":8080", 8080, false},
		{"127.0.0.1:9000", 9000, false},
		{"0.0.0.0:1", 1, false},
		{"localhost:65535", 65535, false},
		{"8080", 0, true},          // no port separator
		{"host:0", 0, true},        // out of range
		{"host:70000", 0, true},    // out of range
		{"host:abc", 0, true},      // non-numeric
	}
	for _, tt := range tests {
		got, err := listenPort(tt.addr)
		if tt.wantErr {
			if err == nil {
				t.Errorf("listenPort(%q): expected error, got %d", tt.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("listenPort(%q): unexpected error: %v", tt.addr, err)
			continue
		}
		if got != tt.want {
			t.Errorf("listenPort(%q) = %d, want %d", tt.addr, got, tt.want)
		}
	}
}

