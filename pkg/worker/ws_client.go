package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
	"golang.org/x/net/websocket"
)

const (
	// wsRegisterBackoffBase is the initial reconnect delay after a lost
	// WebSocket connection (then 2s, 4s, ... capped at the configured max).
	wsRegisterBackoffBase = time.Second

	// streamingGracePeriod is how long the worker waits for a backend's
	// StreamChat/StreamCompletions to produce its first chunk before falling
	// back to the non-streaming completion path. Most backend adapters do not
	// implement real streaming and close immediately; this preserves the
	// existing HTTP handler behavior of "streaming requested but falling back
	// to non-streaming" without blocking on channels that never close.
	streamingGracePeriod = 5 * time.Second
)

// WSConfig configures the outbound worker→router WebSocket registration loop.
type WSConfig struct {
	// RouterURL is the router base URL, e.g. "ws://127.0.0.1:8080".
	// http:// and https:// schemes are accepted and converted to ws/wss.
	RouterURL string

	// HeartbeatInterval is how often the worker re-sends its WorkerInfo
	// (defaults to wsutil.DefaultHeartbeatInterval).
	HeartbeatInterval time.Duration

	// MaxReconnectDelay caps the exponential reconnect backoff (default 30s).
	MaxReconnectDelay time.Duration

	// WriteTimeout is the deadline for each outbound frame (default
	// wsutil.WriteTimeout).
	WriteTimeout time.Duration
}

func (c WSConfig) withDefaults() WSConfig {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = wsutil.DefaultHeartbeatInterval
	}
	if c.MaxReconnectDelay <= 0 {
		c.MaxReconnectDelay = 30 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = wsutil.WriteTimeout
	}
	return c
}

// WSRegisterLoop connects the worker to the router over WebSocket, registers,
// serves inference/model-load requests, and reconnects with exponential
// backoff until ctx is cancelled. infoFn is called on register and on every
// heartbeat so the router always sees fresh capabilities/models.
func WSRegisterLoop(ctx context.Context, cfg WSConfig, infoFn func() protocol.WorkerInfo, srv *Server, log *slog.Logger) {
	cfg = cfg.withDefaults()

	connectURL, err := normalizeConnectURL(cfg.RouterURL)
	if err != nil {
		log.Error("invalid router url; websocket registration disabled", "router", cfg.RouterURL, "error", err)
		return
	}

	delay := wsRegisterBackoffBase
	for {
		if ctx.Err() != nil {
			return
		}
		err := wsConnectOnce(ctx, connectURL, cfg, infoFn, srv, log)
		if ctx.Err() != nil {
			return
		}
		log.Warn("websocket connection to router lost; reconnecting",
			"router", connectURL, "error", err, "delay", delay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = nextBackoff(delay, cfg.MaxReconnectDelay)
	}
}

// normalizeConnectURL converts a router base URL (possibly http://-style, as
// accepted by --router) into a WebSocket URL targeting /v1/connect. A bare
// host/port gets the path appended; an explicit path is kept as-is so callers
// can point at custom endpoints.
func normalizeConnectURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "http://"):
		s = "ws://" + strings.TrimPrefix(s, "http://")
	case strings.HasPrefix(s, "https://"):
		s = "wss://" + strings.TrimPrefix(s, "https://")
	case !strings.HasPrefix(s, "ws://") && !strings.HasPrefix(s, "wss://"):
		s = "ws://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid router url %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/connect"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// nextBackoff doubles the delay up to max and applies ±20% jitter so
// reconnect storms never synchronize across a fleet.
func nextBackoff(prev, max time.Duration) time.Duration {
	if prev <= 0 {
		prev = wsRegisterBackoffBase
	}
	next := prev * 2
	if max > 0 && next > max {
		next = max
	}
	delta := next / 5
	return next - delta + time.Duration(rand.Float64()*float64(2*delta))
}

// wsConnectOnce dials, registers, and serves messages on a single connection.
// It returns when the connection dies or ctx is cancelled.
func wsConnectOnce(ctx context.Context, connectURL string, cfg WSConfig, infoFn func() protocol.WorkerInfo, srv *Server, log *slog.Logger) error {
	conn, err := wsutil.Dial(ctx, connectURL, "")
	if err != nil {
		return fmt.Errorf("dial router: %w", err)
	}
	defer wsutil.Close(conn)
	log.Info("websocket connected to router", "router", connectURL)

	out := &wsOutbound{conn: conn, timeout: cfg.WriteTimeout}

	registerMsg, err := protocol.NewMessage(protocol.MsgRegister, protocol.RegisterPayload{Worker: infoFn()})
	if err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}
	if err := out.send(registerMsg); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// Per-connection context: heartbeat and in-flight dispatcher goroutines
	// stop as soon as this connection tears down.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// Force the (blocking) reader loop to return on app shutdown.
	go func() {
		select {
		case <-ctx.Done():
			_ = wsutil.Close(conn)
		case <-connCtx.Done():
		}
	}()

	go wsHeartbeat(connCtx, out, infoFn, cfg.HeartbeatInterval, log)

	err = wsReaderLoop(connCtx, conn, out, srv, log)
	connCancel()
	return err
}

// wsOutbound serializes writes to a WebSocket connection. x/net/websocket's
// Conn is not safe for concurrent writes, and the worker writes from the
// heartbeat goroutine, the reader loop's ping echo, and per-request dispatch
// goroutines, so every outbound frame goes through this mutex.
type wsOutbound struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	timeout time.Duration
}

func (o *wsOutbound) send(m protocol.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timeout > 0 {
		if err := wsutil.SetWriteDeadline(o.conn, time.Now().Add(o.timeout)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}
	return wsutil.SendJSON(o.conn, m)
}

// wsHeartbeat re-sends the worker's info at a fixed interval. A failed send
// closes the connection so the reader loop errors and the outer loop
// reconnects instead of silently wedging.
func wsHeartbeat(ctx context.Context, out *wsOutbound, infoFn func() protocol.WorkerInfo, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msg, err := protocol.NewMessage(protocol.MsgHeartbeat, protocol.HeartbeatPayload{Worker: infoFn()})
			if err != nil {
				log.Warn("failed to build heartbeat", "error", err)
				continue
			}
			if err := out.send(msg); err != nil {
				log.Warn("heartbeat send failed; closing connection", "error", err)
				_ = wsutil.Close(out.conn)
				return
			}
		}
	}
}

// wsReaderLoop reads frames until the connection dies or the router asks us
// to close. Long-running work is dispatched to goroutines so the loop stays
// responsive to pings.
func wsReaderLoop(ctx context.Context, conn *websocket.Conn, out *wsOutbound, srv *Server, log *slog.Logger) error {
	for {
		_ = wsutil.SetReadDeadline(conn, time.Now().Add(wsutil.ReadTimeout))
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(conn, &msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("receive: %w", err)
		}
		shouldClose, err := handleWSMessage(ctx, msg, out, srv, log)
		if err != nil {
			return err
		}
		if shouldClose {
			return nil
		}
	}
}

// handleWSMessage processes a single inbound message. Long-running inference
// runs in a goroutine so pings are never starved.
func handleWSMessage(ctx context.Context, msg protocol.Message, out *wsOutbound, srv *Server, log *slog.Logger) (bool, error) {
	switch msg.Type {
	case protocol.MsgPing:
		reply := protocol.Message{ID: msg.ID, Type: protocol.MsgPing}
		if err := out.send(reply); err != nil {
			return false, fmt.Errorf("echo ping: %w", err)
		}
	case protocol.MsgInferenceRequest:
		go dispatchInferenceRequest(ctx, msg, out, srv)
	case protocol.MsgModelLoadRequest:
		go dispatchModelLoadRequest(ctx, msg, out, srv, log)
	case protocol.MsgClose:
		var p protocol.ClosePayload
		_ = msg.DecodePayload(&p)
		log.Info("router requested connection close", "reason", p.Reason)
		return true, nil
	case protocol.MsgError:
		var p protocol.ErrorPayload
		_ = msg.DecodePayload(&p)
		log.Warn("router reported error", "message", p.Message, "code", p.Code)
	default:
		log.Warn("unexpected message type from router", "type", msg.Type)
	}
	return false, nil
}

func dispatchInferenceRequest(ctx context.Context, msg protocol.Message, out *wsOutbound, srv *Server) {
	if srv == nil || srv.backend == nil {
		_ = sendError(out, msg.ID, errors.New("worker backend not configured"))
		return
	}
	var p protocol.InferenceRequestPayload
	if err := msg.DecodePayload(&p); err != nil {
		_ = sendError(out, msg.ID, fmt.Errorf("bad inference request: %w", err))
		return
	}
	switch p.Kind {
	case "chat":
		dispatchChat(ctx, msg.ID, p, srv.backend, out)
	case "completion":
		dispatchCompletion(ctx, msg.ID, p, srv.backend, out)
	default:
		_ = sendError(out, msg.ID, fmt.Errorf("unknown inference kind %q", p.Kind))
	}
}

func dispatchChat(ctx context.Context, id string, p protocol.InferenceRequestPayload, backend Backend, out *wsOutbound) {
	var req ChatRequest
	if err := json.Unmarshal(p.Body, &req); err != nil {
		_ = sendError(out, id, fmt.Errorf("invalid chat request body: %w", err))
		return
	}
	if req.Stream {
		streamOrFallback(ctx, id, "chat",
			func(c context.Context) (<-chan ChatChunk, <-chan error) {
				return backend.StreamChat(c, req.Model, req)
			},
			func(c context.Context) (interface{}, error) {
				resp, err := backend.CompleteChat(c, req.Model, req)
				return resp, err
			},
			out)
		return
	}
	respBody, err := backend.CompleteChat(ctx, req.Model, req)
	if err != nil {
		_ = sendBackendError(out, id, err)
		return
	}
	raw, err := json.Marshal(respBody)
	if err != nil {
		_ = sendError(out, id, fmt.Errorf("marshal response: %w", err))
		return
	}
	_ = sendInferenceDone(out, id, raw)
}

func dispatchCompletion(ctx context.Context, id string, p protocol.InferenceRequestPayload, backend Backend, out *wsOutbound) {
	var req CompletionRequest
	if err := json.Unmarshal(p.Body, &req); err != nil {
		_ = sendError(out, id, fmt.Errorf("invalid completion request body: %w", err))
		return
	}
	if req.Stream {
		streamOrFallback(ctx, id, "completion",
			func(c context.Context) (<-chan CompletionChunk, <-chan error) {
				return backend.StreamCompletions(c, req.Model, req)
			},
			func(c context.Context) (interface{}, error) {
				resp, err := backend.CompleteCompletions(c, req.Model, req)
				return resp, err
			},
			out)
		return
	}
	respBody, err := backend.CompleteCompletions(ctx, req.Model, req)
	if err != nil {
		_ = sendBackendError(out, id, err)
		return
	}
	raw, err := json.Marshal(respBody)
	if err != nil {
		_ = sendError(out, id, fmt.Errorf("marshal response: %w", err))
		return
	}
	_ = sendInferenceDone(out, id, raw)
}

// streamOrFallback relays a backend streaming response as inference_chunk
// messages terminated by an inference_response(Done=true). Backends without
// real streaming never deliver a first chunk inside the grace window, so the
// request falls back to the non-streaming completion path (the same behavior
// the HTTP handlers exhibit).
func streamOrFallback[T any](ctx context.Context, id, kind string,
	stream func(context.Context) (<-chan T, <-chan error),
	complete func(context.Context) (interface{}, error),
	out *wsOutbound) {

	chunkCh, errCh := stream(ctx)

	select {
	case chunk, ok := <-chunkCh:
		if !ok {
			_ = sendInferenceDone(out, id, nil)
			return
		}
		if err := sendChunk(out, id, kind, chunk); err != nil {
			return
		}
	case err, ok := <-errCh:
		if ok && err != nil {
			_ = sendBackendError(out, id, err)
			return
		}
		_ = sendInferenceDone(out, id, nil)
		return
	case <-time.After(streamingGracePeriod):
		respBody, err := complete(ctx)
		if err != nil {
			_ = sendBackendError(out, id, err)
			return
		}
		raw, err := json.Marshal(respBody)
		if err != nil {
			_ = sendError(out, id, fmt.Errorf("marshal response: %w", err))
			return
		}
		_ = sendInferenceDone(out, id, raw)
		return
	case <-ctx.Done():
		return
	}

	for {
		select {
		case chunk, ok := <-chunkCh:
			if !ok {
				_ = sendInferenceDone(out, id, nil)
				return
			}
			if err := sendChunk(out, id, kind, chunk); err != nil {
				return
			}
		case err, ok := <-errCh:
			if ok && err != nil {
				_ = sendBackendError(out, id, err)
				return
			}
			_ = sendInferenceDone(out, id, nil)
			return
		case <-ctx.Done():
			return
		}
	}
}

func dispatchModelLoadRequest(ctx context.Context, msg protocol.Message, out *wsOutbound, srv *Server, log *slog.Logger) {
	var p protocol.ModelLoadRequestPayload
	if err := msg.DecodePayload(&p); err != nil {
		_ = sendError(out, msg.ID, fmt.Errorf("bad model load request: %w", err))
		return
	}

	payload := protocol.ModelLoadResponsePayload{Model: p.Model}
	if srv == nil {
		payload.Error = "worker server not configured"
	} else {
		payload.Loaded, payload.Error = srv.loadModelByName(p.Model)
	}

	resp, err := protocol.NewMessage(protocol.MsgModelLoadResponse, payload)
	if err != nil {
		_ = sendError(out, msg.ID, fmt.Errorf("marshal model load response: %w", err))
		return
	}
	resp.ID = msg.ID
	if err := out.send(resp); err != nil {
		log.Warn("failed to send model load response", "error", err)
	}
}

// sendChunk marshals a streamed chunk into an inference_chunk message.
func sendChunk[T any](out *wsOutbound, id, kind string, chunk T) error {
	raw, err := json.Marshal(chunk)
	if err != nil {
		_ = sendError(out, id, fmt.Errorf("marshal chunk: %w", err))
		return err
	}
	msg := protocol.Message{ID: id, Type: protocol.MsgInferenceChunk}
	payload, err := json.Marshal(protocol.InferenceChunkPayload{Kind: kind, Chunk: raw})
	if err != nil {
		_ = sendError(out, id, fmt.Errorf("marshal chunk envelope: %w", err))
		return err
	}
	msg.Payload = payload
	return out.send(msg)
}

// sendInferenceDone terminates a streaming sequence or returns a single
// non-streaming response body.
func sendInferenceDone(out *wsOutbound, id string, response json.RawMessage) error {
	payload := protocol.InferenceResponsePayload{Done: true}
	if len(response) > 0 {
		payload.Response = response
	}
	msg := protocol.Message{ID: id, Type: protocol.MsgInferenceResponse}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	msg.Payload = raw
	return out.send(msg)
}

// sendBackendError reports a backend failure as a terminal inference response
// so the router fails the correlated call with a useful message.
func sendBackendError(out *wsOutbound, id string, err error) error {
	payload := protocol.InferenceResponsePayload{Done: true, Error: err.Error()}
	msg := protocol.Message{ID: id, Type: protocol.MsgInferenceResponse}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return marshalErr
	}
	msg.Payload = raw
	return out.send(msg)
}

// sendError reports a protocol-level error on the correlated call.
func sendError(out *wsOutbound, id string, err error) error {
	payload := protocol.ErrorPayload{Message: err.Error(), Code: "worker_error", ID: id}
	msg := protocol.Message{ID: id, Type: protocol.MsgError}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return marshalErr
	}
	msg.Payload = raw
	return out.send(msg)
}