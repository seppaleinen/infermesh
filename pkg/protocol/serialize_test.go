package protocol

import (
	"strings"
	"testing"
)

func TestSerializeDeserializeWorkerInfo_RoundTrip(t *testing.T) {
	info := WorkerInfo{
		ID:       "w-1",
		Hostname: "host-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   StatusAvailable,
		Version:  "v1",
	}
	raw, err := SerializeWorkerInfo(info)
	if err != nil {
		t.Fatalf("SerializeWorkerInfo: %v", err)
	}
	if raw == "" {
		t.Fatal("expected non-empty serialized worker info")
	}
	got, err := DeserializeWorkerInfo(raw)
	if err != nil {
		t.Fatalf("DeserializeWorkerInfo: %v", err)
	}
	if got.ID != info.ID || got.Hostname != info.Hostname || got.IP != info.IP ||
		got.Port != info.Port || got.Status != info.Status || got.Version != info.Version {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, info)
	}
}

func TestSerializeWorkerInfo_Empty(t *testing.T) {
	raw, err := SerializeWorkerInfo(WorkerInfo{})
	if err != nil {
		t.Fatalf("SerializeWorkerInfo empty: %v", err)
	}
	got, err := DeserializeWorkerInfo(raw)
	if err != nil {
		t.Fatalf("DeserializeWorkerInfo empty: %v", err)
	}
	if got.ID != "" {
		t.Fatalf("expected empty ID, got %q", got.ID)
	}
}

func TestDeserializeWorkerInfo_InvalidJSON(t *testing.T) {
	_, err := DeserializeWorkerInfo("not json {")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateTXTRecord_OK(t *testing.T) {
	if err := ValidateTXTRecord(strings.Repeat("a", 255)); err != nil {
		t.Fatalf("ValidateTXTRecord(255): %v", err)
	}
}

func TestValidateTXTRecord_TooLong(t *testing.T) {
	if err := ValidateTXTRecord(strings.Repeat("a", 256)); err == nil {
		t.Fatal("expected error for 256-byte record")
	}
}

func TestSerializeDiscoveryInfo_RoundTrip(t *testing.T) {
	disc := DiscoveryInfo{
		ID:       "w-1",
		Hostname: "host-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Version:  "v1",
		Status:   StatusAvailable,
	}
	raw, err := SerializeDiscoveryInfo(disc)
	if err != nil {
		t.Fatalf("SerializeDiscoveryInfo: %v", err)
	}
	if len(raw) > MaxTXTRecordLength {
		t.Fatalf("serialized discovery info %d bytes exceeds DNS limit", len(raw))
	}
	got, err := DeserializeDiscoveryInfo(raw)
	if err != nil {
		t.Fatalf("DeserializeDiscoveryInfo: %v", err)
	}
	if got.ID != disc.ID || got.Hostname != disc.Hostname || got.IP != disc.IP ||
		got.Port != disc.Port || got.Status != disc.Status {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, disc)
	}
}

func TestSerializeDiscoveryInfo_TooLarge(t *testing.T) {
	disc := DiscoveryInfo{ID: strings.Repeat("x", 400)}
	if _, err := SerializeDiscoveryInfo(disc); err == nil {
		t.Fatal("expected error for oversized discovery info")
	}
}

func TestDeserializeDiscoveryInfo_InvalidJSON_Serialize(t *testing.T) {
	_, err := DeserializeDiscoveryInfo("{bad")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDiscoveryInfo_ToWorkerInfo(t *testing.T) {
	disc := DiscoveryInfo{
		ID:       "w-1",
		Hostname: "host-1",
		IP:       "127.0.0.1",
		Port:     8081,
		Version:  "v1",
		Status:   StatusAvailable,
	}
	wi := disc.ToWorkerInfo()
	if wi.ID != disc.ID || wi.Hostname != disc.Hostname || wi.IP != disc.IP ||
		wi.Port != disc.Port || wi.Status != disc.Status || wi.Version != disc.Version {
		t.Fatalf("ToWorkerInfo mismatch: %+v", wi)
	}
}

func TestDiscoveryInfo_ToWorkerInfo_Empty(t *testing.T) {
	wi := (DiscoveryInfo{}).ToWorkerInfo()
	if wi.ID != "" || wi.Hostname != "" {
		t.Fatalf("expected empty worker info, got %+v", wi)
	}
}
