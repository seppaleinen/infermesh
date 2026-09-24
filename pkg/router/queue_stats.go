package router

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

// QueueStatsCapacity is the fixed-size reservoir used to estimate queue-wait
// percentiles without unbounded memory growth.
const QueueStatsCapacity = 1024

// WorkerQueueStats is the per-worker half of a /v1/queue/stats response.
type WorkerQueueStats struct {
	WorkerID         string  `json:"worker_id"`
	QueueDepth       int64   `json:"queue_depth"`
	InFlight         int64   `json:"in_flight"`
	AvgWaitMs        float64 `json:"avg_wait_ms"`
	Rejected429Total int64   `json:"rejected_429_total"`
	Rejected504Total int64   `json:"rejected_504_total"`
}

// PoolQueueStats is the pool-level half of a /v1/queue/stats response.
type PoolQueueStats struct {
	TotalQueued      int64   `json:"total_queued"`
	TotalInFlight    int64   `json:"total_in_flight"`
	TotalRejected429 int64   `json:"total_rejected_429"`
	TotalRejected504 int64   `json:"total_rejected_504"`
	QueueTimeP50Ms   float64 `json:"queue_time_p50_ms"`
	QueueTimeP95Ms   float64 `json:"queue_time_p95_ms"`
	QueueTimeP99Ms   float64 `json:"queue_time_p99_ms"`
}

// QueueStatsResponse is the response from GET /v1/queue/stats.
type QueueStatsResponse struct {
	Workers []WorkerQueueStats `json:"workers"`
	Pool    PoolQueueStats     `json:"pool"`
}

// reservoir samples queue-wait durations with fixed-size reservoir sampling
// so percentile estimates stay bounded regardless of call volume.
type reservoir struct {
	mu      sync.Mutex
	samples [QueueStatsCapacity]time.Duration
	count   int64 // total samples seen (valid entries = min(count, cap))
	rng     *rand.Rand
}

func newReservoir() *reservoir {
	return &reservoir{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// record adds a sample using reservoir sampling.
func (r *reservoir) record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.count
	r.count++
	if i < QueueStatsCapacity {
		r.samples[i] = d
		return
	}
	// Replace an existing slot with probability cap/i.
	j := r.rng.Int63n(i + 1)
	if j < QueueStatsCapacity {
		r.samples[j] = d
	}
}

// snapshot returns a copy of the currently-valid samples.
func (r *reservoir) snapshot() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := int64(QueueStatsCapacity)
	if r.count < n {
		n = r.count
	}
	out := make([]time.Duration, n)
	copy(out, r.samples[:n])
	return out
}

// percentiles returns the 50th, 95th and 99th percentiles of the sampled
// queue-wait durations using nearest-rank estimation.
func (r *reservoir) percentiles() (p50, p95, p99 time.Duration) {
	samples := r.snapshot()
	if len(samples) == 0 {
		return 0, 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	n := len(samples)
	p50 = samples[idx(n, 50)]
	p95 = samples[idx(n, 95)]
	p99 = samples[idx(n, 99)]
	return
}

// idx returns the nearest-rank index for percentile p of n sorted samples.
func idx(n, p int) int {
	if n <= 0 {
		return 0
	}
	i := n * p / 100
	if i >= n {
		i = n - 1
	}
	return i
}

// queueMetrics holds pool-level queue observability for the router hub.
type queueMetrics struct {
	res *reservoir
}

func newQueueMetrics() *queueMetrics {
	return &queueMetrics{res: newReservoir()}
}

// recordWait feeds a queue-wait sample into the pool reservoir.
func (q *queueMetrics) recordWait(d time.Duration) {
	if q != nil {
		q.res.record(d)
	}
}

// percentiles delegates to the reservoir for the /v1/queue/stats endpoint.
func (q *queueMetrics) percentiles() (p50, p95, p99 time.Duration) {
	if q == nil {
		return 0, 0, 0
	}
	return q.res.percentiles()
}

// floatMs converts a duration to milliseconds as a float64.
func floatMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
