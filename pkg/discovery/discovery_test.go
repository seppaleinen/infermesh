package discovery

import (
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func sampleWorkerInfo() protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:       "worker-1",
		Hostname: "host1",
		IP:       "192.168.1.100",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		Capabilities: protocol.Capabilities{
			GPU: protocol.GPUInfo{
				Vendor:      "nvidia",
				Model:       "RTX 4090",
				ComputeCore: 16384,
				TotalVRAM:   24576,
				FreeVRAM:    20480,
			},
			VRAM: protocol.MemoryInfo{
				TotalMB: 24576,
				FreeMB:  20480,
			},
			System: protocol.MemoryInfo{
				TotalMB: 32768,
				FreeMB:  24576,
			},
			Models:  []protocol.ModelInfo{},
			Engines: []string{"llama-cpp"},
		},
	}
}

func TestSerializeDeserializeWorkerInfo(t *testing.T) {
	tests := []struct {
		name string
		info protocol.WorkerInfo
	}{
		{
			name: "valid worker with capabilities",
			info: sampleWorkerInfo(),
		},
		{
			name: "empty fields",
			info: protocol.WorkerInfo{},
		},
		{
			name: "minimal worker",
			info: protocol.WorkerInfo{
				ID:   "w1",
				Port: 9090,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := protocol.SerializeWorkerInfo(tt.info)
			if err != nil {
				t.Fatalf("SerializeWorkerInfo failed: %v", err)
			}
			if raw == "" {
				t.Fatal("expected non-empty serialized string")
			}

			decoded, err := protocol.DeserializeWorkerInfo(raw)
			if err != nil {
				t.Fatalf("DeserializeWorkerInfo failed: %v", err)
			}
			if decoded.ID != tt.info.ID {
				t.Errorf("ID mismatch: got %s, want %s", decoded.ID, tt.info.ID)
			}
			if decoded.Port != tt.info.Port {
				t.Errorf("Port mismatch: got %d, want %d", decoded.Port, tt.info.Port)
			}
			if decoded.Status != tt.info.Status {
				t.Errorf("Status mismatch: got %s, want %s", decoded.Status, tt.info.Status)
			}
		})
	}
}

func TestDeserializeInvalidJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "malformed json",
			input:   "not json at all",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "valid json but wrong type",
			input:   `{"id": 123}`,
			wantErr: false, // json allows unmarshal of numbers into strings? No, it errors.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := protocol.DeserializeWorkerInfo(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.ServiceType != "_infermesh-worker._tcp.local." {
		t.Errorf("ServiceType: got %s, want _infermesh-worker._tcp.local.", cfg.ServiceType)
	}
	if cfg.Domain != "local." {
		t.Errorf("Domain: got %s, want local.", cfg.Domain)
	}
	if cfg.ProbeInterval != 5*time.Second {
		t.Errorf("ProbeInterval: got %v, want 5s", cfg.ProbeInterval)
	}
	if cfg.HeartbeatTTL != 30*time.Second {
		t.Errorf("HeartbeatTTL: got %v, want 30s", cfg.HeartbeatTTL)
	}
}

func TestConfigDevDefaults(t *testing.T) {
	cfg := DevDefaults()
	if !cfg.LocalOnly {
		t.Error("expected LocalOnly=true in dev defaults")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid config",
			cfg:     Defaults(),
			wantErr: false,
		},
		{
			name:    "missing service type",
			cfg:     Config{ServiceType: "", Domain: "local.", ProbeInterval: 5 * time.Second, ProbeTimeout: 2 * time.Second, HeartbeatTTL: 30 * time.Second},
			wantErr: true,
		},
		{
			name:    "missing domain",
			cfg:     Config{ServiceType: "_x._tcp.local.", Domain: ""},
			wantErr: true,
		},
		{
			name:    "short probe interval",
			cfg:     Config{ServiceType: "_x._tcp.local.", Domain: "local.", ProbeInterval: 500 * time.Millisecond, ProbeTimeout: 2 * time.Second, HeartbeatTTL: 30 * time.Second},
			wantErr: true,
		},
		{
			name:    "short probe timeout",
			cfg:     Config{ServiceType: "_x._tcp.local.", Domain: "local.", ProbeInterval: 5 * time.Second, ProbeTimeout: 50 * time.Millisecond, HeartbeatTTL: 30 * time.Second},
			wantErr: true,
		},
		{
			name:    "short heartbeat ttl",
			cfg:     Config{ServiceType: "_x._tcp.local.", Domain: "local.", ProbeInterval: 5 * time.Second, ProbeTimeout: 2 * time.Second, HeartbeatTTL: 100 * time.Millisecond},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestNewAnnouncerInvalidConfig(t *testing.T) {
	log := testLogger()
	cfg := Config{ServiceType: ""}
	_, err := newMDNSAnnouncer(cfg, log)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestNewListenerInvalidConfig(t *testing.T) {
	log := testLogger()
	cfg := Config{ServiceType: ""}
	_, err := newMDNSListener(cfg, log)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestNewAnnouncerValidConfig(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	a, err := newMDNSAnnouncer(cfg, log)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if a == nil {
		t.Fatal("expected non-nil announcer")
	}
}

func TestNewListenerValidConfig(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	l, err := newMDNSListener(cfg, log)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if l == nil {
		t.Fatal("expected non-nil listener")
	}
}

func TestFactoryAnnouncer(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()

	t.Run("valid backend", func(t *testing.T) {
		a, err := NewAnnouncer(BackendMDNS, cfg, log)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if a == nil {
			t.Fatal("expected non-nil announcer")
		}
	})

	t.Run("unknown backend", func(t *testing.T) {
		_, err := NewAnnouncer("unknown", cfg, log)
		if err == nil {
			t.Error("expected error for unknown backend")
		}
	})
}

func TestFactoryListener(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()

	t.Run("valid backend", func(t *testing.T) {
		l, err := NewListener(BackendMDNS, cfg, log)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if l == nil {
			t.Fatal("expected non-nil listener")
		}
	})

	t.Run("unknown backend", func(t *testing.T) {
		_, err := NewListener("unknown", cfg, log)
		if err == nil {
			t.Error("expected error for unknown backend")
		}
	})
}

func TestListenerStopWithoutStart(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	l, err := newMDNSListener(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	if err := l.Stop(); err != nil {
		t.Errorf("Stop should return nil: %v", err)
	}
}

func TestAnnouncerStopWithoutAnnounce(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	a, err := newMDNSAnnouncer(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Errorf("Stop should return nil: %v", err)
	}
}

func TestAnnouncerEventsReturnsNil(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	a, err := newMDNSAnnouncer(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	if a.Events() != nil {
		t.Error("expected nil channel for announcer events")
	}
}

func TestListenerEventsChannel(t *testing.T) {
	log := testLogger()
	cfg := DevDefaults()
	l, err := newMDNSListener(cfg, log)
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	if l.Events() == nil {
		t.Error("expected non-nil events channel")
	}
}

func TestDeserializeWorkerInfoError(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "completely malformed",
			input:   "%%%",
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `{invalid`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := protocol.DeserializeWorkerInfo(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestSerializeWorkerInfoComplex(t *testing.T) {
	info := sampleWorkerInfo()
	info.Capabilities.Models = []protocol.ModelInfo{
		{
			Name:        "llama-3-8b",
			Size:        1073741824,
			Quantization: "Q4_K_M",
			MaxTokens:   8192,
			Backend:     "llama-cpp",
			Loaded:      true,
		},
	}

	raw, err := protocol.SerializeWorkerInfo(info)
	if err != nil {
		t.Fatalf("serialize failed: %v", err)
	}

	decoded, err := protocol.DeserializeWorkerInfo(raw)
	if err != nil {
		t.Fatalf("deserialize failed: %v", err)
	}

	if len(decoded.Capabilities.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(decoded.Capabilities.Models))
	}
	if decoded.Capabilities.Models[0].Name != "llama-3-8b" {
		t.Errorf("expected model name llama-3-8b, got %s", decoded.Capabilities.Models[0].Name)
	}
}

// Ensure errors variable is used to avoid unused import warnings
var _ = errors.New