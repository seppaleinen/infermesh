package router

import (
	"testing"
)

// TestNormalizeRelayURL_StripsExistingSuffix verifies that a user-supplied
// /v1/connect is stripped before the canonical path is appended, so both URL
// forms normalize to the same canonical endpoint (issue #191).
func TestNormalizeRelayURL_StripsExistingSuffix(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"suffix stripped", "ws://192.168.1.216:8090/v1/connect", "ws://192.168.1.216:8090/v1/connect"},
		{"suffix stripped http scheme", "http://192.168.1.216:8090/v1/connect", "ws://192.168.1.216:8090/v1/connect"},
		{"trailing slash stripped", "ws://192.168.1.216:8090/v1/connect/", "ws://192.168.1.216:8090/v1/connect"},
		{"suffix stripped with query", "ws://192.168.1.216:8090/v1/connect?x=1", "ws://192.168.1.216:8090/v1/connect"},
		{"bare host", "ws://192.168.1.216:8090", "ws://192.168.1.216:8090/v1/connect"},
		{"root path", "ws://192.168.1.216:8090/", "ws://192.168.1.216:8090/v1/connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRelayURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeRelayURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRelayURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNormalizeRelayURL_BareHost verifies the documented bare form: the
// canonical /v1/connect path is appended and scheme conversion works.
func TestNormalizeRelayURL_BareHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"ws scheme", "ws://relay:8090", "ws://relay:8090/v1/connect"},
		{"wss scheme", "wss://relay:8090", "wss://relay:8090/v1/connect"},
		{"http scheme converts", "http://relay:8090", "ws://relay:8090/v1/connect"},
		{"https scheme converts", "https://relay:8090", "wss://relay:8090/v1/connect"},
		{"no scheme", "relay:8090", "ws://relay:8090/v1/connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRelayURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeRelayURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRelayURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}