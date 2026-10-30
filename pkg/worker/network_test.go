package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"golang.org/x/net/websocket"
)

// TestNormalizeConnectURL_StripsExistingSuffix verifies that a user-supplied
// /v1/connect is stripped before the canonical path is appended, so both URL
// forms normalize to the same canonical endpoint (issue #191).
func TestNormalizeConnectURL_StripsExistingSuffix(t *testing.T) {
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
			got, err := normalizeConnectURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeConnectURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeConnectURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNormalizeConnectURL_BareHost verifies the documented bare form: the
// canonical /v1/connect path is appended and scheme conversion works.
func TestNormalizeConnectURL_BareHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"ws scheme", "ws://router:8080", "ws://router:8080/v1/connect"},
		{"wss scheme", "wss://router:8080", "wss://router:8080/v1/connect"},
		{"http scheme converts", "http://router:8080", "ws://router:8080/v1/connect"},
		{"https scheme converts", "https://router:8080", "wss://router:8080/v1/connect"},
		{"no scheme", "router:8080", "ws://router:8080/v1/connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeConnectURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizeConnectURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeConnectURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestRelayReachable_NormalizesFirst proves the reachability probe dials the
// canonical /v1/connect endpoint when given a bare relay URL. Without the
// normalization fix, wsutil.Dial on a pathless URL fails the handshake and
// the worker silently falls back to HTTP registration (issue #191).
func TestRelayReachable_NormalizesFirst(t *testing.T) {
	var seenPath string
	var upgraded int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		// Accept the WebSocket upgrade and immediately close the socket so
		// relayReachable's fire-and-forget dial completes its handshake.
		wsutil.Server(func(ws *websocket.Conn) {
			atomic.StoreInt32(&upgraded, 1)
			_ = ws.Close()
		}).ServeHTTP(w, r)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")

	// Bare URL: normalization must append /v1/connect before dialing.
	if !relayReachable(context.Background(), "ws://" + host) {
		t.Fatal("relayReachable returned false for a live relay with bare URL; " +
			"expected normalization to append /v1/connect before dialing (issue #191)")
	}
	if seenPath != "/v1/connect" {
		t.Fatalf("dial hit path %q, want /v1/connect", seenPath)
	}
	if atomic.LoadInt32(&upgraded) != 1 {
		t.Fatal("server did not observe a WebSocket upgrade")
	}

	// Both URL forms must probe the same canonical endpoint.
	if !relayReachable(context.Background(), "ws://" + host + "/v1/connect") {
		t.Fatal("relayReachable returned false for the explicit-suffix URL")
	}

	// A closed port must still report unreachable (no false positive).
	if relayReachable(context.Background(), "ws://127.0.0.1:1") {
		t.Fatal("relayReachable returned true for a closed port")
	}
}