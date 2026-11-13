package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/internal/wsutil"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"golang.org/x/net/websocket"
)

// --- Flags ---

var (
	relayListen = flag.String("listen", ":8090", "address to listen on for worker and router connections")
	relayDev    = flag.Bool("dev-mode", false, "dev mode (no auth, loopback only)")
)

// Relay is a WebSocket broker that bridges workers and routers.
// Workers connect outbound to the relay and send "register" as their
// first message. The router connects outbound and sends "router_ready"
// as its first message. The relay routes messages between them using
// the worker_id field in the message envelope.
type Relay struct {
	log        *slog.Logger
	listenAddr string

	mu          sync.RWMutex
	workers     map[string]*websocket.Conn // worker ID → connection
	routerConn  *websocket.Conn            // single router connection
	routerMu    sync.Mutex
	workerQueue []protocol.Message // buffered messages when router is disconnected

	// writeTimeout is the per-frame write deadline applied by the single
	// writer loops (routerWriterLoop, workerWriterLoop). It is captured at
	// loop start so tests can configure it at Relay construction time
	// instead of racing a package-global (issue #203).
	writeTimeout time.Duration

	// routerSendCh is the single outbound queue for the relay's current
	// router connection. Every goroutine that wants to send to the router —
	// the worker read loops (sendToRouter), the ping loop, and the buffered-
	// message flush — enqueues marshaled frames here instead of writing
	// directly. routerWriterLoop is the ONLY goroutine that ever calls
	// wsutil.WriteText on routerConn, so the connection has exactly one
	// writer and frames cannot be interleaved (issue #196).
	//
	// The channel is recreated per router connection under routerMu and set
	// to nil by the writer on exit, so enqueue callers snapshot it under the
	// same lock and never send to a channel whose writer has gone. The
	// channel is never closed (sending to a closed channel panics), so the
	// writer drains its own snapshot and the old channel is GC'd.
	routerSendCh chan []byte

	// workerSendCh is the single outbound queue for each worker connection.
	// workerWriterLoop is the ONLY goroutine that ever calls wsutil.WriteText
	// on a worker socket, so frames from the read loop's heartbeat echo and
	// from routerReadLoop's forwardToWorker/broadcastToWorkers cannot be
	// interleaved (issue #203 — the relay↔worker side was missed by #196).
	//
	// The channel is created per connection in handleWorker under r.mu,
	// atomically with the socket in r.workers, and removed by the writer
	// loop on exit, so enqueueWorkerFrame never delivers a frame into a
	// channel whose writer has gone.
	workerSendCh map[string]chan []byte
}

func main() {
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if *relayDev {
		log.Info("relay starting in dev mode", "listen", *relayListen)
	} else {
		log.Info("relay starting", "listen", *relayListen)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relay := &Relay{
		log:          log,
		listenAddr:   *relayListen,
		workers:      make(map[string]*websocket.Conn),
		workerQueue:  make([]protocol.Message, 0),
		workerSendCh: make(map[string]chan []byte),
		writeTimeout: wsutil.WriteTimeout,
	}

	// Start WebSocket server (both workers and routers connect here)
	go relay.startWSServer(ctx)

	// Wait for shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("shutting down")
	cancel()
}

// startWSServer starts the WebSocket server that accepts connections
// from both workers and routers at /v1/connect.
func (r *Relay) startWSServer(ctx context.Context) {
	r.log.Info("relay WebSocket server listening", "addr", r.listenAddr)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/connect", func(w http.ResponseWriter, req *http.Request) {
		wsutil.Server(r.handleWS).ServeHTTP(w, req)
	})

	server := &http.Server{
		Addr:    r.listenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.log.Error("relay server error", "error", err)
	}
}

// handleWS handles a new WebSocket connection. It reads the first
// message to determine if this is a worker or router connection.
func (r *Relay) handleWS(ws *websocket.Conn) {
	// Read first message to determine connection type
	if err := wsutil.SetReadDeadline(ws, time.Now().Add(10*time.Second)); err != nil {
		r.log.Warn("failed to set read deadline", "error", err)
		_ = ws.Close()
		return
	}

	var msg protocol.Message
	if err := wsutil.ReceiveJSON(ws, &msg); err != nil {
		r.log.Warn("failed to read first message", "error", err)
		_ = ws.Close()
		return
	}

	switch msg.Type {
	case protocol.MsgRegister:
		r.handleWorker(ws, msg)
	case protocol.MsgRouterReady:
		r.handleRouter(ws)
	default:
		r.log.Warn("unknown connection type", "type", msg.Type)
		_ = ws.Close()
	}
}

// routableIP extracts the real client source IP from a relay's accepted
// WebSocket connection. A worker behind NAT advertises its private IP (see
// pkg/worker/host.go LocalIP); the relay knows the actual dialer address and
// must rewrite it so the fleet router can route back to the worker. Returns ""
// when no source is determinable — the caller then keeps the worker's
// advertised IP unchanged.
func routableIP(ws *websocket.Conn) string {
	if ws == nil {
		return ""
	}
	req := ws.Request()
	if req == nil || req.RemoteAddr == "" {
		return ""
	}
	return ipFromRemote(req.RemoteAddr)
}

// ipFromRemote parses a net.Addr string (host or host:port, IPv4 or IPv6) into
// a routable IP string. Prefers an IPv4 — workers advertise IPv4, and this
// unwraps a v4-in-v6 mapped form into a clean dotted-quad — falling back to the
// address as-is for IPv6-only input. Returns "" on any parse failure so callers
// keep the worker's advertised IP unchanged.
func ipFromRemote(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// RemoteAddr without a port (rare); parse directly.
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String() // IPv6-only fallback
}

// newRelayNonce returns a random hex token that identifies one worker
// registration. The relay injects it into the register frame; the router
// stores it on the synthetic conn and matches a worker_gone signal against
// it, so a signal that arrives after a reconnect prunes the right conn.
func newRelayNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// handleWorker registers a worker connection and starts relay loops.
func (r *Relay) handleWorker(ws *websocket.Conn, msg protocol.Message) {
	var reg protocol.RegisterPayload
	if err := msg.DecodePayload(&reg); err != nil {
		r.log.Warn("bad worker register payload", "error", err)
		_ = ws.Close()
		return
	}
	workerID := reg.Worker.ID
	if workerID == "" {
		r.log.Warn("worker register without ID")
		_ = ws.Close()
		return
	}

	// NAT reachability fix: a worker behind NAT advertises its private IP (see
	// pkg/worker/host.go LocalIP). The relay knows the real client source and
	// rewrites the advertised IP so the fleet router can route back to it. All
	// other Worker fields are preserved by re-marshalling the whole payload;
	// when no routable source is determinable we keep the original IP as-is.
	if routable := routableIP(ws); routable != "" {
		reg.Worker.IP = routable
		if data, err := json.Marshal(reg); err == nil {
			msg.Payload = json.RawMessage(data)
		} else {
			r.log.Warn("failed to re-marshal rewritten register; keeping original payload", "worker_id", workerID, "error", err)
		}
	}

	r.mu.Lock()
	r.workers[workerID] = ws
	// Publish a fresh outbound queue for this connection, atomically with
	// the socket, so enqueueWorkerFrame never delivers a frame into a
	// channel whose writer has exited (issue #203).
	sendCh := make(chan []byte, wsutil.SendQueueSize)
	r.workerSendCh[workerID] = sendCh
	// Stamp a per-connection nonce onto the register so the router can
	// match a later worker_gone signal to THIS connection specifically.
	nonce := newRelayNonce()
	reg.RelayNonce = nonce
	if data, err := json.Marshal(reg); err == nil {
		msg.Payload = json.RawMessage(data)
	} else {
		r.log.Warn("failed to re-marshal register with relay nonce; keeping original payload", "worker_id", workerID, "error", err)
	}
	r.mu.Unlock()

	// Start the single writer goroutine for this worker connection BEFORE
	// flushing or reading: every outbound frame (heartbeat echo from the
	// read loop, forwarded router frames) enqueues onto sendCh and only
	// this loop writes them to the socket (issue #203).
	go r.workerWriterLoop(ws, workerID, sendCh)

	// Forward the register to the router so it knows this worker exists.
	msg.WorkerID = workerID

	r.routerMu.Lock()
	conn := r.routerConn
	r.routerMu.Unlock()

	if conn != nil {
		if err := r.sendToRouter(context.Background(), msg); err != nil {
			r.log.Warn("failed to forward worker register to router; buffering", "worker_id", workerID, "error", err)
			r.bufferWorkerMessage(msg)
		}
	} else {
		// Buffer the register for when the router connects
		if r.bufferWorkerMessage(msg) {
			r.log.Debug("buffered worker register (router disconnected)", "worker_id", workerID)
		}
	}

	r.log.Info("worker connected", "worker_id", workerID)

	// If router is connected, flush buffered messages
	r.flushBufferedMessages()

	// Block until the connection closes (handler must not return or
	// net/http will close the WebSocket connection immediately).
	// Pass the nonce explicitly: a worker_gone signal must carry THIS
	// socket's identity. Reading r.workerNonces[workerID] at exit time
	// would race a reconnect — the old socket's reader loop could exit
	// after the worker re-registered, pick up the NEW nonce, and prune
	// the still-live successor (issue #201).
	r.workerReadLoop(ws, workerID, nonce)
}

// handleRouter registers a router connection and starts relay loops.
func (r *Relay) handleRouter(ws *websocket.Conn) {
	r.routerMu.Lock()
	old := r.routerConn
	if old != nil {
		// A new router superseded the old one: tear the old socket down so
		// its reader loop exits and its writer stops below.
		r.log.Warn("router already connected, closing old connection")
		_ = old.Close()
	}
	r.routerConn = ws
	// Publish a fresh outbound queue for this connection, atomically with
	// the connection. Snapshot callers (sendToRouter, flush, ping) read it
	// under the same lock, so they never enqueue onto a channel whose
	// writer has exited.
	sendCh := make(chan []byte, wsutil.SendQueueSize)
	r.routerSendCh = sendCh
	r.routerMu.Unlock()

	r.log.Info("router connected")

	// done signals this connection's writer and ping loops to stop; it is
	// closed exactly once, after the reader loop returns.
	done := make(chan struct{})

	// Start the single writer goroutine for the router connection BEFORE
	// flushing buffered messages: the flush enqueues frames onto sendCh,
	// and only this loop writes them to the socket (issue #196).
	go r.routerWriterLoop(ws, sendCh, done)

	// Flush buffered messages for all workers when router reconnects
	r.flushBufferedMessages()

	// The relay is otherwise silent between inference requests: the router
	// only writes to it when dispatching work. Both sides apply a 90s idle
	// read deadline, so an idle router<->relay link is torn down every 90s
	// and re-dialled on a timer. Keep it alive with protocol pings: the
	// relay pings the router, and the router echoes the ping back, so each
	// side sees inbound traffic and resets its idle deadline.
	go r.relayPingLoop(done)

	// Block until the connection closes (handler must not return or
	// net/http will close the WebSocket connection immediately).
	r.routerReadLoop(ws)

	// Connection gone: stop this connection's writer and ping loops.
	close(done)
}

// workerReadLoop reads messages from a worker and forwards them to the router.
// nonce is the identity the relay stamped onto this worker's register frame;
// the worker_gone signal it emits on disconnect carries it, so the router
// matches the signal to THIS connection rather than a successor that
// reconnected under the same worker ID (issue #201).
//
// This loop is a READER ONLY (issue #203): its heartbeat echo enqueues onto
// workerSendCh for the single writer goroutine. Writing the echo directly
// here would race routerReadLoop's forwardToWorker on the same socket.
func (r *Relay) workerReadLoop(ws *websocket.Conn, workerID, nonce string) {
	defer func() {
		r.mu.Lock()
		delete(r.workers, workerID)
		delete(r.workerSendCh, workerID)
		r.mu.Unlock()

		// Notify the router that this worker's connection is gone so it can
		// prune the synthetic conn immediately, instead of waiting up to the
		// 120s registry sweep. Only sent when a router is connected: with no
		// router, no register was ever forwarded, so there is no stale conn
		// to prune (issue #201).
		if nonce != "" && r.routerConnected() {
			if err := r.sendWorkerGone(context.Background(), workerID, nonce); err != nil {
				r.log.Debug("failed to send worker_gone to router", "worker_id", workerID, "error", err)
			}
		}
		r.log.Info("worker disconnected", "worker_id", workerID)
	}()

	// This loop is a READER ONLY (issue #203): its heartbeat echo enqueues onto
	// workerSendCh for the single writer goroutine. Writing the echo directly
	// here would race routerReadLoop's forwardToWorker on the same socket.
	for {
		if err := wsutil.SetReadDeadline(ws, time.Now().Add(wsutil.ReadTimeout)); err != nil {
			return
		}
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(ws, &msg); err != nil {
			if errors.Is(err, wsutil.ErrMessageTooLarge) {
				r.log.Warn("oversized message from worker", "worker_id", workerID)
			} else {
				r.log.Debug("worker read failed", "worker_id", workerID, "error", err)
			}
			return
		}

		// Echo heartbeats back to the worker. Workers behind NAT/proxy/firewall
		// (the common case for remote inference workers) have no inbound path,
		// so the relay must send traffic outbound to them — otherwise the
		// intermediate proxy drops the idle link at its own timeout (often
		// 60s, well under the worker's 90s read deadline). The worker's
		// wsReaderLoop sees the echo and resets its read deadline. The echo
		// is forwarded to the router too, so the registry stays current.
		//
		// The echo is ENQUEUED for the single writer goroutine, not written
		// directly: writing here would race routerReadLoop's forwardToWorker
		// on the same socket (issue #203).
		if msg.Type == protocol.MsgHeartbeat {
			echo := protocol.Message{ID: msg.ID, Type: protocol.MsgHeartbeat}
			if data, err := json.Marshal(echo); err == nil {
				r.enqueueWorkerFrame(workerID, data)
			} else {
				r.log.Debug("failed to marshal worker heartbeat echo", "worker_id", workerID, "error", err)
			}
		}

		// Stamp WorkerID before forwarding to router or buffering
		msg.WorkerID = workerID

		// Forward to router or buffer if router is disconnected
		r.routerMu.Lock()
		conn := r.routerConn
		r.routerMu.Unlock()

		if conn != nil {
			if err := r.sendToRouter(context.Background(), msg); err != nil {
				r.log.Warn("failed to forward worker message to router", "worker_id", workerID, "error", err)
			}
		} else {
			// Buffer message for when router reconnects
			r.bufferWorkerMessage(msg)
			r.log.Debug("buffered worker message (router disconnected)", "worker_id", workerID)
		}
	}
}

// routerReadLoop reads messages from the router and routes them to workers.
func (r *Relay) routerReadLoop(ws *websocket.Conn) {
	defer func() {
		r.routerMu.Lock()
		if r.routerConn == ws {
			r.routerConn = nil
		}
		r.routerMu.Unlock()
		// Close the socket so the router's reader loop gets EOF and exits,
		// signaling runRelay to re-dial. Without this, a read-timeout exit
		// leaves a half-open socket: the router keeps reading forever, the
		// relay never sees its done channel close, and worker heartbeats
		// buffer indefinitely (issue #199).
		_ = ws.Close()
		r.log.Info("router disconnected")
	}()

	for {
		if err := wsutil.SetReadDeadline(ws, time.Now().Add(wsutil.ReadTimeout)); err != nil {
			return
		}
		var msg protocol.Message
		if err := wsutil.ReceiveJSON(ws, &msg); err != nil {
			if errors.Is(err, wsutil.ErrMessageTooLarge) {
				r.log.Warn("oversized message from router")
			} else {
				r.log.Debug("router read failed", "error", err)
			}
			return
		}

		// Route message to worker(s) based on WorkerID
		if msg.WorkerID != "" {
			r.forwardToWorker(msg)
		} else {
			// Broadcast to all workers
			r.broadcastToWorkers(msg)
		}
	}
}

// forwardToWorker sends a message to a specific worker. It enqueues the
// marshaled frame onto the worker's outbound queue; workerWriterLoop is the
// ONLY goroutine that writes the socket, so frames from the read loop's
// heartbeat echo and from this router-driven path cannot interleave
// (issue #203).
func (r *Relay) forwardToWorker(msg protocol.Message) {
	r.mu.RLock()
	_, ok := r.workers[msg.WorkerID]
	r.mu.RUnlock()

	if !ok {
		r.log.Warn("worker not found", "worker_id", msg.WorkerID)
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		r.log.Warn("failed to marshal forward-to-worker frame", "worker_id", msg.WorkerID, "error", err)
		return
	}
	r.enqueueWorkerFrame(msg.WorkerID, data)
}

// broadcastToWorkers sends a message to all connected workers. Each frame is
// enqueued onto the per-worker outbound queue; only workerWriterLoop writes
// the sockets (issue #203).
func (r *Relay) broadcastToWorkers(msg protocol.Message) {
	r.mu.RLock()
	workerIDs := make([]string, 0, len(r.workers))
	for id := range r.workers {
		workerIDs = append(workerIDs, id)
	}
	r.mu.RUnlock()

	data, err := json.Marshal(msg)
	if err != nil {
		r.log.Warn("failed to marshal broadcast frame", "error", err)
		return
	}
	for _, id := range workerIDs {
		r.enqueueWorkerFrame(id, data)
	}
}

// enqueueWorkerFrame enqueues a pre-marshaled frame onto workerID's outbound
// queue for the single writer goroutine. The channel snapshot is obtained
// under r.mu alongside the socket, so a caller never delivers a frame into a
// channel whose writer has exited. It is the ONLY path for outbound worker
// frames: call through this function instead of writing to the worker socket
// directly (issue #203). On a full queue the oldest buffered frame is dropped
// (almost always a redundant heartbeat) and the frame is kept — the same
// pattern as bufferWorkerMessage.
func (r *Relay) enqueueWorkerFrame(workerID string, data []byte) {
	r.mu.Lock()
	sendCh, ok := r.workerSendCh[workerID]
	r.mu.Unlock()
	if !ok {
		// Worker disconnected between the caller's lookup and here; the
		// frame is lost with the socket, same as a write to a closed conn.
		r.log.Debug("dropping frame for disconnected worker", "worker_id", workerID)
		return
	}

	select {
	case sendCh <- data:
		// Delivered to the single writer goroutine.
	default:
		// Queue full: drop the oldest buffered frame (almost always a
		// redundant heartbeat) and enqueue this one, so the link keeps
		// making progress instead of wedging. Re-enqueue under the lock so
		// the writer cannot drain between the drop and the re-add.
		r.mu.Lock()
		sendCh2, ok := r.workerSendCh[workerID]
		if !ok {
			r.mu.Unlock()
			r.log.Debug("dropping frame for disconnected worker", "worker_id", workerID)
			return
		}
		select {
		case <-sendCh2:
			// Drained one oldest frame to make room.
		default:
			r.mu.Unlock()
			return
		}
		select {
		case sendCh2 <- data:
		default:
			// Still full after draining one: the writer is stalled; drop
			// this frame rather than block the read loop.
			r.log.Warn("worker send queue still full; dropping frame",
				"worker_id", workerID, "queued", len(sendCh2))
		}
		r.mu.Unlock()
	}
}

// workerWriterLoop is the ONLY goroutine that writes to a worker connection.
// It drains the per-connection workerSendCh and applies the per-frame write
// deadline; on write failure it closes the socket so the read loop exits and
// the worker is pruned (mirrors routerWriterLoop, issue #203).
//
// On exit it removes workerSendCh[workerID] under r.mu so enqueueWorkerFrame
// (which snapshots the channel under the same lock) can never deliver a
// frame into a channel whose writer has gone — the frame would otherwise sit
// buffered forever and be silently lost.
func (r *Relay) workerWriterLoop(ws *websocket.Conn, workerID string, sendCh chan []byte) {
	defer func() {
		r.mu.Lock()
		if r.workerSendCh[workerID] == sendCh {
			delete(r.workerSendCh, workerID)
		}
		r.mu.Unlock()
	}()

	for data := range sendCh {
		if err := wsutil.WriteTextWithTimeout(ws, data, r.writeTimeout); err != nil {
			r.log.Debug("failed to send to worker", "worker_id", workerID, "error", err)
			// On write failure close the socket so the read loop exits
			// and the worker is pruned (mirrors routerWriterLoop).
			_ = ws.Close()
			return
		}
	}
}

// bufferWorkerMessage appends msg to workerQueue, dropping the oldest
// buffered frame when the queue is at its cap. The cap is wsutil.SendQueueSize
// so the queue and the reconnect flush can never grow without limit during a
// prolonged router outage (issue #196 — the "flushed buffered messages
// count=1738" evidence). The oldest frame is almost always a redundant
// heartbeat; the log warning makes a dropped register visible. Returns true
// when the message was kept.
func (r *Relay) bufferWorkerMessage(msg protocol.Message) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.workerQueue) >= wsutil.SendQueueSize {
		dropped := r.workerQueue[0]
		r.workerQueue = r.workerQueue[1:]
		r.log.Warn("worker queue full; dropping oldest buffered message",
			"dropped_worker_id", dropped.WorkerID, "dropped_type", dropped.Type,
			"queued", len(r.workerQueue))
	}
	r.workerQueue = append(r.workerQueue, msg)
	return true
}

// routerConnected reports whether the relay currently has a live router
// connection. The gate for outbound frames: with no router, no register was
// ever forwarded, so there is no stale conn on the router to prune.
func (r *Relay) routerConnected() bool {
	r.routerMu.Lock()
	defer r.routerMu.Unlock()
	return r.routerConn != nil
}

// sendWorkerGone enqueues a worker_gone frame for the single writer loop.
// It is best-effort: if the router link is wedged the relay is already on
// its way to re-dialing, and the router's 120s sweep remains the safety
// net. The nonce lets the router match the signal to the conn that died.
func (r *Relay) sendWorkerGone(ctx context.Context, workerID, nonce string) error {
	msg, err := protocol.NewMessage(protocol.MsgWorkerGone, protocol.WorkerGonePayload{
		WorkerID: workerID, RelayNonce: nonce,
	})
	if err != nil {
		return fmt.Errorf("marshal worker_gone: %w", err)
	}
	// The router routes relay frames by WorkerID; stamp it so the signal
	// reaches the target conn directly instead of the broadcast path.
	msg.WorkerID = workerID
	return r.enqueueRouter(ctx, msg)
}

// enqueueRouter marshals msg and enqueues it onto the current routerSendCh
// for the single writer goroutine. The channel snapshot is obtained under
// routerMu so the caller never sends to a channel whose writer has exited.
// It is the ONLY path for outbound relay frames: call through this function
// instead of writing to routerConn directly (issue #196). Returns an error
// if the router is not connected, the queue is full, or the context is
// cancelled.
//
// The gate is routerConn != nil, NOT routerSendCh != nil: the channel is
// created lazily by handleRouter when a router connects, so a nil channel
// means "no router connected yet" — enqueuing into it would silently drop
// the frame (the buffered-register ordering bug, where a worker's register
// reached the relay before any router existed).
func (r *Relay) enqueueRouter(ctx context.Context, msg protocol.Message) error {
	// Gate on a live router connection: routerConn and routerSendCh are
	// published atomically by handleRouter under routerMu, so a non-nil
	// routerConn guarantees a live writer loop draining the snapshot.
	r.routerMu.Lock()
	conn := r.routerConn
	sendCh := r.routerSendCh
	r.routerMu.Unlock()

	if conn == nil {
		return errors.New("router not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal router frame: %w", err)
	}
	select {
	case sendCh <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Queue full: transient backpressure.
		return errQueueFull
	}
}

// sendToRouter forwards a worker message to the router via the single writer
// goroutine. On failure the message is re-buffered onto workerQueue for the
// next flush (issue #196). Returns nil on enqueue success, or an error if the
// router is not connected (the caller will buffer).
func (r *Relay) sendToRouter(ctx context.Context, msg protocol.Message) error {
	if err := r.enqueueRouter(ctx, msg); err != nil {
		return err
	}
	return nil
}

// sendQueueFull is returned when the outbound queue is full and the frame
// cannot be enqueued for the single writer goroutine.
var errQueueFull = errors.New("router send queue full")

// routerWriterLoop is the ONLY goroutine that writes to the router connection.
// It drains the per-connection sendCh and applies the per-frame write deadline;
// on write failure it closes the socket so the reader loop exits and
// runRelay re-dials (issue #195, #196).
//
// On exit it nils routerSendCh under routerMu so enqueueRouter (which gates on
// routerConn != nil) can never deliver a frame into a channel whose writer has
// gone — the frame would otherwise sit buffered forever and be silently lost.
func (r *Relay) routerWriterLoop(ws *websocket.Conn, sendCh chan []byte, done chan struct{}) {
	defer func() {
		r.routerMu.Lock()
		if r.routerSendCh == sendCh {
			r.routerSendCh = nil
		}
		r.routerMu.Unlock()
	}()

	for {
		select {
		case <-done:
			return
		case data, ok := <-sendCh:
			if !ok {
				return
			}
			if err := wsutil.WriteTextWithTimeout(ws, data, r.writeTimeout); err != nil {
				r.log.Warn("failed to send to router", "error", err)
				// On write failure close the connection so the router's
				// reader loop exits and runRelay re-dials (issue #195).
				_ = ws.Close()
				return
			}
		}
	}
}

// relayPingLoop sends protocol pings to the router at a fixed cadence so the
// router<->relay link never goes idle. Both sides apply a 90s idle read
// deadline, so without keepalive traffic the link is torn down and re-dialled
// every 90s. The router echoes each ping back; the echo is what resets the
// relay's own read deadline (the relay is blocked reading, so its outbound
// ping alone does not help). Pings enqueue onto routerSendCh — this loop is
// never a writer (issue #196).
func (r *Relay) relayPingLoop(done chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			msg, err := protocol.NewMessage(protocol.MsgPing, protocol.PingPayload{Timestamp: time.Now().Unix()})
			if err != nil {
				continue
			}
			// Pings are best-effort: if the writer is gone the reader
			// loop will exit and runRelay re-dials.
			_ = r.enqueueRouter(context.Background(), msg)
		}
	}
}

// flushBufferedMessages drains workerQueue and enqueues each frame onto the
// router's outbound queue for the single writer goroutine, instead of
// writing directly. Frames are bounded by the channel's cap; if the queue
// fills the flush stops and remaining frames stay buffered for the next
// flush (issue #196 — the "flushed buffered messages count=1738" evidence).
//
// The gate is a live router connection (routerConn != nil), not the channel
// itself: the channel is created lazily by handleRouter, so without this
// check a flush would enqueue into an orphaned channel when no router is
// connected and the buffered register would be silently lost.
func (r *Relay) flushBufferedMessages() {
	r.routerMu.Lock()
	conn := r.routerConn
	sendCh := r.routerSendCh
	r.routerMu.Unlock()

	if conn == nil {
		return
	}

	r.mu.Lock()
	if len(r.workerQueue) == 0 {
		r.mu.Unlock()
		return
	}
	workers := make([]protocol.Message, len(r.workerQueue))
	copy(workers, r.workerQueue)
	r.workerQueue = r.workerQueue[:0]
	r.mu.Unlock()

	flushed := 0
	for _, msg := range workers {
		data, err := json.Marshal(msg)
		if err != nil {
			r.log.Warn("failed to marshal buffered message", "worker_id", msg.WorkerID, "error", err)
			continue
		}
		select {
		case sendCh <- data:
			flushed++
		default:
			// Queue full: keep the rest buffered for the next flush.
			r.mu.Lock()
			r.workerQueue = append(r.workerQueue, workers[flushed:]...)
			r.mu.Unlock()
			r.log.Debug("router send queue full during flush; re-buffering", "remaining", len(workers)-flushed)
			return
		}
	}

	r.log.Info("flushed buffered messages", "count", flushed)
}
