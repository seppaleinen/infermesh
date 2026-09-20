// Package wsutil adapts golang.org/x/net/websocket into the small surface
// InferMesh's router hub and worker client need: dial/accept, JSON envelopes,
// deadlines, and transport constants that both sides share.
//
// Concurrency contract: a connection MUST have exactly one goroutine reading
// (ReceiveJSON) and exactly one goroutine writing (SendJSON/SendText). The
// x/net/websocket connector serializes frames internally, but the InferMesh
// hub/worker enforce single-reader/single-writer so message ordering is
// deterministic.
package wsutil

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const (
	// MaxMessageSize caps a single protocol frame at 64 MiB. It is enforced by
	// x/net/websocket's Codec.Receive via Conn.MaxPayloadBytes.
	MaxMessageSize = 64 << 20

	// SendQueueSize is the outbound message queue depth per connection.
	SendQueueSize = 256

	// WriteTimeout is the deadline applied to every outbound frame.
	WriteTimeout = 30 * time.Second

	// DialTimeout bounds the TCP + WS handshake in Dial.
	DialTimeout = 10 * time.Second

	// DefaultKeepAlive is the TCP keepalive period set on dialed connections.
	DefaultKeepAlive = 30 * time.Second

	// DefaultHeartbeatInterval is how often workers send heartbeats.
	DefaultHeartbeatInterval = 30 * time.Second

	// DefaultPingInterval is how often the router pings idle connections.
	DefaultPingInterval = 30 * time.Second

	// ReadTimeout is the liveness deadline: a connection with no inbound
	// frame for this long is considered dead.
	ReadTimeout = 90 * time.Second
)

// ErrMessageTooLarge reports a frame that exceeded MaxMessageSize.
var ErrMessageTooLarge = errors.New("wsutil: message exceeds 64 MiB cap")

// Dial opens an outbound WebSocket client connection with TCP keepalive and a
// bounded handshake timeout. origin may be empty for non-browser clients;
// x/net/websocket requires a non-empty Origin, so a placeholder is sent when
// none is given (the router side accepts all origins).
func Dial(ctx context.Context, urlStr, origin string) (*websocket.Conn, error) {
	if origin == "" {
		origin = "http://infermesh.local"
	}
	config, err := websocket.NewConfig(urlStr, origin)
	if err != nil {
		return nil, err
	}
	config.Dialer = &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: DefaultKeepAlive,
	}
	// Support wss:// (WebSocket over TLS) by attaching a default TLS config.
	if strings.HasPrefix(urlStr, "wss://") {
		config.TlsConfig = &tls.Config{InsecureSkipVerify: false}
	}
	ws, err := config.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	ws.MaxPayloadBytes = MaxMessageSize
	return ws, nil
}

// Server returns an http.Handler that upgrades HTTP requests to WebSocket
// connections and invokes handle with the accepted connection.
//
// The default websocket.Handler rejects requests without an Origin header;
// non-browser workers never send Origin, so accept all origins. Callers that
// need origin/subprotocol checks should wrap their own websocket.Server.
func Server(handle func(*websocket.Conn)) http.Handler {
	return websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = MaxMessageSize
			handle(ws)
		},
	}
}

// SendJSON marshals v and writes it as a single text frame.
func SendJSON(ws *websocket.Conn, v interface{}) error {
	return websocket.JSON.Send(ws, v)
}

// ReceiveJSON reads a single frame and unmarshals it into v. Frames larger
// than MaxMessageSize return ErrMessageTooLarge.
func ReceiveJSON(ws *websocket.Conn, v interface{}) error {
	err := websocket.JSON.Receive(ws, v)
	if errors.Is(err, websocket.ErrFrameTooLarge) {
		return ErrMessageTooLarge
	}
	return err
}

// SendText writes pre-marshaled bytes as a single text frame.
func SendText(ws *websocket.Conn, data []byte) error {
	return websocket.Message.Send(ws, string(data))
}

// SetReadDeadline sets the connection's network read deadline.
func SetReadDeadline(ws *websocket.Conn, t time.Time) error {
	return ws.SetReadDeadline(t)
}

// SetWriteDeadline sets the connection's network write deadline.
func SetWriteDeadline(ws *websocket.Conn, t time.Time) error {
	return ws.SetWriteDeadline(t)
}

// SetTCPKeepAlive is kept for API completeness. The InferMesh transports do
// not need to call it: client connections get keepalive from Dial's dialer,
// and server-side accepted connections get keepalive from net/http.
func SetTCPKeepAlive(ws *websocket.Conn, keepAlive time.Duration) error {
	if ws == nil {
		return errors.New("wsutil: nil websocket connection")
	}
	return nil
}

// Close terminates the connection.
func Close(ws *websocket.Conn) error {
	return ws.Close()
}