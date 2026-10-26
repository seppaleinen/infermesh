package router

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
)

// errRelayDisconnected is returned when a relay worker request cannot be
// sent because the relay connection is down.
var errRelayDisconnected = errors.New("relay connection unavailable")

// normalizeRelayURL converts a relay WebSocket URL (possibly http://-style, as
// accepted by --relay-url) into a WebSocket URL targeting /v1/connect. A bare
// host/port gets the path appended; an explicit path is kept as-is so callers
// can point at custom endpoints.
func normalizeRelayURL(raw string) (string, error) {
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
		return "", fmt.Errorf("invalid relay url %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/connect"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
// relayConnected returns true if the relay connection is active.
func (h *WSHub) relayConnected() bool {
	h.relayMu.RLock()
	defer h.relayMu.RUnlock()
	return h.relayConn != nil
}
// relayConnSnapshot returns the current relay connection, or nil when the
// relay is not connected. Callers must treat a nil result as "relay down".
func (h *WSHub) relayConnSnapshot() *websocket.Conn {
	h.relayMu.RLock()
	defer h.relayMu.RUnlock()
	return h.relayConn
}
// registerRelayCall registers a pending call on the relay connection,
// keyed by message ID. Returns the pendingCall or an error if a duplicate
// ID already exists or the worker is at its MaxInFlight cap.
func (h *WSHub) registerRelayCall(id string, respCh chan protocol.Message, errCh chan error, workerID string) (*pendingCall, error) {
	pc := &pendingCall{
		id:       id,
		respCh:   respCh,
		errCh:    errCh,
		doneCh:   make(chan struct{}),
		conn:     nil, // no wsConnection; hub-level tracking
		hub:      h,
		workerID: workerID,
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
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	if h.relayInFlight[workerID] >= int64(h.maxInFlightFor(workerID)) {
		h.relayRejected429[workerID]++
		return nil, errRateLimited
	}
	if _, exists := h.relayPending[id]; exists {
		return nil, fmt.Errorf("duplicate relay pending message id %s", id)
	}
	h.relayInFlight[workerID]++
	pc.queuedAt = time.Now()
	h.relayPending[id] = pc
	return pc, nil
}
// registerRelayStream registers a streaming call on the relay connection.
func (h *WSHub) registerRelayStream(id string, chunkCh chan StreamEvent, errCh chan error, workerID string) (*pendingCall, error) {
	pc := &pendingCall{
		id:       id,
		chunkCh:  chunkCh,
		errCh:    errCh,
		doneCh:   make(chan struct{}),
		conn:     nil, // no wsConnection; hub-level tracking
		hub:      h,
		workerID: workerID,
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
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	if h.relayInFlight[workerID] >= int64(h.maxInFlightFor(workerID)) {
		h.relayRejected429[workerID]++
		return nil, errRateLimited
	}
	if _, exists := h.relayPending[id]; exists {
		return nil, fmt.Errorf("duplicate relay pending message id %s", id)
	}
	h.relayInFlight[workerID]++
	pc.queuedAt = time.Now()
	h.relayPending[id] = pc
	return pc, nil
}
// routeRelayResponse routes a response message (from the relay) to the
// pending call identified by msg.ID. Returns true if a pending call was
// found and the message was consumed; false otherwise.
func (h *WSHub) routeRelayResponse(msg protocol.Message) bool {
	h.relayPendingMu.Lock()
	pc, ok := h.relayPending[msg.ID]
	h.relayPendingMu.Unlock()
	if !ok {
		return false
	}
	pc.handle(msg)
	return true
}
// DialRelay dials the relay WebSocket connection, sends router_ready
// as the first frame, and starts reader/writer loops that bridge the
// relay connection into the hub's existing dispatch and connection maps.
func (h *WSHub) DialRelay(ctx context.Context, url string) error {
	normalized, err := normalizeRelayURL(url)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	conn, err := wsutil.Dial(ctx, normalized, "")
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}

	// Send router_ready as the first frame. On failure the connection is
	// discarded before it is ever published, so relayConnected() stays false.
	readyMsg := protocol.Message{Type: protocol.MsgRouterReady}
	if err := wsutil.SendJSON(conn, readyMsg); err != nil {
		_ = conn.Close()
		return fmt.Errorf("send router_ready: %w", err)
	}

	// Publish the new connection and a fresh drop-signal channel. The reader
	// loop closes `done` when this connection dies; runRelay waits on it to
	// decide when to re-dial.
	h.relayMu.Lock()
	h.relayConn = conn
	done := make(chan struct{})
	h.relayDone = done
	h.relayMu.Unlock()

	h.log.Info("connected to relay", "url", url)

	go h.relayReaderLoop(ctx, done)
	go h.relayWriterLoop(ctx, done)
	return nil
}
// relayRetry bounds the exponential backoff applied when re-dialing the
// relay after a failed or dropped connection.
const (
	relayRetryInitial = 2 * time.Second
	relayRetryMax     = 30 * time.Second
)
// runRelay maintains the outbound relay connection until ctx is cancelled:
// it dials immediately, then re-dials with exponential backoff whenever the
// link drops. The router's HTTP server is unaffected — only the relay link
// retries. It blocks; call it in a goroutine.
func (h *WSHub) runRelay(ctx context.Context, url string) {
	backoff := relayRetryInitial
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.DialRelay(ctx, url); err != nil {
			h.log.Warn("relay dial failed; retrying", "error", err, "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > relayRetryMax {
				backoff = relayRetryMax
			}
			continue
		}
		backoff = relayRetryInitial
		h.relayMu.RLock()
		done := h.relayDone
		h.relayMu.RUnlock()
		select {
		case <-done:
			h.log.Info("relay connection lost; re-dialing")
		case <-ctx.Done():
			return
		}
	}
}
// sleepCtx sleeps for d or until ctx is cancelled; reports whether the full
// duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
// relayWriterLoop sends messages from the relaySendCh to the relay connection.
// It captures the connection it owns once at startup (DialRelay publishes
// relayConn before spawning this loop) and only ever writes to that captured
// conn. It exits when ctx is cancelled, when the reader loop signals the
// connection dropped (done closed), or when a write fails, so the relay is
// never written from more than one goroutine at a time and an old loop can
// never touch a successor's connection.
func (h *WSHub) relayWriterLoop(ctx context.Context, done chan struct{}) {
	h.relayMu.RLock()
	conn := h.relayConn
	h.relayMu.RUnlock()
	if conn == nil {
		h.log.Warn("relay writer loop started without a connection")
		return
	}

	defer func() {
		h.relayMu.Lock()
		if h.relayConn == conn {
			h.relayConn = nil
		}
		h.relayMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		h.log.Info("relay writer loop exited")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			// The reader loop detected the disconnect; exit so the next
			// dial owns the conn.
			return
		case data, ok := <-h.relaySendCh:
			if !ok {
				return
			}
			if err := wsutil.SendText(conn, data); err != nil {
				h.log.Warn("failed to send to relay", "error", err)
				return
			}
		}
	}
}
// relayReaderLoop reads messages from the relay and dispatches them
// to the appropriate worker connection based on WorkerID. On exit it nils
// the relayConn field (under relayMu) and closes `done` so runRelay can
// re-dial.
func (h *WSHub) relayReaderLoop(ctx context.Context, done chan struct{}) {
	h.relayMu.RLock()
	conn := h.relayConn
	h.relayMu.RUnlock()

	defer func() {
		h.relayMu.Lock()
		if h.relayConn == conn {
			h.relayConn = nil
		}
		h.relayMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		// Fail in-flight relay calls so HTTP clients get a fast error
		// instead of waiting on a response that can never arrive (the
		// relay link is gone). Finished calls are no-ops (closeOnce).
		h.relayPendingMu.Lock()
		pend := make([]*pendingCall, 0, len(h.relayPending))
		for _, pc := range h.relayPending {
			pend = append(pend, pc)
		}
		h.relayPendingMu.Unlock()
		for _, pc := range pend {
			pc.fail(errRelayDisconnected)
		}
		// Clean up relay metric maps to prevent accumulation of stale worker IDs
		h.cleanupRelayMetrics()
		close(done)
		h.log.Info("relay reader loop exited")
	}()

	for {
		if err := wsutil.SetReadDeadline(conn, time.Now().Add(wsutil.ReadTimeout)); err != nil {
			return
		}
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(conn, &msg); err != nil {
			if errors.Is(err, wsutil.ErrMessageTooLarge) {
				h.log.Warn("oversized message from relay")
			} else {
				h.log.Debug("relay read failed", "error", err)
			}
			return
		}

		// Handle relay-internal register messages — create synthetic connections.
		if msg.Type == protocol.MsgRegister {
			h.handleRelayRegister(msg)
			continue
		}

		// Echo relay keepalive pings straight back. The relay is blocked in
		// its read loop, so its own outbound ping does not reset its idle
		// deadline — the echo is what keeps the link alive (see
		// relayPingLoop in cmd/relay/main.go).
		if msg.Type == protocol.MsgPing {
			reply := protocol.Message{ID: msg.ID, Type: protocol.MsgPing}
			if err := wsutil.SendJSON(conn, reply); err != nil {
				h.log.Debug("relay ping echo failed", "error", err)
				return
			}
			continue
		}

		// Route response-type messages to relay-level pending calls
		// (relayWorkerClient path). Returns true if consumed.
		consumed := false
		switch msg.Type {
		case protocol.MsgInferenceResponse, protocol.MsgModelLoadResponse,
			protocol.MsgError, protocol.MsgInferenceChunk:
			consumed = h.routeRelayResponse(msg)
		}

		// Messages from relay have WorkerID set by the worker.
		// Dispatch to the synthetic wsConnection for wsWorkerClient pending
		// calls and non-response types (heartbeats, capabilities, pings).
		// Skip if routeRelayResponse already consumed the message.
		if msg.WorkerID != "" {
			if !consumed {
				h.mu.RLock()
				c, ok := h.conns[msg.WorkerID]
				h.mu.RUnlock()
				if ok {
					c.hub.dispatch(c, msg)
				} else {
					h.log.Debug("no worker connection for relay message", "worker_id", msg.WorkerID)
				}
			}
		} else {
			// Broadcast to all workers or log
			h.mu.RLock()
			conns := make([]*wsConnection, 0, len(h.conns))
			for _, c := range h.conns {
				conns = append(conns, c)
			}
			h.mu.RUnlock()
			for _, c := range conns {
				c.hub.dispatch(c, msg)
			}
		}
	}
}
// cleanupRelayMetrics removes zero-value entries from relay metric maps
// to prevent accumulation of stale worker IDs when the relay connection drops.
func (h *WSHub) handleRelayRegister(msg protocol.Message) {
	var reg protocol.RegisterPayload
	if err := msg.DecodePayload(&reg); err != nil {
		h.log.Warn("bad relay register payload", "error", err)
		return
	}
	worker := reg.Worker
	if worker.ID == "" {
		h.log.Warn("relay register without worker id")
		return
	}

	// Validate API key if configured
	if h.apiKey != "" && worker.APIKey != h.apiKey {
		h.log.Warn("invalid API key for relay registration", "worker", worker.ID)
		return
	}

	worker.Transport = protocol.TransportWS
	if worker.Status == "" {
		worker.Status = protocol.StatusAvailable
	}

	conn := &wsConnection{
		hub:     h,
		id:      worker.ID,
		ws:      nil, // no real socket — relay handles transport
		worker:  worker,
		sendCh:  make(chan []byte, h.sendQueueSize),
		doneCh:  make(chan struct{}),
		pending: make(map[string]*pendingCall),
	}

	// One active connection per worker ID: a newer connection supersedes the old one.
	h.mu.Lock()
	if old, ok := h.conns[worker.ID]; ok {
		h.log.Info("superseding existing relay connection", "worker", worker.ID)
		old.enqueueClose("superseded")
	}
	h.conns[worker.ID] = conn
	h.mu.Unlock()

	h.bridge(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: worker})

	// Do NOT start writerLoop — there is no socket; relaySendCh handles outbound.
	// Do NOT start readerLoop — the relay reader loop remains the single reader.

	welcome, _ := protocol.NewMessage(protocol.MsgWelcome, protocol.WelcomePayload{
		WorkerID: worker.ID,
		Message:  "registered via relay",
	})
	conn.enqueue(welcome)
	h.log.Info("websocket worker registered", "worker", worker.ID, "ip", worker.IP, "port", worker.Port)
}
