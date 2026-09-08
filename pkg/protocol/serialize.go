package protocol

import (
	"encoding/json"
	"fmt"
)

// SerializeWorkerInfo converts WorkerInfo to a compact JSON string for mDNS TXT records.
func SerializeWorkerInfo(info WorkerInfo) (string, error) {
	data, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DeserializeWorkerInfo parses a WorkerInfo from an mDNS TXT record.
func DeserializeWorkerInfo(raw string) (WorkerInfo, error) {
	var info WorkerInfo
	err := json.Unmarshal([]byte(raw), &info)
	return info, err
}

// MaxTXTRecordLength is the DNS limit for a single TXT record string.
const MaxTXTRecordLength = 255

// ValidateTXTRecord returns an error if s exceeds the DNS TXT string limit.
func ValidateTXTRecord(s string) error {
	if len(s) > MaxTXTRecordLength {
		return fmt.Errorf("txt record %d bytes exceeds DNS limit of %d", len(s), MaxTXTRecordLength)
	}
	return nil
}

// SerializeDiscoveryInfo converts DiscoveryInfo to a compact JSON string
// for mDNS TXT records, verifying it stays under the DNS 255-byte limit.
func SerializeDiscoveryInfo(info DiscoveryInfo) (string, error) {
	data, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	s := string(data)
	if err := ValidateTXTRecord(s); err != nil {
		return "", err
	}
	return s, nil
}

// DeserializeDiscoveryInfo parses a DiscoveryInfo from an mDNS TXT record.
func DeserializeDiscoveryInfo(raw string) (DiscoveryInfo, error) {
	var info DiscoveryInfo
	err := json.Unmarshal([]byte(raw), &info)
	return info, err
}

// ToWorkerInfo converts a DiscoveryInfo into a minimal WorkerInfo with
// empty Capabilities. The router hydrates Capabilities via HTTP later.
func (d DiscoveryInfo) ToWorkerInfo() WorkerInfo {
	return WorkerInfo{
		ID:       d.ID,
		Hostname: d.Hostname,
		IP:       d.IP,
		Port:     d.Port,
		Version:  d.Version,
		Status:   d.Status,
	}
}