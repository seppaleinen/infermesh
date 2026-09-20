package wsutil

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestPingPongRoundTrip dials the echo server, sends a JSON object and
// receives the same object back, proving Dial + SendJSON + ReceiveJSON work.
func TestPingPongRoundTrip(t *testing.T) {
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		var msg map[string]interface{}
		if err := ReceiveJSON(ws, &msg); err != nil {
			return
		}
		_ = SendJSON(ws, msg)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ws, err := Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	payload := map[string]interface{}{"hello": "world", "n": 42}
	if err := SendJSON(ws, payload); err != nil {
		t.Fatalf("SendJSON: %v", err)
	}

	var got map[string]interface{}
	if err := ReceiveJSON(ws, &got); err != nil {
		t.Fatalf("ReceiveJSON: %v", err)
	}
	if got["hello"] != "world" || got["n"] != float64(42) {
		t.Fatalf("echo mismatch: %v", got)
	}
}

// TestReceiveEnforcesCap verifies that a frame above MaxMessageSize is
// rejected with ErrMessageTooLarge by the receiving side.
func TestReceiveEnforcesCap(t *testing.T) {
	serverSide := make(chan error, 1)
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		var big []byte
		serverSide <- ReceiveJSON(ws, &big)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ws, err := Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// Send a JSON frame larger than the 64 MiB cap. The server side bails on
	// the oversized frame and closes, so the client write may or may not
	// complete; that is fine — the assertion is server-side rejection.
	big := strings.Repeat("a", MaxMessageSize+1)
	_ = SetReadDeadline(ws, time.Now().Add(5*time.Second))
	_ = SendText(ws, []byte(`{"data":"`+big+`"}`))

	select {
	case err := <-serverSide:
		if !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("expected ErrMessageTooLarge, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server-side receive")
	}
}

func TestSendTextRoundTrip(t *testing.T) {
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		var raw json.RawMessage
		if err := ReceiveJSON(ws, &raw); err != nil {
			return
		}
		_ = SendJSON(ws, raw)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ws, err := Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if err := SendText(ws, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	var got map[string]int
	if err := ReceiveJSON(ws, &got); err != nil {
		t.Fatalf("ReceiveJSON: %v", err)
	}
	if got["a"] != 1 {
		t.Fatalf("payload mismatch: %v", got)
	}
}

func TestReadDeadlineTriggers(t *testing.T) {
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		// Server never responds; client read should time out.
		var v interface{}
		_ = ReceiveJSON(ws, &v)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ws, err := Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if err := SetWriteDeadline(ws, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if err := SendText(ws, []byte(`{"ping":true}`)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	if err := SetReadDeadline(ws, time.Now().Add(200*time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	var got interface{}
	err = ReceiveJSON(ws, &got)
	if err == nil {
		t.Fatal("expected read timeout error, got nil")
	}
}

func TestServerRejectsMalformedUpgrade(t *testing.T) {
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {}))
	t.Cleanup(srv.Close)

	// A plain HTTP GET (no WebSocket upgrade) must not hang or panic; it
	// should just fail the handshake and return a non-101 response.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("unexpected upgrade status: %d", resp.StatusCode)
	}
}

func TestHelpers(t *testing.T) {
	if errors.Is(ErrMessageTooLarge, websocket.ErrFrameTooLarge) {
		t.Fatal("ErrMessageTooLarge should not alias ErrFrameTooLarge")
	}
	if err := SetTCPKeepAlive(nil, time.Second); err == nil {
		t.Fatal("expected error for nil connection")
	}
	if err := SetTCPKeepAlive(&websocket.Conn{}, 30*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}