package wsutil

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestWriteJSON_NilConn returns an error for a nil connection.
func TestWriteJSON_NilConn(t *testing.T) {
	if err := WriteJSON(nil, map[string]string{"x": "y"}); err == nil {
		t.Fatal("WriteJSON(nil) = nil, want error")
	}
}

// TestWriteText_NilConn returns an error for a nil connection.
func TestWriteText_NilConn(t *testing.T) {
	if err := WriteText(nil, []byte("x")); err == nil {
		t.Fatal("WriteText(nil) = nil, want error")
	}
}

// TestWriteJSON_DeadlineExceeded verifies that WriteJSON returns ErrWriteTimeout
// when the peer stops reading and the write deadline fires. The payload is large
// enough to exceed the kernel send buffer so the write genuinely blocks rather
// than completing into the buffer.
func TestWriteJSON_DeadlineExceeded(t *testing.T) {
	orig := WriteTimeout
	SetWriteTimeout(100 * time.Millisecond)
	t.Cleanup(func() { SetWriteTimeout(orig) })

	keepOpen := make(chan struct{})
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		// Accept the upgrade but never read frames — simulate a peer that
		// stopped reading. Block until the test signals shutdown.
		<-keepOpen
	}))
	defer srv.Close()

	ws, err := Dial(context.Background(), wsURL(srv), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	// Payload large enough to exceed the kernel send buffer (~64 KiB on macOS)
	// so the write blocks until the deadline fires. 4 MiB was verified
	// empirically to block reliably; 2 MiB completes into the buffer.
	big := strings.Repeat("x", 4<<20) // 4 MiB
	err = WriteJSON(ws, map[string]string{"payload": big})
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("WriteJSON err = %v, want ErrWriteTimeout", err)
	}
	close(keepOpen)
}

// TestWriteText_DeadlineExceeded verifies the same deadline behavior for
// pre-marshaled text frames.
func TestWriteText_DeadlineExceeded(t *testing.T) {
	orig := WriteTimeout
	SetWriteTimeout(100 * time.Millisecond)
	t.Cleanup(func() { SetWriteTimeout(orig) })

	keepOpen := make(chan struct{})
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		<-keepOpen
	}))
	defer srv.Close()

	ws, err := Dial(context.Background(), wsURL(srv), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	data := []byte(strings.Repeat("y", 4<<20)) // 4 MiB
	err = WriteText(ws, data)
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("WriteText err = %v, want ErrWriteTimeout", err)
	}
	close(keepOpen)
}

// TestWriteJSON_Success verifies the happy path: a peer that reads returns nil.
func TestWriteJSON_Success(t *testing.T) {
	srv := httptest.NewServer(Server(func(ws *websocket.Conn) {
		var v interface{}
		if err := ReceiveJSON(ws, &v); err != nil {
			t.Errorf("server receive: %v", err)
		}
	}))
	defer srv.Close()

	ws, err := Dial(context.Background(), wsURL(srv), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if err := WriteJSON(ws, map[string]string{"ok": "1"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
}

// TestIsWriteTimeout verifies the sentinel detection logic directly.
func TestIsWriteTimeout(t *testing.T) {
	if !isWriteTimeout(ErrWriteTimeout) {
		t.Fatal("isWriteTimeout(ErrWriteTimeout) = false, want true")
	}
	if isWriteTimeout(nil) {
		t.Fatal("isWriteTimeout(nil) = true, want false")
	}
	if isWriteTimeout(io.EOF) {
		t.Fatal("isWriteTimeout(io.EOF) = true, want false")
	}
}

// wsURL converts an httptest server URL to a ws:// dial target.
func wsURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "http://", "ws://", 1)
}
