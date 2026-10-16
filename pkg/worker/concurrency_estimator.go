package worker

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

const (
	defaultInitCap         = 4
	minCap                 = 1
	windowDuration         = 10 * time.Second
	persistInterval        = 30 * time.Second
	latencySpikeMultiplier = 2.0
	http429Threshold       = 2
)

type ConcurrencyEstimator struct {
	mu              sync.Mutex
	cap             int
	window429       []time.Time
	latencies       []time.Duration
	baselineLatency time.Duration
	lastCapChanged  time.Time
	changedCh       chan struct{}
	persistPath     string
	persistTimer    *time.Timer
}

func NewConcurrencyEstimator(persistPath string) *ConcurrencyEstimator {
	e := &ConcurrencyEstimator{
		cap:         defaultInitCap,
		window429:   make([]time.Time, 0),
		latencies:   make([]time.Duration, 0),
		changedCh:   make(chan struct{}, 1),
		persistPath: persistPath,
	}
	if c := e.LoadPersisted(persistPath); c > 0 {
		if c < minCap {
			c = minCap
		}
		e.cap = c
	}
	return e
}

func (e *ConcurrencyEstimator) RecordRequest(start time.Time, statusCode int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	lat := now.Sub(start)
	e.latencies = append(e.latencies, lat)
	// update baseline on first samples or moving average
	if e.baselineLatency == 0 || len(e.latencies) < 10 {
		// initial baseline as recent p50 approximation
		e.baselineLatency = lat
	} else {
		// simple exponential moving average
		e.baselineLatency = time.Duration(float64(e.baselineLatency)*0.9 + float64(lat)*0.1)
	}
	if statusCode == 429 {
		e.window429 = append(e.window429, now)
	}
	// trigger persist debounce
	e.schedulePersist()
}

func (e *ConcurrencyEstimator) RecordCircuitOpen() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cap != 1 {
		e.cap = 1
		e.lastCapChanged = time.Now()
		e.notifyChanged()
		e.schedulePersist()
	}
}

func (e *ConcurrencyEstimator) RecordCircuitClosed() {
	e.mu.Lock()
	defer e.mu.Unlock()
	// re-increase to previous value or init if first
	if e.cap == 1 {
		// we don't know previous, go to init
		e.cap = defaultInitCap
		e.lastCapChanged = time.Now()
		e.notifyChanged()
		e.schedulePersist()
	}
}

func (e *ConcurrencyEstimator) Cap() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cap < minCap {
		return minCap
	}
	return e.cap
}

func (e *ConcurrencyEstimator) Start(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Stop()
			return
		case <-ticker.C:
			e.recompute()
		}
	}
}

func (e *ConcurrencyEstimator) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persist()
}

func (e *ConcurrencyEstimator) recompute() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	// prune old entries
	cutoff := now.Add(-windowDuration)
	// 429 window
	new429 := e.window429[:0]
	for _, t := range e.window429 {
		if t.After(cutoff) {
			new429 = append(new429, t)
		}
	}
	e.window429 = new429
	// latencies window — no per-sample timestamp, so prune by count only
	// (keep the last 200 samples ≈ 10s at ~1 request per 5s).
	if len(e.latencies) > 200 {
		e.latencies = e.latencies[len(e.latencies)-200:]
	}

	// Recompute cap
	originalCap := e.cap
	// If circuit open? Circuit state handled via RecordCircuitOpen, so skip here.

	// count 429
	if len(e.window429) >= http429Threshold {
		if e.cap > minCap {
			e.cap--
			e.lastCapChanged = now
		}
	}
	// latency spike p95
	if len(e.latencies) >= 5 && e.baselineLatency > 0 {
		p95 := e.percentile(e.latencies, 0.95)
		if p95 > time.Duration(float64(e.baselineLatency)*latencySpikeMultiplier) {
			if e.cap > minCap {
				e.cap--
				e.lastCapChanged = now
			}
		}
	}
	// slow recovery
	if e.cap < defaultInitCap && now.Sub(e.lastCapChanged) > 60*time.Second {
		// check no 429s in last 30s
		hasRecent429 := false
		recentCutoff := now.Add(-30 * time.Second)
		for _, t := range e.window429 {
			if t.After(recentCutoff) {
				hasRecent429 = true
				break
			}
		}
		if !hasRecent429 {
			e.cap++
			if e.cap > defaultInitCap {
				e.cap = defaultInitCap
			}
			e.lastCapChanged = now
		}
	}
	if e.cap != originalCap {
		e.notifyChanged()
		e.schedulePersist()
	}
}

func (e *ConcurrencyEstimator) percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	// simple sort copy
	tmp := make([]time.Duration, len(d))
	copy(tmp, d)
	// insertion sort for small slices
	for i := 1; i < len(tmp); i++ {
		j := i
		for j > 0 && tmp[j-1] > tmp[j] {
			tmp[j-1], tmp[j] = tmp[j], tmp[j-1]
			j--
		}
	}
	idx := int(float64(len(tmp)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return tmp[idx]
}

func (e *ConcurrencyEstimator) notifyChanged() {
	select {
	case e.changedCh <- struct{}{}:
	default:
	}
}

func (e *ConcurrencyEstimator) schedulePersist() {
	if e.persistTimer != nil {
		e.persistTimer.Stop()
	}
	e.persistTimer = time.AfterFunc(persistInterval, func() {
		e.mu.Lock()
		e.persist()
		e.mu.Unlock()
	})
}

func (e *ConcurrencyEstimator) persist() {
	if e.persistPath == "" {
		return
	}
	data := map[string]int{"concurrency_cap": e.cap}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	tmpPath := e.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmpPath, e.persistPath)
}

func (e *ConcurrencyEstimator) LoadPersisted(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var data struct {
		ConcurrencyCap int `json:"concurrency_cap"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return 0
	}
	return data.ConcurrencyCap
}
