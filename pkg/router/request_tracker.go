package router

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Request statuses reported by GET /meta/requests/inflight (issue #156).
const (
	StatusInflight  = "in-flight"
	StatusStreaming = "streaming"
	StatusDone      = "done"
	StatusError     = "error"
)

// defaultMaxCompletedRequests bounds the finished-request ring buffer.
// defaultCompletedRequestTTL reaps finished entries older than this so the
// buffer cannot grow without bound even if the ring cap is large.
const (
	defaultMaxCompletedRequests = 200
	defaultCompletedRequestTTL  = 10 * time.Minute
)

// requestIDSeq is a process-wide counter used to mint unique request IDs.
// It starts from a random seed so two router restarts in the same nanosecond
// cannot collide on the wire (a client could otherwise correlate traces).
var requestIDSeq atomic.Int64

func nextRequestID() string {
	n := requestIDSeq.Add(1)
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), n)
}

// RequestRecord is a single inference request tracked by the router for
// visibility (issue #156). It is the wire shape of GET /meta/requests/inflight.
type RequestRecord struct {
	RequestID  string    `json:"request_id"`
	Model      string    `json:"model"`
	WorkerID   string    `json:"worker_id"`
	Kind       string    `json:"kind"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Status     string    `json:"status"`
	DurationMs int64     `json:"duration_ms,omitempty"`
}

// RequestTracker records every dispatched inference request: the model and
// worker it was sent to, when it started, and its final status. In-flight
// requests live in an active map; finished requests move to a bounded ring
// buffer so the last N completions are visible alongside live calls.
//
// It is in-memory and stdlib-only (mirrors CallCounter). No persistence: a
// router restart clears the trace, which is acceptable for a LAN dashboard.
type RequestTracker struct {
	mu           sync.Mutex
	active       map[string]*RequestRecord
	completed    []RequestRecord
	maxCompleted int
	completedTTL time.Duration
	pruneCh      chan struct{}
}

// NewRequestTracker creates a tracker. maxCompleted <= 0 and completedTTL <= 0
// fall back to the defaults so a zero-value construction is safe.
func NewRequestTracker(maxCompleted int, completedTTL time.Duration) *RequestTracker {
	if maxCompleted <= 0 {
		maxCompleted = defaultMaxCompletedRequests
	}
	if completedTTL <= 0 {
		completedTTL = defaultCompletedRequestTTL
	}
	return &RequestTracker{
		active:       make(map[string]*RequestRecord),
		maxCompleted: maxCompleted,
		completedTTL: completedTTL,
		pruneCh:      make(chan struct{}),
	}
}

// Begin records the dispatch of a new inference request and returns its ID.
// The record stays "in-flight" until Finish or SetStatus is called.
// (Named Begin rather than Start to avoid colliding with the Start(ctx)
// lifecycle method that mirrors CallCounter.)
func (t *RequestTracker) Begin(model, workerID, kind string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := nextRequestID()
	t.active[id] = &RequestRecord{
		RequestID: id,
		Model:     model,
		WorkerID:  workerID,
		Kind:      kind,
		StartedAt: time.Now(),
		Status:    StatusInflight,
	}
	return id
}

// SetStatus updates the status of an in-flight request (e.g. in-flight →
// streaming when the first chunk arrives). Unknown IDs are ignored.
func (t *RequestTracker) SetStatus(id, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.active[id]; ok {
		r.Status = status
	}
}

// Finish marks a request as completed with the given status ("done" or
// "error"), moving it from the active map into the bounded completed buffer.
// Unknown IDs are ignored.
func (t *RequestTracker) Finish(id, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.active[id]
	if !ok {
		return
	}
	delete(t.active, id)
	now := time.Now()
	r.FinishedAt = now
	r.Status = status
	r.DurationMs = now.Sub(r.StartedAt).Milliseconds()
	// Prepend so the buffer is newest-first.
	t.completed = append([]RequestRecord{*r}, t.completed...)
	t.pruneLocked(now)
}

// Snapshot returns a copy of the current in-flight and recent-completed
// records, newest-first, with expired completed records pruned. Safe to call
// concurrently.
func (t *RequestTracker) Snapshot() []RequestRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())
	out := make([]RequestRecord, 0, len(t.active)+len(t.completed))
	for _, r := range t.active {
		out = append(out, *r)
	}
	out = append(out, t.completed...)
	// Newest-first with a deterministic tie-break on RequestID so repeated
	// polls do not shuffle equal-timestamped records.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartedAt.Equal(b.StartedAt) {
			return a.RequestID < b.RequestID
		}
		return a.StartedAt.After(b.StartedAt)
	})
	return out
}

// pruneLocked caps the completed buffer and reaps entries older than the TTL.
// The completed slice is newest-first, so the tail is the oldest.
func (t *RequestTracker) pruneLocked(now time.Time) {
	if len(t.completed) > t.maxCompleted {
		t.completed = t.completed[:t.maxCompleted]
	}
	threshold := now.Add(-t.completedTTL)
	cut := len(t.completed)
	for cut > 0 && t.completed[cut-1].FinishedAt.Before(threshold) {
		cut--
	}
	if cut < len(t.completed) {
		t.completed = t.completed[:cut]
	}
}

// Start launches the periodic prune goroutine. Mirrors CallCounter.Start.
func (t *RequestTracker) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(t.completedTTL / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.mu.Lock()
				t.pruneLocked(time.Now())
				t.mu.Unlock()
			case <-ctx.Done():
				return
			case <-t.pruneCh:
				t.mu.Lock()
				t.pruneLocked(time.Now())
				t.mu.Unlock()
			}
		}
	}()
}

// Stop signals the prune goroutine to exit. Mirrors CallCounter.Stop.
func (t *RequestTracker) Stop() {
	close(t.pruneCh)
}