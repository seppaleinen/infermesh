package router

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
	"golang.org/x/net/websocket"
)

// fakeWorker drives a client-side WebSocket connection acting as a worker.
type fakeWorker struct {
	t    *testing.T
	conn *websocket.Conn
}

func dialFakeWorker(t *testing.T, url string) *fakeWorker {
	t.Helper()
	// httptest servers expose http:// URLs; the websocket config needs ws://.
	wsURL := "ws://" + strings.TrimPrefix(url, "http://")
	conn, err := wsutil.Dial(context.Background(), wsURL, "")
	if err != nil {
		t.Fatalf("failed to dial hub: %v", err)
	}
	t.Cleanup(func() { _ = wsutil.Close(conn) })
	return &fakeWorker{t: t, conn: conn}
}

func (w *fakeWorker) send(t *testing.T, msg protocol.Message) {
	t.Helper()
	if err := wsutil.SendJSON(w.conn, msg); err != nil {
		t.Fatalf("fake worker send failed: %v", err)
	}
}

func (w *fakeWorker) recv(t *testing.T, timeout time.Duration) protocol.Message {
	t.Helper()
	if err := wsutil.SetReadDeadline(w.conn, time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var msg protocol.Message
	if err := wsutil.ReceiveJSON(w.conn, &msg); err != nil {
		t.Fatalf("fake worker receive failed: %v", err)
	}
	return msg
}

func rawMsg(t *testing.T, mt protocol.MessageType, payload interface{}) protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(mt, payload)
	if err != nil {
		t.Fatalf("build %s message: %v", mt, err)
	}
	return msg
}

// replyTo builds a response whose ID echoes the correlated request.
func replyTo(t *testing.T, req protocol.Message, mt protocol.MessageType, payload interface{}) protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(mt, payload)
	if err != nil {
		t.Fatalf("build %s message: %v", mt, err)
	}
	msg.ID = req.ID
	return msg
}

func testWSHub(t *testing.T) (*WSHub, *httptest.Server) {
	t.Helper()
	reg, err := registry.New(registry.Defaults(), wsHubTestLogger())
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	hub := NewWSHub(reg, wsHubTestLogger())
	hub.Start(context.Background())
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = reg.Stop()
	})
	return hub, server
}

// wsHubTestLogger returns a default test logger.
func wsHubTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}