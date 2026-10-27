package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/internal/wsutil"
)

// errConnClosed is delivered to in-flight calls when their connection dies.
var errConnClosed = errors.New("websocket connection closed")

// errWorkerInferenceError is wrapped around errors reported by a WebSocket
// worker's backend (e.g. LM Studio 404, model not loaded, backend crash).
// It is a retryable sentinel so dispatchWithFailover can fail over to the
// next worker advertising the same model.
var errWorkerInferenceError = errors.New("worker reported inference error")

// wsConnection is one worker's WebSocket connection. It owns the socket, the
// outbound send queue and the map of in-flight calls keyed by message ID.
type wsConnection struct {
	hub *WSHub
	id  string
	ws  *websocket.Conn

	sendCh chan []byte
	doneCh chan struct{}

	lastRead atomic.Int64

	// queueDepth counts inference requests sitting in sendCh awaiting
	// the writer loop. It is incremented in send() and decremented in
	// writerLoop, so it reflects real backpressure on the outbound path
	// (the reader-loop-freeze vector from issue #58).
	queueDepth atomic.Int64

	stateMu sync.RWMutex
	worker  protocol.WorkerInfo // latest known snapshot (register/heartbeat/caps)

	pendingMu sync.Mutex
	pending   map[string]*pendingCall

	// Per-worker observability counters (guarded by pendingMu).
	rejected429 int64
	rejected504 int64

	closeOnce sync.Once

	// slot is this connection's semaphore slot, acquired by handleConn
	// before any per-connection state is allocated. It is released exactly
	// once: eagerly by the superseding connection (so a 1:1 replacement
	// never holds the pool at capacity) or by this connection's own shutdown.
	// Nil when the cap is disabled (no semaphore was ever created).
	slot         chan struct{}
	slotReleased sync.Once
}
// queueDepthSnapshot returns the current queue depth.
func (c *wsConnection) queueDepthSnapshot() int64 {
	return c.queueDepth.Load()
}
func (c *wsConnection) setWorker(w protocol.WorkerInfo) {
	c.stateMu.Lock()
	c.worker = w
	c.stateMu.Unlock()
}

func (c *wsConnection) workerSnapshot() protocol.WorkerInfo {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.worker
}
// readerLoop is the single reader goroutine for the connection.
func (c *wsConnection) readerLoop() {
	defer c.shutdown("read loop exited")
	for {
		if err := wsutil.SetReadDeadline(c.ws, time.Now().Add(wsutil.ReadTimeout)); err != nil {
			c.hub.log.Debug("set read deadline failed", "worker", c.id, "error", err)
			return
		}
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(c.ws, &msg); err != nil {
			if errors.Is(err, wsutil.ErrMessageTooLarge) {
				c.hub.log.Warn("oversized websocket message", "worker", c.id)
			} else {
				c.hub.log.Debug("websocket read failed", "worker", c.id, "error", err)
			}
			return
		}
		c.lastRead.Store(time.Now().UnixNano())
		c.hub.dispatch(c, msg)
	}
}
// writerLoop is the single writer goroutine for the connection. It applies
// the write deadline to every frame and shuts the connection down on failure.
func (c *wsConnection) writerLoop() {
	for {
		select {
		case data := <-c.sendCh:
			c.queueDepth.Add(-1)
			if err := wsutil.SetWriteDeadline(c.ws, time.Now().Add(c.hub.writeTimeout)); err != nil {
				c.shutdown("write deadline error")
				return
			}
			if err := wsutil.SendText(c.ws, data); err != nil {
				c.hub.log.Warn("websocket write failed", "worker", c.id, "error", err)
				c.shutdown("write error")
				return
			}
		case <-c.doneCh:
			return
		}
	}
}
// send enqueues a marshaled envelope, honoring ctx cancellation.
func (c *wsConnection) send(ctx context.Context, msg protocol.Message) error {
	// If relay is configured, send through relay with WorkerID set
	if c.hub.relayConnected() {
		msg.WorkerID = c.id
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal websocket message: %w", err)
		}
		c.hub.mu.Lock()
		sendCh := c.hub.relaySendCh
		c.hub.mu.Unlock()

		select {
		case sendCh <- data:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-c.doneCh:
			return errConnClosed
		}
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal websocket message: %w", err)
	}
	c.queueDepth.Add(1)
	select {
	case c.sendCh <- data:
		return nil
	case <-ctx.Done():
		c.queueDepth.Add(-1)
		return ctx.Err()
	case <-c.doneCh:
		c.queueDepth.Add(-1)
		return errConnClosed
	}
}
// enqueue enqueues a message without blocking; returns false if the queue is
// full or the connection is closing (the message is dropped).
func (c *wsConnection) enqueue(msg protocol.Message) bool {
	// If relay is configured, send through relay with WorkerID set
	if c.hub.relayConnected() {
		msg.WorkerID = c.id
		data, err := json.Marshal(msg)
		if err != nil {
			return false
		}
		c.hub.mu.Lock()
		sendCh := c.hub.relaySendCh
		c.hub.mu.Unlock()

		select {
		case sendCh <- data:
			return true
		case <-c.doneCh:
			return false
		default:
			return false
		}
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	select {
	case c.sendCh <- data:
		return true
	case <-c.doneCh:
		return false
	default:
		c.hub.log.Warn("websocket send queue full; dropping message", "worker", c.id, "type", msg.Type)
		return false
	}
}
// enqueueClose politely asks the peer to close, then force-closes if it does
// not comply within a grace period.
// In relay mode, the relay connection is shared and should not be closed.
func (c *wsConnection) enqueueClose(reason string) bool {
	msg, err := protocol.NewMessage(protocol.MsgClose, protocol.ClosePayload{Reason: reason})
	if err != nil {
		return false
	}
	ok := c.enqueue(msg)

	// In relay mode, don't force-close the shared relay connection.
	if c.hub.relayConnected() {
		return ok
	}

	go func() {
		select {
		case <-time.After(3 * time.Second):
			c.shutdown("close grace period expired")
		case <-c.doneCh:
		}
	}()
	return ok
}
// shutdown tears the connection down exactly once: it closes the socket,
// removes the connection from the hub, fails in-flight calls and deregisters
// the worker from the registry.
func (c *wsConnection) shutdown(reason string) {
	c.closeOnce.Do(func() {
		c.hub.log.Info("websocket connection closed", "worker", c.id, "reason", reason)
		close(c.doneCh)
		if c.ws != nil {
			_ = c.ws.Close()
		}

		c.hub.mu.Lock()
		if cur, ok := c.hub.conns[c.id]; ok && cur == c {
			delete(c.hub.conns, c.id)
		}
		c.hub.mu.Unlock()

		c.pendingMu.Lock()
		pend := make([]*pendingCall, 0, len(c.pending))
		for _, pc := range c.pending {
			pend = append(pend, pc)
		}
		c.pending = make(map[string]*pendingCall)
		c.pendingMu.Unlock()
		for _, pc := range pend {
			pc.fail(errConnClosed)
		}

		w := c.workerSnapshot()
		if w.ID != "" {
			c.hub.bridge(protocol.DiscoveryEvent{Type: protocol.EventRemoved, Worker: w})
		}
	})
}
// routeResponse delivers a chunk/response message to its pending call.
func (c *wsConnection) routeResponse(msg protocol.Message) {
	c.pendingMu.Lock()
	pc, ok := c.pending[msg.ID]
	c.pendingMu.Unlock()
	if !ok {
		c.hub.log.Debug("response for unknown request id", "worker", c.id, "id", msg.ID)
		return
	}
	pc.handle(msg)
}
// failPending terminates the pending call for id with err.
func (c *wsConnection) failPending(id string, err error) {
	c.pendingMu.Lock()
	pc, ok := c.pending[id]
	c.pendingMu.Unlock()
	if ok {
		pc.fail(err)
	}
}
// addPending registers a pending call, rejecting duplicate IDs and rejecting
// the call when the worker is at its MaxInFlight cap (returns errRateLimited).
//
// The cap is checked atomically with the enqueue so a concurrent dequeue
// cannot race a new enqueue past the limit: a queued request that is
// dequeued still re-checks the cap here.
func (c *wsConnection) addPending(pc *pendingCall) error {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if _, exists := c.pending[pc.id]; exists {
		return fmt.Errorf("duplicate pending message id %s", pc.id)
	}
	if int64(len(c.pending)) >= int64(c.hub.maxInFlightFor(c.id)) {
		c.hub.recordRejectionLocked(c)
		return errRateLimited
	}
	pc.queuedAt = time.Now()
	c.pending[pc.id] = pc
	return nil
}
// registerStream registers a streaming inference call. Inbound chunks are
// framed as SSE events (data: <json>\n\n); the terminal response emits
// "data: [DONE]" with Done=true ahead of the channel closing.
func (c *wsConnection) registerStream(id string, chunkCh chan StreamEvent, errCh chan error) (*pendingCall, error) {
	pc := &pendingCall{
		conn:    c,
		id:      id,
		chunkCh: chunkCh,
		errCh:   errCh,
		doneCh:  make(chan struct{}),
	}
	pc.handle = func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MsgInferenceChunk:
			var p protocol.InferenceChunkPayload
			if err := msg.DecodePayload(&p); err != nil {
				pc.fail(fmt.Errorf("bad chunk payload: %w", err))
				return
			}
			framed := append([]byte("data: "), p.Chunk...)
			framed = append(framed, '\n', '\n')
			select {
			case pc.chunkCh <- StreamEvent{Data: framed}:
			case <-pc.doneCh:
			}
		case protocol.MsgInferenceResponse:
			var p protocol.InferenceResponsePayload
			if err := msg.DecodePayload(&p); err != nil {
				pc.fail(fmt.Errorf("bad response payload: %w", err))
				return
			}
			if p.Error != "" {
				pc.fail(errors.New(p.Error))
				return
			}
			if len(p.Response) > 0 {
				framed := append([]byte("data: "), p.Response...)
				framed = append(framed, '\n', '\n')
				select {
				case pc.chunkCh <- StreamEvent{Data: framed}:
				case <-pc.doneCh:
				}
			}
			if p.Done {
				pc.completeStream()
			}
		case protocol.MsgError:
			var p protocol.ErrorPayload
			_ = msg.DecodePayload(&p)
			pc.fail(errors.New(p.Message))
		}
	}
	if err := c.addPending(pc); err != nil {
		return nil, err
	}
	return pc, nil
}
// registerCall registers a single-response call (model load, non-streaming
// inference). The first correlated response is forwarded to respCh.
func (c *wsConnection) registerCall(id string, respCh chan protocol.Message, errCh chan error) (*pendingCall, error) {
	pc := &pendingCall{
		conn:   c,
		id:     id,
		respCh: respCh,
		errCh:  errCh,
		doneCh: make(chan struct{}),
	}
	pc.handle = func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MsgInferenceResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			}
			pc.completeCall()
		case protocol.MsgModelLoadResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			}
			pc.completeCall()
		case protocol.MsgError:
			var p protocol.ErrorPayload
			_ = msg.DecodePayload(&p)
			pc.fail(errors.New(p.Message))
		}
	}
	if err := c.addPending(pc); err != nil {
		return nil, err
	}
	return pc, nil
}
// workerInFlight returns the current in-flight count for worker id. The
// counter is owned by the connection's pendingMu; callers must not hold
// pendingMu across this call.
func (c *wsConnection) workerInFlight() int64 {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return int64(len(c.pending))
}
// releaseSlot releases this connection's semaphore slot exactly once. It is
// a no-op when the cap is disabled (slot is nil).
func (c *wsConnection) releaseSlot() {
	c.slotReleased.Do(func() {
		if c.slot == nil {
			return
		}
		select {
		case <-c.slot:
		default:
		}
		c.slot = nil
	})
}
// wsWorkerClient sends inference/load requests over a worker's WebSocket
// connection and consumes the correlated responses.
type wsWorkerClient struct {
	conn *wsConnection
	log  *slog.Logger
}
func (c *wsWorkerClient) Transport() string { return protocol.TransportWS }
func (c *wsWorkerClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent, wsutil.SendQueueSize)
	errCh := make(chan error, 1)

	req, err := inferenceRequest(kind, body)
	if err != nil {
		errCh <- err
		close(chunkCh)
		return chunkCh, errCh
	}
	msg, err := protocol.NewMessage(protocol.MsgInferenceRequest, req)
	if err != nil {
		errCh <- err
		close(chunkCh)
		return chunkCh, errCh
	}
	pc, err := c.conn.registerStream(msg.ID, chunkCh, errCh)
	if err != nil {
		errCh <- err
		close(chunkCh)
		return chunkCh, errCh
	}
	if err := c.conn.send(ctx, msg); err != nil {
		pc.fail(err)
		return chunkCh, errCh
	}
	go func() {
		select {
		case <-ctx.Done():
			pc.fail(ctx.Err())
		case <-pc.doneCh:
		}
	}()
	return chunkCh, errCh
}
func (c *wsWorkerClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	body, err := forceNonStreaming(body)
	if err != nil {
		return nil, err
	}
	req, err := inferenceRequest(kind, body)
	if err != nil {
		return nil, err
	}
	msg, err := protocol.NewMessage(protocol.MsgInferenceRequest, req)
	if err != nil {
		return nil, err
	}
	respCh := make(chan protocol.Message, 4)
	errCh := make(chan error, 1)
	pc, err := c.conn.registerCall(msg.ID, respCh, errCh)
	if err != nil {
		return nil, err
	}
	if err := c.conn.send(ctx, msg); err != nil {
		pc.fail(err)
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			pc.fail(ctx.Err())
		case <-pc.doneCh:
		}
	}()
	return awaitResponse(ctx, respCh, errCh)
}
func (c *wsWorkerClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	msg, err := protocol.NewMessage(protocol.MsgModelLoadRequest, protocol.ModelLoadRequestPayload{Model: model})
	if err != nil {
		return false, err
	}
	respCh := make(chan protocol.Message, 4)
	errCh := make(chan error, 1)
	pc, err := c.conn.registerCall(msg.ID, respCh, errCh)
	if err != nil {
		return false, err
	}
	if err := c.conn.send(ctx, msg); err != nil {
		pc.fail(err)
		return false, err
	}
	go func() {
		select {
		case <-ctx.Done():
			pc.fail(ctx.Err())
		case <-pc.doneCh:
		}
	}()
	select {
	case m, ok := <-respCh:
		if !ok {
			return false, errConnClosed
		}
		var p protocol.ModelLoadResponsePayload
		if err := m.DecodePayload(&p); err != nil {
			return false, fmt.Errorf("decode model load response: %w", err)
		}
		if p.Error != "" {
			return false, errors.New(p.Error)
		}
		return p.Loaded, nil
	case err := <-errCh:
		return false, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
func (c *wsWorkerClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	// WebSocket workers push capabilities on register/heartbeat; the router
	// never dials back, so return the hub's latest snapshot.
	return c.conn.workerSnapshot().Capabilities, nil
}
func (c *wsWorkerClient) Close() error {
	c.conn.enqueueClose("client close")
	return nil
}
// RecordTimeout bumps the per-worker 504 counter for this direct-connection
// worker, surfaced by /v1/queue/stats.
func (c *wsWorkerClient) RecordTimeout(workerID string) {
	c.conn.hub.recordTimeout(workerID)
}
// awaitResponse reads the single correlated response of a call.
func awaitResponse(ctx context.Context, respCh chan protocol.Message, errCh chan error) ([]byte, error) {
	for {
		select {
		case m, ok := <-respCh:
			if !ok {
				return nil, errConnClosed
			}
			var p protocol.InferenceResponsePayload
			if err := m.DecodePayload(&p); err != nil {
				return nil, fmt.Errorf("decode inference response: %w", err)
			}
			if p.Error != "" {
				return nil, fmt.Errorf("%w: %w", errWorkerInferenceError, errors.New(p.Error))
			}
			if len(p.Response) > 0 {
				return append([]byte(nil), p.Response...), nil
			}
			if p.Done {
				return nil, fmt.Errorf("%w: worker returned empty response", errWorkerInferenceError)
			}
		case err := <-errCh:
			return nil, fmt.Errorf("%w: %w", errWorkerInferenceError, err)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
// inferenceRequest builds the WS protocol request payload from an
// OpenAI-compatible request body.
func inferenceRequest(kind string, body []byte) (protocol.InferenceRequestPayload, error) {
	var rb struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &rb); err != nil {
		return protocol.InferenceRequestPayload{}, fmt.Errorf("invalid request body: %w", err)
	}
	return protocol.InferenceRequestPayload{Kind: kind, Model: rb.Model, Body: body}, nil
}
// forceNonStreaming rewrites body so the worker returns a single response.
func forceNonStreaming(body []byte) ([]byte, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	m["stream"] = false
	return json.Marshal(m)
}
