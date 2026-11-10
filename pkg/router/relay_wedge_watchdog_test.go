package router

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// TestRelayReaderWatchdogBreaksSilentRelay verifies the watchdog goroutine in
// relayReaderLoop force-breaks a stuck ReceiveJSON when the relay goes silent
// for watchdogRelayReadTimeout — even when the relay's ws.Close() never
// delivers its close frame (the lost-close-frame wedge, issue #199).
//
// The relay (an http.Server) sends a few pings so the router's watchdog
// resets, then holds the socket open without sending any more frames and
// WITHOUT calling ws.Close() — simulating the case where the close frame is
// discarded in the relay's internal bufio.Writer and the CNI drops the TCP
// FIN. The router's reader loop must detect the silence on its own.
func TestRelayReaderWatchdogBreaksSilentRelay(t *testing.T) {
	orig := watchdogRelayReadTimeout
	watchdogRelayReadTimeout = 500 * time.Millisecond
	defer func() { watchdogRelayReadTimeout = orig }()

	// release lets the relay handler hold the socket open (silent, no
	// ws.Close) until the test is done.
	release := make(chan struct{})
	defer close(release)

	srv := &http.Server{Addr: "127.0.0.1:0", Handler: websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = wsutil.MaxMessageSize
			// Read router_ready (first frame from the router).
			var ready protocol.Message
			if err := wsutil.ReceiveJSON(ws, &ready); err != nil {
				return
			}
			// A few pings so the router's watchdog resets.
			for i := 0; i < 3; i++ {
				_ = wsutil.WriteJSON(ws, protocol.Message{Type: protocol.MsgPing})
			}
			// Then go SILENT: hold the socket open, send nothing, and do
			// NOT call ws.Close(). This is the lost-close-frame wedge:
			// the relay's routerReadLoop exited, but its close frame was
			// discarded and the FIN never reached the router.
			<-release
		},
	}}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	defer func() { _ = ln.Close() }()

	// ROUTER side: dial, send router_ready, run the real relayReaderLoop.
	conn, err := wsutil.Dial(context.Background(), "ws://"+ln.Addr().String()+"/", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := wsutil.WriteJSON(conn, protocol.Message{Type: protocol.MsgRouterReady}); err != nil {
		t.Fatalf("send router_ready: %v", err)
	}

	hub := &WSHub{
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		conns:        make(map[string]*wsConnection),
		relayPending: make(map[string]*pendingCall),
		relaySendCh:  make(chan []byte, wsutil.SendQueueSize),
	}
	hub.relayMu.Lock()
	hub.relayConn = conn
	hub.relayMu.Unlock()

	done := make(chan struct{})
	start := time.Now()

	go hub.relayReaderLoop(context.Background(), done)

	select {
	case <-done:
		elapsed := time.Since(start)
		// The watchdog fires after watchdogRelayReadTimeout; the reader
		// must exit shortly after. Anything much longer means the
		// watchdog did not break the stuck read.
		if elapsed < watchdogRelayReadTimeout-100*time.Millisecond {
			t.Fatalf("reader exited in %v — earlier than the watchdog could have fired", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("watchdog took %v — expected ~%v", elapsed, watchdogRelayReadTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relayReaderLoop did NOT exit after the relay went silent — watchdog failed to break the stuck read")
	}
}
