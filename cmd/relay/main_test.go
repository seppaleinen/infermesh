package main

// Tests for the NAT reachability fix in cmd/relay. A worker behind NAT advertises
// its private IP (pkg/worker/host.go LocalIP); the relay rewrites it with the
// real client source address so the fleet router can route back to the worker.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// ipFromRemote unit tests — the pure parsing core of the rewrite.
func TestIpFromRemote(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"ipv4 with port", "172.19.121.23:54321", "172.19.121.23"},
		{"bare ipv4", "127.0.0.1", "127.0.0.1"},
		{"v4-in-v6 mapped with port", "[::ffff:172.19.121.23]:54321", "172.19.121.23"},
		{"bare v4-in-v6 mapped", "::ffff:10.0.0.5", "10.0.0.5"},
		{"ipv6 with port", "[2001:db8::1]:54321", "2001:db8::1"},
		{"bare ipv6", "2001:db8::1", "2001:db8::1"},
		{"empty returns empty", "", ""},
		{"garbage returns empty", "not-an-ip", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ipFromRemote(c.input)
			if got != c.want {
				t.Fatalf("ipFromRemote(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// newTestRelayServer spins up an in-process relay using the same /v1/connect
// handler as startWSServer so handleWorker runs for real. It returns both the
// server and the backing *Relay (the relay is needed to poll router state).
func newTestRelayServer(t *testing.T) (*httptest.Server, *Relay) {
	t.Helper()
	r := &Relay{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		listenAddr:  "127.0.0.1:0",
		workers:     make(map[string]*websocket.Conn),
		workerQueue: make([]protocol.Message, 0),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/connect", func(w http.ResponseWriter, req *http.Request) {
		wsutil.Server(r.handleWS).ServeHTTP(w, req)
	})
	return httptest.NewServer(mux), r
}

// wsURL converts an httptest server URL to a ws:// dial target.
func wsURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "http://", "ws://", 1)
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, err := wsutil.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// waitForRouterConnected polls the relay until a router connection is set.
func waitForRouterConnected(t *testing.T, r *Relay) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.routerMu.Lock()
		ok := r.routerConn != nil
		r.routerMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("router never connected to relay")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustMessage builds a protocol envelope for test payloads.
func mustMessage(t *testing.T, mt protocol.MessageType, payload ...interface{}) protocol.Message {
	t.Helper()
	if len(payload) == 0 {
		m, err := protocol.NewMessage(mt, nil)
		if err != nil {
			t.Fatalf("NewMessage(%s): %v", mt, err)
		}
		return m
	}
	m, err := protocol.NewMessage(mt, payload[0])
	if err != nil {
		t.Fatalf("NewMessage(%s): %v", mt, err)
	}
	return m
}

// TestRelayRewritesPrivateIPOnRegister drives handleWorker end-to-end: a router
// connects, then a worker registers advertising its private IP. The forwarded
// register must carry the real client source address (loopback in-test) and
// preserve every other Worker field. This proves the reachability rewrite that
// fixes fleet routing to workers behind NAT.
func TestRelayRewritesPrivateIPOnRegister(t *testing.T) {
	srv, relay := newTestRelayServer(t)

	// Router connects first so the worker's register forwards immediately.
	routerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(routerWS, mustMessage(t, protocol.MsgRouterReady)); err != nil {
		t.Fatalf("failed to send router_ready: %v", err)
	}
	waitForRouterConnected(t, relay)

	// Worker advertises a private RFC1918 IP (the NAT bug scenario).
	regPayload := protocol.RegisterPayload{
		Worker: protocol.WorkerInfo{
			ID:        "worker-1",
			Hostname:  "nat-client",
			IP:        "172.19.121.23", // private IP behind NAT
			Port:      8081,
			Status:    protocol.StatusAvailable,
			Version:   "relay-test-version",
			Transport: "ws",
			Capabilities: protocol.Capabilities{
				Engines: []string{"llama-cpp"},
			},
		},
		Relay: true,
	}
	msg := mustMessage(t, protocol.MsgRegister, regPayload)

	// Worker registers; handleWorker rewrites the IP and forwards to the router.
	workerWS := dialWS(t, wsURL(srv)+"/v1/connect")
	if err := wsutil.SendJSON(workerWS, msg); err != nil {
		t.Fatalf("failed to send register: %v", err)
	}

	// Router receives the forwarded register envelope.
	_ = routerWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	var fwd protocol.Message
	if err := websocket.JSON.Receive(routerWS, &fwd); err != nil {
		t.Fatalf("router did not receive forwarded register: %v", err)
	}

	if fwd.Type != protocol.MsgRegister {
		t.Fatalf("forwarded message type = %q, want register", fwd.Type)
	}
	if fwd.WorkerID != "worker-1" {
		t.Fatalf("forwarded worker_id = %q, want worker-1", fwd.WorkerID)
	}

	var decoded protocol.RegisterPayload
	if err := fwd.DecodePayload(&decoded); err != nil {
		t.Fatalf("failed to decode forwarded register: %v", err)
	}

	gotIP := decoded.Worker.IP
	wantSource := "127.0.0.1" // loopback client source address (relay's real dialer)
	if gotIP != wantSource {
		t.Fatalf("forwarded worker IP = %q, want routable source %q", gotIP, wantSource)
	}

	// Other fields must be preserved unchanged by the rewrite.
	want := decoded.Worker
	if want.ID != "worker-1" {
		t.Errorf("ID not preserved: %q", want.ID)
	}
	if want.Hostname != "nat-client" {
		t.Errorf("Hostname not preserved: %q", want.Hostname)
	}
	if want.Port != 8081 {
		t.Errorf("Port not preserved: %d", want.Port)
	}
	if want.Version != "relay-test-version" {
		t.Errorf("Version not preserved: %q", want.Version)
	}
	if want.Transport != "ws" {
		t.Errorf("Transport not preserved: %q", want.Transport)
	}
	if want.Status != protocol.StatusAvailable {
		t.Errorf("Status not preserved: %q", want.Status)
	}
	if len(want.Capabilities.Engines) != 1 || want.Capabilities.Engines[0] != "llama-cpp" {
		t.Errorf("Capabilities.Engines not preserved: %+v", want.Capabilities.Engines)
	}
	if !decoded.Relay {
		t.Errorf("Relay flag not preserved: %v", decoded.Relay)
	}
}

// TestRoutableIPSentinel verifies the no-source fallback returns the empty
// sentinel so callers keep the worker's advertised IP unchanged.
func TestRoutableIPSentinel(t *testing.T) {
	if routableIP(nil) != "" {
		t.Fatal("routableIP(nil) should be empty sentinel")
	}
	if ipFromRemote("") != "" {
		t.Fatal("ipFromRemote(\"\") should be empty sentinel")
	}
}
