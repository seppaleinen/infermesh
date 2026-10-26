package router

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
)

// errRateLimited is returned when a worker is at its MaxInFlight cap. It is
// surfaced to HTTP clients as an OpenAI-shaped 429 with Retry-After.
var errRateLimited = errors.New("rate limited: max in-flight calls reached for this worker")

// errConnLimit is returned when the router's global connection cap is reached.
// It is surfaced to the dialing worker as a protocol-level error frame so the
// worker can back off and retry (issue #54).
var errConnLimit = errors.New("router at max connections; try again later")

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
	relayInFlight    map[string]int64
	relayRejected429 map[string]int64
	relayRejected504 map[string]int64

	// qm holds pool-level queue-wait observability (reservoir percentiles).
	qm *queueMetrics

	writeTimeout  time.Duration
	sendQueueSize int
	apiKey        string

	// maxInFlight caps concurrent in-flight calls per worker (streaming +
	// non-streaming + model-load). 0 means defaultMaxInFlight.
	maxInFlight int

	// workerMaxInFlight holds per-worker concurrency caps from capabilities.
	// A value of 0 means "use hub default". Protected by mu.
	workerMaxInFlight map[string]int

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
// NewWSHub creates a hub bridging WebSocket worker connections into the
// registry. reg may be nil (events are then dropped).
func NewWSHub(reg registry.Registry, log *slog.Logger) *WSHub {
	return &WSHub{
		log:               log,
		reg:               reg,
		conns:             make(map[string]*wsConnection),
		relaySendCh:       make(chan []byte, wsutil.SendQueueSize),
		relayDone:         make(chan struct{}),
		relayPending:      make(map[string]*pendingCall),
		relayInFlight:     make(map[string]int64),
		relayRejected429:  make(map[string]int64),
		relayRejected504:  make(map[string]int64),
		qm:                newQueueMetrics(),
		writeTimeout:      wsutil.WriteTimeout,
		sendQueueSize:     wsutil.SendQueueSize,
		maxInFlight:       defaultMaxInFlight,
		maxConnections:    defaultMaxConnections,
		workerMaxInFlight: make(map[string]int),
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

// SetWorkerMaxInFlight sets the concurrency cap for a specific worker.
// A value <= 0 removes the override (falls back to hub default).
func (h *WSHub) SetWorkerMaxInFlight(workerID string, cap int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cap <= 0 {
		delete(h.workerMaxInFlight, workerID)
	} else {
		h.workerMaxInFlight[workerID] = cap
	}
}

// maxInFlightFor returns the effective cap for a worker connection.
func (h *WSHub) maxInFlightFor(workerID string) int {
	h.mu.RLock()
	wcap, ok := h.workerMaxInFlight[workerID]
	h.mu.RUnlock()
	if ok && wcap > 0 {
		return wcap
	}
	if h.maxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return h.maxInFlight
}

// MaxInFlight returns the effective per-worker in-flight cap, resolving the
// 0 sentinel to defaultMaxInFlight. Read-only; safe to call concurrently.
// Exposed so the busy-status projection and scheduler can derive thresholds
// from the configured cap rather than a hardcode.
func (h *WSHub) MaxInFlight() int {
	return h.maxInFlightFor("")
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
// bridge forwards a discovery event into the registry, which in turn feeds
// the capability cache and /v1/workers.
func (h *WSHub) bridge(event protocol.DiscoveryEvent) {
	if h.reg == nil {
		return
	}
	if err := h.reg.HandleEvent(event); err != nil {
		h.log.Warn("registry event failed", "type", event.Type, "worker", event.Worker.ID, "error", err)
	}
	// Also forward RegistryRemoved/Unavailable events to clean up workerMaxInFlight
	if event.Type == protocol.EventRemoved || event.Type == protocol.EventExpired {
		h.HandleRegistryEvent(event)
	}
}

// HandleRegistryEvent processes registry-level events to clean up worker state.
// Currently used to prune workerMaxInFlight entries when workers leave the registry.
func (h *WSHub) HandleRegistryEvent(event protocol.DiscoveryEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.workerMaxInFlight, event.Worker.ID)
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
		// Sync per-worker concurrency cap
		if p.Capabilities.ConcurrencyCap > 0 {
			h.SetWorkerMaxInFlight(c.id, p.Capabilities.ConcurrencyCap)
		}

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
