package protocol

import (
	"strings"
	"testing"
)

func TestSerializeDiscoveryInfo_SizeUnderLimit(t *testing.T) {
	info := DiscoveryInfo{
		ID:       "worker-1",
		Hostname: "gpu-server-01",
		IP:       "192.168.1.100",
		Port:     8081,
		Version:  "v1.0.0",
		Status:   StatusAvailable,
	}

	s, err := SerializeDiscoveryInfo(info)
	if err != nil {
		t.Fatalf("SerializeDiscoveryInfo failed: %v", err)
	}
	if s == "" {
		t.Fatal("expected non-empty serialized string")
	}
	if len(s) > MaxTXTRecordLength {
		t.Errorf("serialized size %d exceeds DNS limit %d", len(s), MaxTXTRecordLength)
	}

	// Verify round-trip
	decoded, err := DeserializeDiscoveryInfo(s)
	if err != nil {
		t.Fatalf("DeserializeDiscoveryInfo failed: %v", err)
	}
	if decoded.ID != info.ID {
		t.Errorf("ID mismatch: got %s, want %s", decoded.ID, info.ID)
	}
	if decoded.Hostname != info.Hostname {
		t.Errorf("Hostname mismatch: got %s, want %s", decoded.Hostname, info.Hostname)
	}
	if decoded.IP != info.IP {
		t.Errorf("IP mismatch: got %s, want %s", decoded.IP, info.IP)
	}
	if decoded.Port != info.Port {
		t.Errorf("Port mismatch: got %d, want %d", decoded.Port, info.Port)
	}
	if decoded.Version != info.Version {
		t.Errorf("Version mismatch: got %s, want %s", decoded.Version, info.Version)
	}
	if decoded.Status != info.Status {
		t.Errorf("Status mismatch: got %s, want %s", decoded.Status, info.Status)
	}
}

func TestSerializeDiscoveryInfo_OverLimit(t *testing.T) {
	// Create a DiscoveryInfo with very long fields that exceeds 255 bytes
	info := DiscoveryInfo{
		ID:       strings.Repeat("x", 100),
		Hostname: strings.Repeat("y", 100),
		IP:       "192.168.1.100",
		Port:     8081,
		Version:  "v1.0.0",
		Status:   StatusAvailable,
	}

	_, err := SerializeDiscoveryInfo(info)
	if err == nil {
		t.Fatal("expected error for oversized TXT record")
	}
	if !strings.Contains(err.Error(), "exceeds DNS limit") {
		t.Errorf("expected DNS limit error, got: %v", err)
	}
}

func TestSerializeDeserializeDiscoveryInfo(t *testing.T) {
	tests := []struct {
		name string
		info DiscoveryInfo
	}{
		{
			name: "fully populated",
			info: DiscoveryInfo{
				ID:       "worker-42",
				Hostname: "gpu-box-01",
				IP:       "10.0.0.5",
				Port:     9090,
				Version:  "v2.0",
				Status:   StatusBusy,
			},
		},
		{
			name: "minimal fields",
			info: DiscoveryInfo{
				ID:   "w1",
				Port: 8080,
			},
		},
		{
			name: "with unavailable status",
			info: DiscoveryInfo{
				ID:       "dead-worker",
				Hostname: "old-server",
				IP:       "172.16.0.1",
				Port:     8081,
				Version:  "v0.1",
				Status:   StatusUnavailable,
			},
		},
		{
			name: "status omitted (zero value)",
			info: DiscoveryInfo{
				ID:       "no-status",
				Hostname: "host",
				IP:       "127.0.0.1",
				Port:     8081,
				Version:  "v1",
			},
		},
		{
			name: "empty struct",
			info: DiscoveryInfo{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := SerializeDiscoveryInfo(tt.info)
			if err != nil {
				t.Fatalf("SerializeDiscoveryInfo failed: %v", err)
			}

			decoded, err := DeserializeDiscoveryInfo(s)
			if err != nil {
				t.Fatalf("DeserializeDiscoveryInfo failed: %v", err)
			}

			if decoded.ID != tt.info.ID {
				t.Errorf("ID: got %s, want %s", decoded.ID, tt.info.ID)
			}
			if decoded.Hostname != tt.info.Hostname {
				t.Errorf("Hostname: got %s, want %s", decoded.Hostname, tt.info.Hostname)
			}
			if decoded.IP != tt.info.IP {
				t.Errorf("IP: got %s, want %s", decoded.IP, tt.info.IP)
			}
			if decoded.Port != tt.info.Port {
				t.Errorf("Port: got %d, want %d", decoded.Port, tt.info.Port)
			}
			if decoded.Version != tt.info.Version {
				t.Errorf("Version: got %s, want %s", decoded.Version, tt.info.Version)
			}
			if decoded.Status != tt.info.Status {
				t.Errorf("Status: got %s, want %s", decoded.Status, tt.info.Status)
			}
		})
	}
}

func TestDeserializeDiscoveryInfo_InvalidJSON(t *testing.T) {
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
			name:    "truncated json",
			input:   `{"id": "test"`,
			wantErr: true,
		},
		{
			name:    "valid json object",
			input:   `{"id": "test", "hostname": "host"}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DeserializeDiscoveryInfo(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestToWorkerInfo(t *testing.T) {
	disc := DiscoveryInfo{
		ID:       "worker-1",
		Hostname: "gpu-server",
		IP:       "192.168.1.100",
		Port:     8081,
		Version:  "v1.0",
		Status:   StatusAvailable,
	}

	wi := disc.ToWorkerInfo()

	// Verify scalar mapping
	if wi.ID != disc.ID {
		t.Errorf("ID: got %s, want %s", wi.ID, disc.ID)
	}
	if wi.Hostname != disc.Hostname {
		t.Errorf("Hostname: got %s, want %s", wi.Hostname, disc.Hostname)
	}
	if wi.IP != disc.IP {
		t.Errorf("IP: got %s, want %s", wi.IP, disc.IP)
	}
	if wi.Port != disc.Port {
		t.Errorf("Port: got %d, want %d", wi.Port, disc.Port)
	}
	if wi.Version != disc.Version {
		t.Errorf("Version: got %s, want %s", wi.Version, disc.Version)
	}
	if wi.Status != disc.Status {
		t.Errorf("Status: got %s, want %s", wi.Status, disc.Status)
	}

	// Verify empty Capabilities
	if len(wi.Capabilities.Models) != 0 {
		t.Errorf("expected empty Models, got %d", len(wi.Capabilities.Models))
	}
	if len(wi.Capabilities.Engines) != 0 {
		t.Errorf("expected empty Engines, got %d", len(wi.Capabilities.Engines))
	}
}
