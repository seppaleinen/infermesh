package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/wsutil"
	"golang.org/x/net/websocket"
)

// --- Flags ---

var (
	relayListen = flag.String("listen", ":8090", "address to listen on for worker and router connections")
	relayDev    = flag.Bool("dev-mode", false, "dev mode (no auth, loopback only)")
	relayProd   = flag.Bool("prod-mode", false, "production mode (mTLS required)")
)

// relayWorkerEntry holds a connected worker's WebSocket connection.
type relayWorkerEntry struct {
	conn *websocket.Conn
}

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
		log:         log,
		listenAddr:  *relayListen,
		workers:     make(map[string]*websocket.Conn),
		workerQueue: make([]protocol.Message, 0),
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

	r.mu.Lock()
	r.workers[workerID] = ws
	r.mu.Unlock()

	// Forward the register to the router so it knows this worker exists.
	msg.WorkerID = workerID

	r.routerMu.Lock()
	conn := r.routerConn
	r.routerMu.Unlock()

	if conn != nil {
		if err := r.sendToRouter(msg); err != nil {
			r.log.Warn("failed to forward worker register to router; buffering", "worker_id", workerID, "error", err)
			r.mu.Lock()
			r.workerQueue = append(r.workerQueue, msg)
			r.mu.Unlock()
		}
	} else {
		// Buffer the register for when the router connects
		r.mu.Lock()
		r.workerQueue = append(r.workerQueue, msg)
		r.mu.Unlock()
		r.log.Debug("buffered worker register (router disconnected)", "worker_id", workerID)
	}

	r.log.Info("worker connected", "worker_id", workerID)

	// If router is connected, flush buffered messages
	r.flushBufferedMessages()

	// Block until the connection closes (handler must not return or
	// net/http will close the WebSocket connection immediately).
	r.workerReadLoop(ws, workerID)
}

// handleRouter registers a router connection and starts relay loops.
func (r *Relay) handleRouter(ws *websocket.Conn) {
	r.routerMu.Lock()
	if r.routerConn != nil {
		r.log.Warn("router already connected, closing old connection")
		_ = r.routerConn.Close()
	}
	r.routerConn = ws
	r.routerMu.Unlock()

	r.log.Info("router connected")

	// Flush buffered messages for all workers when router reconnects
	r.flushBufferedMessages()

	// Block until the connection closes (handler must not return or
	// net/http will close the WebSocket connection immediately).
	r.routerReadLoop(ws)
}

// workerReadLoop reads messages from a worker and forwards them to the router.
func (r *Relay) workerReadLoop(ws *websocket.Conn, workerID string) {
	defer func() {
		r.mu.Lock()
		delete(r.workers, workerID)
		r.mu.Unlock()
		r.log.Info("worker disconnected", "worker_id", workerID)
	}()

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

		// Stamp WorkerID before forwarding to router or buffering
		msg.WorkerID = workerID

		// Forward to router or buffer if router is disconnected
		r.routerMu.Lock()
		conn := r.routerConn
		r.routerMu.Unlock()

		if conn != nil {
			if err := r.sendToRouter(msg); err != nil {
				r.log.Warn("failed to forward worker message to router", "worker_id", workerID, "error", err)
			}
		} else {
			// Buffer message for when router reconnects
			r.mu.Lock()
			r.workerQueue = append(r.workerQueue, msg)
			r.mu.Unlock()
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

// forwardToWorker sends a message to a specific worker.
func (r *Relay) forwardToWorker(msg protocol.Message) {
	r.mu.RLock()
	conn, ok := r.workers[msg.WorkerID]
	r.mu.RUnlock()

	if !ok {
		r.log.Warn("worker not found", "worker_id", msg.WorkerID)
		return
	}

	if err := wsutil.SendJSON(conn, msg); err != nil {
		r.log.Warn("failed to forward to worker", "worker_id", msg.WorkerID, "error", err)
	}
}

// broadcastToWorkers sends a message to all connected workers.
func (r *Relay) broadcastToWorkers(msg protocol.Message) {
	r.mu.RLock()
	workers := make([]*websocket.Conn, 0, len(r.workers))
	for _, conn := range r.workers {
		workers = append(workers, conn)
	}
	r.mu.RUnlock()

	for _, conn := range workers {
		if err := wsutil.SendJSON(conn, msg); err != nil {
			r.log.Warn("failed to broadcast to worker", "error", err)
		}
	}
}

// sendToRouter sends a message to the router connection.
func (r *Relay) sendToRouter(msg protocol.Message) error {
	r.routerMu.Lock()
	conn := r.routerConn
	r.routerMu.Unlock()

	if conn == nil {
		return errors.New("router not connected")
	}

	return wsutil.SendJSON(conn, msg)
}

// flushBufferedMessages sends all buffered messages when router reconnects.
func (r *Relay) flushBufferedMessages() {
	r.routerMu.Lock()
	conn := r.routerConn
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

	// Send all buffered messages
	for _, msg := range workers {
		if err := wsutil.SendJSON(conn, msg); err != nil {
			r.log.Warn("failed to flush buffered message", "worker_id", msg.WorkerID, "error", err)
		}
	}

	r.log.Info("flushed buffered messages", "count", len(workers))
}
