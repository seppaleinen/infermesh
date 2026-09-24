package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
)

// errConnClosed is delivered to in-flight calls when their connection dies.
var errConnClosed = errors.New("websocket connection closed")

// errRelayDisconnected is returned when a relay worker request cannot be
// sent because the relay connection is down.
var errRelayDisconnected = errors.New("relay connection unavailable")

// WSHub manages outbound WebSocket connections from workers. Workers dial the
// router (never the reverse), so the hub holds an active connection per worker
// ID and multiplexes inference traffic over it.
//
// Concurrency: each connection has exactly one reader goroutine (readerLoop)
// and exactly one writer goroutine (writerLoop); all outbound frames go
// through the connection's send queue.
type WSHub struct {
	log *slog.Logger
	reg registry.Registry

	mu    sync.RWMutex
	conns map[string]*wsConnection

	relayURL    string
	relayMu     sync.RWMutex
	relayConn   *websocket.Conn
	relaySendCh chan []byte
	relayDone   chan struct{}

	// Relay-specific pending calls for correlating requests/responses when
	// workers connect via relay (no direct connection).
	relayPendingMu sync.Mutex
	relayPending   map[string]*pendingCall

	// relayInFlight tracks per-worker in-flight call counts across relay
	// shares (the relay connection is shared, so counts live on the hub).
	// relayRejected429 tracks per-worker 429 rejections in relay mode.
	// relayRejected504 tracks per-worker 504 (gateway timeout) rejections
	// in relay mode; surfaced by /v1/queue/stats alongside the direct-
	// connection wsConnection.rejected504 counter.
	relayInFlight     map[string]int64
	relayRejected429  map[string]int64
	relayRejected504  map[string]int64

	// qm holds pool-level queue-wait observability (reservoir percentiles).
	qm *queueMetrics

	writeTimeout  time.Duration
	sendQueueSize int
	apiKey        string

	// maxInFlight caps concurrent in-flight calls per worker (streaming +
	// non-streaming + model-load). 0 means defaultMaxInFlight.
	maxInFlight int

	// maxConnections caps the total number of concurrent worker WebSocket
	// connections (semaphore). 0 means defaultMaxConnections. Excess
	// handshakes are rejected with a 503 before any per-connection state is
	// allocated, so a connection flood cannot exhaust memory or file
	// descriptors (issue #54).
	maxConnections int

	// connSem is the buffered channel acting as the connection semaphore.
	// nil until SetMaxConnections is called (or until the first handshake,
	// which lazily initialises it from the default).
	connSem chan struct{}
}

// defaultMaxInFlight is the per-worker concurrent-call cap applied when the
// router is started without --max-in-flight. A slow/stalled HTTP client can
// fill chunkCh and freeze the reader loop (heartbeats, pings and all other
// in-flight streams stall) without this cap.
const defaultMaxInFlight = 4

// defaultMaxConnections is the total worker WebSocket connection cap applied
// when the router is started without --max-connections. It bounds the number
// of concurrently accepted handshakes so a connection flood cannot exhaust
// memory, file descriptors or goroutines (issue #54).
const defaultMaxConnections = 256

// connAcquireTimeout bounds how long handleConn waits for a connection slot
// before rejecting the handshake. Kept short so a slow accept queue does not
// hold the HTTP server's goroutine pool hostage.
const connAcquireTimeout = 2 * time.Second

// errRateLimited is returned when a worker is at its MaxInFlight cap. It is
// surfaced to HTTP clients as an OpenAI-shaped 429 with Retry-After.
var errRateLimited = errors.New("rate limited: max in-flight calls reached for this worker")

// errConnLimit is returned when the router's global connection cap is reached.
// It is surfaced to the dialing worker as a protocol-level error frame so the
// worker can back off and retry (issue #54).
var errConnLimit = errors.New("router at max connections; try again later")

// acquireConn acquires a connection slot, returning the slot handle that
// identifies it for release. It returns nil when the cap is disabled (no
// semaphore was ever created) — callers that want to release must treat
// nil as a no-op.
func (h *WSHub) acquireConn(ctx context.Context) (chan struct{}, error) {
	sem := h.connSemFor()
	select {
	case sem <- struct{}{}:
		return sem, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errConnLimit
	}
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

// NewWSHub creates a hub bridging WebSocket worker connections into the
// registry. reg may be nil (events are then dropped).
func NewWSHub(reg registry.Registry, log *slog.Logger) *WSHub {
	return &WSHub{
		log:              log,
		reg:              reg,
		conns:            make(map[string]*wsConnection),
		relaySendCh:      make(chan []byte, wsutil.SendQueueSize),
		relayDone:        make(chan struct{}),
		relayPending:     make(map[string]*pendingCall),
		relayInFlight:    make(map[string]int64),
		relayRejected429: make(map[string]int64),
		relayRejected504: make(map[string]int64),
		qm:               newQueueMetrics(),
		writeTimeout:     wsutil.WriteTimeout,
		sendQueueSize:    wsutil.SendQueueSize,
		maxInFlight:      defaultMaxInFlight,
		maxConnections:   defaultMaxConnections,
	}
}

// SetMaxInFlight overrides the per-worker concurrent-call cap. A value <= 0
// falls back to defaultMaxInFlight.
func (h *WSHub) SetMaxInFlight(n int) {
	if n <= 0 {
		h.maxInFlight = defaultMaxInFlight
		return
	}
	h.maxInFlight = n
}

// maxInFlightFor returns the effective cap for a worker connection.
func (h *WSHub) maxInFlightFor() int {
	if h.maxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return h.maxInFlight
}

// SetMaxConnections overrides the total worker WebSocket connection cap. A
// value <= 0 falls back to defaultMaxConnections. The semaphore is created
// lazily on first use so NewWSHub stays allocation-free when the cap is never
// overridden.
func (h *WSHub) SetMaxConnections(n int) {
	if n <= 0 {
		n = defaultMaxConnections
	}
	h.maxConnections = n
	h.connSem = make(chan struct{}, n)
}

// connSemFor returns the connection semaphore, creating it from the default
// cap on first use so callers never observe a nil channel. Safe to call
// concurrently.
func (h *WSHub) connSemFor() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connSem == nil {
		h.maxConnections = defaultMaxConnections
		h.connSem = make(chan struct{}, defaultMaxConnections)
	}
	return h.connSem
}

// workerInFlight returns the current in-flight count for worker id. The
// counter is owned by the connection's pendingMu; callers must not hold
// pendingMu across this call.
func (c *wsConnection) workerInFlight() int64 {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return int64(len(c.pending))
}

// SetAPIKey sets the API key used to validate worker registrations.
func (h *WSHub) SetAPIKey(key string) {
	h.apiKey = key
}

// SetRelayURL configures the relay WebSocket URL. When set, the hub
// does not accept direct worker connections via Handler(); instead,
// DialRelay must be called to connect to the relay.
func (h *WSHub) SetRelayURL(url string) {
	h.relayURL = url
}

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
	if h.relayInFlight[workerID] >= int64(h.maxInFlightFor()) {
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
	if h.relayInFlight[workerID] >= int64(h.maxInFlightFor()) {
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

// isRateLimited reports whether the worker identified by id is at its
// MaxInFlight cap. For direct-connection workers the count is the size of
// the connection's pending map; for relay-mode workers it is the hub-side
// per-worker counter. The check is read-only and does not mutate state, so
// callers must atomically record the rejection themselves if the check
// passes (see addPending / registerRelayCall).
func (h *WSHub) isRateLimited(workerID string) bool {
	h.mu.RLock()
	c, ok := h.conns[workerID]
	h.mu.RUnlock()
	if ok {
		return c.workerInFlight() >= int64(h.maxInFlightFor())
	}
	return h.relayInFlightAt(workerID) >= int64(h.maxInFlightFor())
}

// relayInFlightAt returns the current in-flight count for worker id in relay
// mode. Safe to call concurrently; the counter is only mutated under
// relayPendingMu, which is also the lock guarding the relayPending map.
func (h *WSHub) relayInFlightAt(workerID string) int64 {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	return h.relayInFlight[workerID]
}

// decrementRelayInFlight decrements the per-worker in-flight counter for
// relay-mode calls. Called from pendingCall.finish for relay-level calls.
func (h *WSHub) decrementRelayInFlight(workerID string) {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	if h.relayInFlight[workerID] <= 0 {
		return
	}
	h.relayInFlight[workerID]--
}

// recordRejection bumps the per-worker 429 counter for a direct-connection
// worker. The counter is stored on the connection so it survives only for
// the life of that connection (matching the per-worker stats semantics).
// Safe to call from outside any pendingMu lock.
func (h *WSHub) recordRejection(workerID string, code int) {
	h.mu.RLock()
	c, ok := h.conns[workerID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	c.pendingMu.Lock()
	c.rejected429++
	c.pendingMu.Unlock()
}

// recordRejectionLocked bumps the per-worker 429 counter for a direct-
	// connection worker. The caller MUST hold c.pendingMu (used by addPending
	// to avoid a re-entrant lock deadlock).
	func (h *WSHub) recordRejectionLocked(c *wsConnection) {
		c.rejected429++
	}

	// recordTimeout bumps the per-worker 504 counter for a direct-connection
	// worker. Safe to call from outside any pendingMu lock.
	func (h *WSHub) recordTimeout(workerID string) {
		h.mu.RLock()
		c, ok := h.conns[workerID]
		h.mu.RUnlock()
		if !ok {
			return
		}
		c.pendingMu.Lock()
		c.rejected504++
		c.pendingMu.Unlock()
	}

	// recordRelayTimeout bumps the per-worker 504 counter for a relay-mode
	// worker. Safe to call concurrently.
	func (h *WSHub) recordRelayTimeout(workerID string) {
		h.relayPendingMu.Lock()
		defer h.relayPendingMu.Unlock()
		h.relayRejected504[workerID]++
	}

// recordWait records a queue-wait sample into the pool reservoir for the
// worker whose call just completed. Called from pendingCall.finish.
func (h *WSHub) recordWait(workerID string, queuedAt time.Time) {
	if h.qm != nil {
		h.qm.recordWait(time.Since(queuedAt))
	}
}

// connSnapshot returns the current connection map under a brief RLock.
// Callers must not retain references to the returned slice's elements
// across a mutation of the hub's conn map.
func (h *WSHub) connSnapshot() []*wsConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*wsConnection, 0, len(h.conns))
	for _, c := range h.conns {
		out = append(out, c)
	}
	return out
}

// avgWaitFor returns the average queue-wait duration for worker id based
// on the pool reservoir's samples. This is a coarse estimate (reservoir
// sampling is not per-worker), but it is read-only and fast.
func (h *WSHub) avgWaitFor(workerID string) time.Duration {
	if h.qm == nil {
		return 0
	}
	p50, _, _ := h.qm.percentiles()
	return p50
}

// relayRejected429Total returns the sum of per-worker 429 counts across
// all relay-mode workers.
func (h *WSHub) relayRejected429Total() int64 {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	var total int64
	for _, v := range h.relayRejected429 {
		total += v
	}
	return total
}

// relayRejected504Total returns the sum of per-worker 504 (gateway timeout)
// counts across all relay-mode workers.
func (h *WSHub) relayRejected504Total() int64 {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	var total int64
	for _, v := range h.relayRejected504 {
		total += v
	}
	return total
}

// relayInFlightSnapshot returns a copy of the per-worker in-flight counts
// for relay-mode workers.
func (h *WSHub) relayInFlightSnapshot() map[string]int64 {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	out := make(map[string]int64, len(h.relayInFlight))
	for k, v := range h.relayInFlight {
		out[k] = v
	}
	return out
}

// queuePercentiles returns the 50th, 95th and 99th percentiles of the
// pool-level queue-wait reservoir.
func (h *WSHub) queuePercentiles() (time.Duration, time.Duration, time.Duration) {
	if h.qm == nil {
		return 0, 0, 0
	}
	return h.qm.percentiles()
}

// QueueStats returns a snapshot of the pool's queue observability for the
// /v1/queue/stats endpoint. Read-only; safe to call concurrently.
func (h *WSHub) QueueStats() QueueStatsResponse {
	conns := h.connSnapshot()
	workers := make([]WorkerQueueStats, 0, len(conns))
	var (
		totalQueued      int64
		totalInFlight    int64
		totalRejected429 int64
		totalRejected504 int64
	)
	for _, c := range conns {
		depth := c.queueDepthSnapshot()
		inFlight := c.workerInFlight()
		rej429 := c.rejected429Snapshot()
		rej504 := c.rejected504Snapshot()
		avgWait := 0.0
		if inFlight > 0 {
			avgWait = floatMs(h.avgWaitFor(c.id))
		}
		workers = append(workers, WorkerQueueStats{
			WorkerID:         c.id,
			QueueDepth:       depth,
			InFlight:         inFlight,
			AvgWaitMs:        avgWait,
			Rejected429Total: rej429,
			Rejected504Total: rej504,
		})
		totalQueued += depth
		totalInFlight += inFlight
		totalRejected429 += rej429
		totalRejected504 += rej504
	}

	// Pool-level relay counters (relay-mode workers have no direct conn).
	for _, inflight := range h.relayInFlightSnapshot() {
		totalInFlight += inflight
	}
	totalRejected429 += h.relayRejected429Total()
	totalRejected504 += h.relayRejected504Total()

	p50, p95, p99 := h.queuePercentiles()
	return QueueStatsResponse{
		Workers: workers,
		Pool: PoolQueueStats{
			TotalQueued:      totalQueued,
			TotalInFlight:    totalInFlight,
			TotalRejected429: totalRejected429,
			TotalRejected504: totalRejected504,
			QueueTimeP50Ms:   floatMs(p50),
			QueueTimeP95Ms:   floatMs(p95),
			QueueTimeP99Ms:   floatMs(p99),
		},
	}
}

// rejected429For returns the per-worker 429 count for a direct-connection
// worker. Safe to call concurrently.
func (c *wsConnection) rejected429Snapshot() int64 {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return c.rejected429
}

// rejected504For returns the per-worker 504 count for a direct-connection
// worker.
func (c *wsConnection) rejected504Snapshot() int64 {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return c.rejected504
}

// Handler returns the /v1/connect upgrade handler.
func (h *WSHub) Handler() http.Handler {
	if h.relayURL != "" {
		return nil
	}
	return wsutil.Server(h.handleConn)
}

// Start launches the idle ping loop that keeps liveness traffic flowing on
// otherwise quiet connections.
func (h *WSHub) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(wsutil.DefaultPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.mu.RLock()
				conns := make([]*wsConnection, 0, len(h.conns))
				for _, c := range h.conns {
					conns = append(conns, c)
				}
				h.mu.RUnlock()
				msg, err := protocol.NewMessage(protocol.MsgPing, protocol.PingPayload{Timestamp: time.Now().Unix()})
				if err != nil {
					continue
				}
				for _, c := range conns {
					c.enqueue(msg)
				}
			}
		}
	}()
}

// Client returns a WorkerClient bound to the active WebSocket connection for
// id, or nil if the worker is not connected over WebSocket.
func (h *WSHub) Client(id string) WorkerClient {
	h.mu.RLock()
	c, ok := h.conns[id]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return &wsWorkerClient{conn: c, log: h.log}
}

// handleConn is invoked once per upgraded connection. The first frame must be
// a register message; afterwards the worker's heartbeats, capabilities pushes
// and inference responses are dispatched until the connection dies.
//
// The global connection semaphore bounds the number of *registered* worker
// connections so a connection flood cannot exhaust memory, file descriptors
// or goroutines (issue #54). Validation (register read, API key, peer IP,
// collision check) happens before the slot is acquired, because a supersede
// (same worker ID, same peer) must release the incumbent's slot before it can
// acquire its own — otherwise a 1:1 replacement would deadlock at capacity.
//
// The acquired slot is stored on the connection (conn.slot) and released
// exactly once: eagerly by a superseding connection (so a 1:1 replacement
// never holds the pool at capacity) or by this connection's own shutdown.
func (h *WSHub) handleConn(ws *websocket.Conn) {
	// --- Validation (no slot held) -------------------------------------
	// Cheap, bounded work: one register read + auth checks. A flood of
	// malformed registrations is bounded by the HTTP server's own goroutine
	// pool and the 64 MiB frame cap; it never spawns reader/writer loops or
	// grows the connection map.

	var msg protocol.Message
	if err := wsutil.ReceiveJSON(ws, &msg); err != nil {
		h.log.Warn("websocket register read failed", "error", err)
		_ = ws.Close()
		return
	}
	if msg.Type != protocol.MsgRegister {
		h.log.Warn("first websocket message is not a register", "type", msg.Type)
		_ = sendErrorRaw(ws, "first message must be register")
		_ = ws.Close()
		return
	}
	var reg protocol.RegisterPayload
	if err := msg.DecodePayload(&reg); err != nil {
		h.log.Warn("bad register payload", "error", err)
		_ = sendErrorRaw(ws, "bad register payload")
		_ = ws.Close()
		return
	}
	worker := reg.Worker
	if worker.ID == "" {
		h.log.Warn("register without worker id")
		_ = sendErrorRaw(ws, "register requires worker id")
		_ = ws.Close()
		return
	}

	// Validate API key if configured
	if h.apiKey != "" && worker.APIKey != h.apiKey {
		h.log.Warn("invalid API key for websocket registration", "worker", worker.ID)
		_ = sendErrorRaw(ws, "invalid or missing API key")
		_ = ws.Close()
		return
	}

	// Derive trusted peer IP from the WebSocket connection (authoritative).
	// This replaces any client-provided IP to prevent spoofing.
	//
	// NOTE: ws.RemoteAddr() returns the WebSocket Origin URL on server-side
	// connections (see x/net/websocket), NOT the TCP peer address. Using it
	// here would dereference a nil Origin and panic. The underlying HTTP
	// request's RemoteAddr is the real TCP peer — ws.Request() is non-nil
	// for server-side connections.
	req := ws.Request()
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		// Normalize any loopback peer to canonical routable IPv4 loopback.
		ip = net.IPv4(127, 0, 0, 1)
	}
	if ip == nil || ip.To4() == nil {
		h.log.Warn("unable to determine routable worker IPv4 from websocket peer", "peer", req.RemoteAddr)
		_ = sendErrorRaw(ws, "unable to determine routable worker IPv4 from peer address")
		_ = ws.Close()
		return
	}
	peerIP := ip.String()

	// Collision check: reject if this ID is already registered by a different
	// peer. This prevents worker ID spoofing / registry poisoning.
	if existing, ok := h.reg.Get(worker.ID); ok {
		if existing.IP != peerIP {
			h.log.Warn("rejected websocket registration: worker ID already claimed by different peer",
				"id", worker.ID, "existing_ip", existing.IP, "peer_ip", peerIP)
			_ = sendErrorRaw(ws, "worker ID already registered by another peer")
			_ = ws.Close()
			return
		}
		// Same peer IP: allow re-registration (legitimate reconnect/restart).
		h.log.Info("websocket worker re-registered from same peer", "id", worker.ID, "peer_ip", peerIP)
	}

	// Override worker IP with the trusted peer identity.
	worker.IP = peerIP
	worker.Transport = protocol.TransportWS
	if worker.Status == "" {
		worker.Status = protocol.StatusAvailable
	}

	// --- Slot acquisition ----------------------------------------------
	// One active connection per worker ID: supersede existing connection
	// (only reachable for same peer since different peers are rejected above).
	h.mu.Lock()
	if old, ok := h.conns[worker.ID]; ok {
		h.log.Info("superseding existing websocket connection (same peer)", "worker", worker.ID)
		// Release the incumbent's slot BEFORE acquiring a new one so a 1:1
		// replacement never holds the pool at capacity.
		old.releaseSlot()
		old.enqueueClose("superseded")
	}
	h.mu.Unlock()

	// Acquire a slot. The upgrade has already happened, so the socket is
	// ours; if we cannot get a slot we close it cleanly rather than leaking
	// the accepted socket.
	ctx, cancel := context.WithTimeout(context.Background(), connAcquireTimeout)
	slot, err := h.acquireConn(ctx)
	cancel()
	if err != nil {
		h.log.Warn("rejected websocket connection: at max connections", "error", err)
		_ = sendErrorRaw(ws, errConnLimit.Error())
		_ = ws.Close()
		return
	}

	conn := &wsConnection{
		hub:     h,
		id:      worker.ID,
		ws:      ws,
		worker:  worker,
		sendCh:  make(chan []byte, h.sendQueueSize),
		doneCh:  make(chan struct{}),
		pending: make(map[string]*pendingCall),
		slot:    slot,
	}

	h.mu.Lock()
	h.conns[worker.ID] = conn
	h.mu.Unlock()

	h.bridge(protocol.DiscoveryEvent{Type: protocol.EventAdded, Worker: worker})

	go conn.writerLoop()
	welcome, _ := protocol.NewMessage(protocol.MsgWelcome, protocol.WelcomePayload{
		WorkerID: worker.ID,
		Message:  "registered",
	})
	conn.enqueue(welcome)
	h.log.Info("websocket worker registered", "worker", worker.ID, "ip", worker.IP, "port", worker.Port)

	// Release the slot when this connection dies. readerLoop blocks until the
	// connection ends, so the deferred release runs after shutdown() has
	// already removed the connection from the map.
	defer conn.releaseSlot()

	// Block until the connection ends; the HTTP server goroutine stays
	// parked here for the life of the connection.
	conn.readerLoop()
}

// handleRelayRegister processes a MsgRegister received through the relay.
// It creates a synthetic wsConnection (no real socket) so that the worker
// appears in h.conns and can receive outbound frames via relaySendCh.
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

// bridge forwards a discovery event into the registry, which in turn feeds
// the capability cache and /v1/workers.
func (h *WSHub) bridge(event protocol.DiscoveryEvent) {
	if h.reg == nil {
		return
	}
	if err := h.reg.HandleEvent(event); err != nil {
		h.log.Warn("registry event failed", "type", event.Type, "worker", event.Worker.ID, "error", err)
	}
}

// sendErrorRaw writes an error message on a not-yet-registered connection.
func sendErrorRaw(ws *websocket.Conn, message string) error {
	msg, err := protocol.NewMessage(protocol.MsgError, protocol.ErrorPayload{Message: message})
	if err != nil {
		return err
	}
	return wsutil.SendJSON(ws, msg)
}

// dispatch routes an inbound message for a registered connection.
func (h *WSHub) dispatch(c *wsConnection, msg protocol.Message) {
	switch msg.Type {
	case protocol.MsgHeartbeat:
		var p protocol.HeartbeatPayload
		if err := msg.DecodePayload(&p); err != nil {
			h.log.Warn("bad heartbeat payload", "worker", c.id, "error", err)
			return
		}
		if p.Worker.ID == "" {
			p.Worker.ID = c.id
		}
		p.Worker.Transport = protocol.TransportWS
		c.setWorker(p.Worker)
		h.bridge(protocol.DiscoveryEvent{Type: protocol.EventUpdated, Worker: p.Worker})

	case protocol.MsgCapabilities:
		var p protocol.CapabilitiesPayload
		if err := msg.DecodePayload(&p); err != nil {
			h.log.Warn("bad capabilities payload", "worker", c.id, "error", err)
			return
		}
		w := c.workerSnapshot()
		w.Capabilities = p.Capabilities
		w.Transport = protocol.TransportWS
		c.setWorker(w)
		h.bridge(protocol.DiscoveryEvent{Type: protocol.EventUpdated, Worker: w})

	case protocol.MsgPing:
		// Keepalive echo from the worker. lastRead is refreshed by the
		// reader loop, so nothing else to do.

	case protocol.MsgInferenceChunk, protocol.MsgInferenceResponse, protocol.MsgModelLoadResponse:
		c.routeResponse(msg)

	case protocol.MsgError:
		var p protocol.ErrorPayload
		_ = msg.DecodePayload(&p)
		if p.ID != "" {
			c.failPending(p.ID, errors.New(p.Message))
		} else {
			h.log.Warn("worker websocket error", "worker", c.id, "message", p.Message, "code", p.Code)
		}

	case protocol.MsgClose:
		var p protocol.ClosePayload
		_ = msg.DecodePayload(&p)
		h.log.Info("worker closed websocket connection", "worker", c.id, "reason", p.Reason)
		c.shutdown("worker requested close")

	default:
		h.log.Debug("ignoring unexpected websocket message", "worker", c.id, "type", msg.Type)
	}
}

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
	if int64(len(c.pending)) >= int64(c.hub.maxInFlightFor()) {
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

// pendingCall correlates an in-flight request with its response(s).
type pendingCall struct {
	conn     *wsConnection
	hub      *WSHub // non-nil for relay-level calls (conn == nil); used to reap the hub-side pending map entry
	workerID string // non-empty for relay-level calls; decrements hub-side in-flight on finish
	id       string
	handle   func(protocol.Message)
	queuedAt time.Time // set by addPending; used to measure queue-wait for stats

	chunkCh chan StreamEvent      // stream flavor: SSE-framed events
	respCh  chan protocol.Message // call flavor: raw correlated messages
	errCh   chan error

	doneCh     chan struct{}
	closeOnce  sync.Once
	removeOnce sync.Once
}

// completeStream terminates a streaming call: it emits the [DONE] sentinel
// (best effort — dropped if the consumer stopped draining) and closes the
// channels exactly once.
func (pc *pendingCall) completeStream() {
	pc.closeOnce.Do(func() {
		select {
		case pc.chunkCh <- StreamEvent{Data: []byte("data: [DONE]\n\n"), Done: true}:
		default:
		}
		pc.finish()
	})
}

// completeCall terminates a single-response call.
func (pc *pendingCall) completeCall() {
	pc.closeOnce.Do(func() {
		pc.finish()
	})
}

// fail terminates the call with err, delivered on errCh.
func (pc *pendingCall) fail(err error) {
	pc.closeOnce.Do(func() {
		select {
		case pc.errCh <- err:
		default:
		}
		pc.finish()
	})
}

func (pc *pendingCall) finish() {
	pc.removeOnce.Do(func() {
		if pc.conn != nil {
			pc.conn.pendingMu.Lock()
			if cur, ok := pc.conn.pending[pc.id]; ok && cur == pc {
				delete(pc.conn.pending, pc.id)
			}
			pc.conn.pendingMu.Unlock()
			if pc.hub != nil {
				pc.hub.recordWait(pc.conn.id, pc.queuedAt)
			}
			return
		}
		// Relay-level call (registered in the hub's relayPending map): reap
		// the hub-side entry so IDs don't accumulate across relay sessions.
		if pc.hub != nil {
			pc.hub.relayPendingMu.Lock()
			if cur, ok := pc.hub.relayPending[pc.id]; ok && cur == pc {
				delete(pc.hub.relayPending, pc.id)
			}
			pc.hub.relayPendingMu.Unlock()
			if pc.workerID != "" {
				pc.hub.decrementRelayInFlight(pc.workerID)
				pc.hub.recordWait(pc.workerID, pc.queuedAt)
			}
		}
	})
	close(pc.doneCh)
	if pc.chunkCh != nil {
		close(pc.chunkCh)
	}
	if pc.respCh != nil {
		close(pc.respCh)
	}
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
				return nil, errors.New(p.Error)
			}
			if len(p.Response) > 0 {
				return append([]byte(nil), p.Response...), nil
			}
			if p.Done {
				return nil, errors.New("worker returned empty response")
			}
		case err := <-errCh:
			return nil, err
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

// relayWorkerClient is a WorkerClient that sends inference/model-load
// requests over a shared relay WebSocket connection. It is used when
// workers connect via relay (no direct connection exists) and the
// normal HTTP fallback is blocked by the firewall.
type relayWorkerClient struct {
	hub *WSHub
	log *slog.Logger
}

func newRelayWorkerClient(hub *WSHub, log *slog.Logger) *relayWorkerClient {
	return &relayWorkerClient{hub: hub, log: log}
}

// Transport returns "ws" for relay connections.
func (c *relayWorkerClient) Transport() string { return protocol.TransportWS }

// Complete sends a non-streaming inference request via the relay and
// awaits the correlated inference response.
func (c *relayWorkerClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
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
	msg.WorkerID = worker.ID

	respCh := make(chan protocol.Message, 4)
	errCh := make(chan error, 1)
	pc, err := c.hub.registerRelayCall(msg.ID, respCh, errCh, worker.ID)
	if err != nil {
		return nil, err
	}
	conn := c.hub.relayConnSnapshot()
	if conn == nil {
		pc.fail(errRelayDisconnected)
		return nil, errRelayDisconnected
	}
	if err := wsutil.SendJSON(conn, msg); err != nil {
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

// Stream sends an inference request via the relay and returns a channel
// of SSE-framed stream events plus an error channel.
func (c *relayWorkerClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
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
	msg.WorkerID = worker.ID

	pc, err := c.hub.registerRelayStream(msg.ID, chunkCh, errCh, worker.ID)
	if err != nil {
		errCh <- err
		close(chunkCh)
		return chunkCh, errCh
	}
	conn := c.hub.relayConnSnapshot()
	if conn == nil {
		pc.fail(errRelayDisconnected)
		return chunkCh, errCh
	}
	if err := wsutil.SendJSON(conn, msg); err != nil {
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

// LoadModel sends a model load request via the relay and awaits the
// correlated model load response.
func (c *relayWorkerClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	msg, err := protocol.NewMessage(protocol.MsgModelLoadRequest, protocol.ModelLoadRequestPayload{Model: model})
	if err != nil {
		return false, err
	}
	msg.WorkerID = worker.ID

	respCh := make(chan protocol.Message, 4)
	errCh := make(chan error, 1)
	pc, err := c.hub.registerRelayCall(msg.ID, respCh, errCh, worker.ID)
	if err != nil {
		return false, err
	}
	conn := c.hub.relayConnSnapshot()
	if conn == nil {
		pc.fail(errRelayDisconnected)
		return false, errRelayDisconnected
	}
	if err := wsutil.SendJSON(conn, msg); err != nil {
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

// Capabilities returns the worker's capabilities from the registry.
// No HTTP call is needed; the relay worker is already registered.
func (c *relayWorkerClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	h := c.hub
	h.mu.RLock()
	conn, ok := h.conns[worker.ID]
	h.mu.RUnlock()
	if ok {
		return conn.workerSnapshot().Capabilities, nil
	}
	// Fallback: try the registry.
	if h.reg != nil {
		w, ok := h.reg.Get(worker.ID)
		if ok {
			return w.Capabilities, nil
		}
	}
	return protocol.Capabilities{}, errors.New("worker not found")
}

// Close is a no-op for relay workers; the shared relay connection
// stays open.
func (c *relayWorkerClient) Close() error { return nil }

// RecordTimeout bumps the per-worker 504 counter for this relay-mode
// worker, surfaced by /v1/queue/stats.
func (c *relayWorkerClient) RecordTimeout(workerID string) {
	c.hub.recordRelayTimeout(workerID)
}
