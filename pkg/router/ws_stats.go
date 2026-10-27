package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/internal/wsutil"
)

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
		return c.workerInFlight() >= int64(h.maxInFlightFor(workerID))
	}
	return h.relayInFlightAt(workerID) >= int64(h.maxInFlightFor(workerID))
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
	if h.relayInFlight[workerID] == 0 {
		delete(h.relayInFlight, workerID)
	}
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
			MaxInFlight:      int64(h.maxInFlightFor(c.id)),
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
func (h *WSHub) cleanupRelayMetrics() {
	h.relayPendingMu.Lock()
	defer h.relayPendingMu.Unlock()
	// relayInFlight: delete zero entries (though decrementRelayInFlight should already handle this)
	for workerID, count := range h.relayInFlight {
		if count == 0 {
			delete(h.relayInFlight, workerID)
		}
	}
	// relayRejected429 and relayRejected504: delete zero entries (though these are cumulative)
	for workerID, count := range h.relayRejected429 {
		if count == 0 {
			delete(h.relayRejected429, workerID)
		}
	}
	for workerID, count := range h.relayRejected504 {
		if count == 0 {
			delete(h.relayRejected504, workerID)
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
