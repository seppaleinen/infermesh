package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// watchdogRelayReadTimeout is how long the relay can go silent before the
// router force-closes the connection. It matches wsutil.ReadTimeout so the
// watchdog fires at the same point the relay's own idle deadline would.
// It is a var (not a const) so tests can shorten it.
var watchdogRelayReadTimeout = wsutil.ReadTimeout

// errRelayDisconnected is returned when a relay worker request cannot be
// sent because the relay connection is down.
var errRelayDisconnected = errors.New("relay connection unavailable")

// errRelayQueueFull is returned when the relay's outbound queue is full and
// the frame cannot be enqueued for the single writer goroutine. Callers
// treat it as a transient backpressure signal (issue #196).
var errRelayQueueFull = errors.New("relay send queue full")

// maxRelayCallTimeout is the hard cap on in-flight relay call duration.
// Even if the relay link stays technically alive but the worker stops responding,
// calls are failed within this timeout to prevent indefinite hangs (issue #197).
const maxRelayCallTimeout = 60 * time.Second

// canonicalConnectPath is the WebSocket endpoint the relay mounts for
// worker and router connections (see cmd/relay/main.go startWSServer). It is
// the same path the router's own /v1/connect handler serves, so both sides of
// a relay link target the canonical endpoint.
const canonicalConnectPath = "/v1/connect"

// normalizeRelayURL converts a relay WebSocket URL (possibly http://-style, as
// accepted by --relay-url) into a WebSocket URL targeting /v1/connect. A bare
// host/port gets the path appended; a user-supplied /v1/connect is stripped
// first so both "ws://host:port" and "ws://host:port/v1/connect" normalize
// to the same canonical URL (issue #191).
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
	// Strip a user-supplied /v1/connect (or trailing-slash variant) so the
	// canonical path is always appended exactly once. Other paths are kept
	// verbatim, so callers can still point at custom endpoints.
	if u.Path == canonicalConnectPath || u.Path == canonicalConnectPath+"/" {
		u.Path = ""
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = canonicalConnectPath
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

// enqueueRelay marshals msg and enqueues it onto relaySendCh for the single
// relayWriterLoop. It is the ONLY path for outbound relay frames: the reader
// loop and the HTTP-handler relayWorkerClient both enqueue here instead of
// writing to relayConn directly, so the connection has exactly one writer
// (issue #196). It honors ctx cancellation and the queue's SendQueueSize cap.
func (h *WSHub) enqueueRelay(ctx context.Context, msg protocol.Message) error {
	if !h.relayConnected() {
		return errRelayDisconnected
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal relay frame: %w", err)
	}
	select {
	case h.relaySendCh <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errRelayQueueFull
	}
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
	pc.handle = callPendingHandler(pc)
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
	pc.handle = streamPendingHandler(pc)
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
	if err := wsutil.WriteJSON(conn, readyMsg); err != nil {
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

// failAllRelayPending fails all pending relay calls with the given error.
// It snapshots the pending calls under the lock and fails them outside of
// it to avoid holding the lock during the fail() call (which itself acquires
// closeOnce). This is used by relayWriterLoop on write failure to provide
// fast-fail behavior for in-flight calls when the relay link wedges (issue #197).
func (h *WSHub) failAllRelayPending(err error) {
	h.relayPendingMu.Lock()
	pend := make([]*pendingCall, 0, len(h.relayPending))
	for _, pc := range h.relayPending {
		pend = append(pend, pc)
	}
	h.relayPendingMu.Unlock()
	for _, pc := range pend {
		pc.fail(err)
	}
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
			if err := wsutil.WriteText(conn, data); err != nil {
				h.log.Warn("failed to send to relay", "error", err)
				// Fail in-flight calls immediately so clients don't wait
				// 120s for the reader loop's defer to notice the disconnect.
				// The reader loop's defer still acts as a safety net for
				// non-wedge disconnects (reader-side EOF, etc.).
				h.failAllRelayPending(errRelayDisconnected)
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
	if conn == nil {
		return
	}

	defer func() { h.cleanupRelayExit(conn, done) }()

	// watchdog: if the relay goes silent for ReadTimeout, force the
	// connection closed so the blocked ReceiveJSON returns. This is the
	// fix for the lost-close-frame wedge: the relay's ws.Close() writes
	// its close frame to an internal bufio.Writer and then closes the
	// TCP socket without flushing, so the close frame is discarded. The
	// CNI path between the relay and router nodes can also lose the TCP
	// FIN. Without this watchdog, the router's reader loop blocks forever
	// in ReceiveJSON's read syscall and runRelay never re-dials (issue
	// #199).
	watchdogDone, watchdogReset := h.startRelayWatchdog(ctx, conn)
	defer close(watchdogDone)

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

		// Reset the watchdog: we received a frame, so the relay is alive.
		select {
		case watchdogReset <- struct{}{}:
		default:
		}

		// Handle relay-internal register messages — create synthetic connections.
		if msg.Type == protocol.MsgRegister {
			h.handleRelayRegister(msg)
			continue
		}

		// Echo relay keepalive pings through the single writer loop. The relay is
		// blocked in its read loop, so its own outbound ping does not reset
		// its idle deadline — the echo is what keeps the link alive (see
		// relayPingLoop in cmd/relay/main.go). The reader loop must NEVER
		// write relayConn (issue #196): enqueue the echo instead so
		// relayWriterLoop sends it.
		if msg.Type == protocol.MsgPing {
			reply := protocol.Message{ID: msg.ID, Type: protocol.MsgPing}
			if err := h.enqueueRelay(context.Background(), reply); err != nil {
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

// startRelayWatchdog starts a goroutine that force-closes the relay connection
// after watchdogRelayReadTimeout of silence. This is the fix for the lost-close-frame
// wedge: the relay's ws.Close() writes its close frame to an internal
// bufio.Writer and then closes the TCP socket without flushing, so the close
// frame is discarded (or the FIN is dropped by the CNI). Without this watchdog,
// the router's reader loop blocks forever in ReceiveJSON's read syscall.
// Returns a done channel and a reset channel; close done to stop, send to reset
// to restart the timer when a frame is received.
func (h *WSHub) startRelayWatchdog(ctx context.Context, conn *websocket.Conn) (chan struct{}, chan struct{}) {
	watchdogDone := make(chan struct{})
	watchdogReset := make(chan struct{}, 1)
	watchdog := time.NewTimer(watchdogRelayReadTimeout)
	defer watchdog.Stop()

	go func() {
		for {
			select {
			case <-watchdogDone:
				return
			case <-ctx.Done():
				return
			case <-watchdog.C:
				// Relay silent for watchdogRelayReadTimeout. Force the read
				// deadline to fire so ReceiveJSON's blocking read syscall
				// returns. SetReadDeadline on the underlying net.Conn is
				// goroutine-safe and works even while ReceiveJSON holds ws.rio.
				_ = conn.SetReadDeadline(time.Now())
				// Give ReceiveJSON a moment to return, then hard-close so
				// the reader loop's defer cleans up and runRelay re-dials.
				select {
				case <-watchdogDone:
					return
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				_ = conn.Close()
				return
			case <-watchdogReset:
				watchdog.Stop()
				watchdog.Reset(watchdogRelayReadTimeout)
			}
		}
	}()
	return watchdogDone, watchdogReset
}

// cleanupRelayExit handles the cleanup when the relay reader loop exits.
func (h *WSHub) cleanupRelayExit(conn *websocket.Conn, done chan struct{}) {
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
