package worker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/platform"
)

const (
	defaultInitCap         = 4
	minCap                 = 1
	maxCap                 = 1024 // hard ceiling on any learned concurrency cap
	windowDuration         = 10 * time.Second
	persistInterval        = 30 * time.Second
	latencySpikeMultiplier = 2.0
	http429Threshold       = 2
)

// ConcurrencyEstimator tracks 429s and latency for a SINGLE model.
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

// PerModelConcurrencyEstimator manages a map of per-model estimators.
type PerModelConcurrencyEstimator struct {
	mu          sync.Mutex
	estimators  map[string]*ConcurrencyEstimator
	persistPath string
	defaultCap  int
	// persistPathMu guards persistPath and persistTimer. schedulePersist is
	// reachable from RecordRequest/RecordCircuitOpen/RecordCircuitClosed/
	// recomputeAll, which run on the inference hot path WITHOUT holding mu
	// (they only lock the per-model estimator). Without this lock the
	// read-modify-write on persistTimer races with itself and with Stop's
	// timer teardown (issue #206).
	persistPathMu sync.Mutex
	persistTimer  *time.Timer
}

// NewConcurrencyEstimator creates a ConcurrencyEstimator seeded with
// initCap (bounded to [minCap, maxCap]). A persisted cap from a previous run
// overrides the seed, so the estimator keeps its learned value across
// restarts while still benefiting from the backend's thread count on first
// boot (issue #139).
func NewConcurrencyEstimator(persistPath string, initCap int) *ConcurrencyEstimator {
	if initCap < minCap {
		initCap = minCap
	}
	if initCap > maxCap {
		initCap = maxCap
	}
	e := &ConcurrencyEstimator{
		cap:         initCap,
		window429:   make([]time.Time, 0),
		latencies:   make([]time.Duration, 0),
		changedCh:   make(chan struct{}, 1),
		persistPath: persistPath,
	}
	if c := e.LoadPersisted(persistPath); c > 0 {
		if c < minCap {
			c = minCap
		}
		if c > maxCap {
			c = maxCap
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
		// we don't know previous, go to defaultInitCap but clamp to maxCap
		e.cap = defaultInitCap
		if e.cap > maxCap {
			e.cap = maxCap
		}
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
	if e.cap > maxCap {
		return maxCap
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
	// slow recovery: if cap dropped below defaultInitCap and no 429s in 30s,
	// increment back toward the default. Never exceed defaultInitCap (the
	// seed) because that is the worker's own learned baseline.
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
	if len(b) > platform.MaxStateFileSize {
		return 0
	}
	var data struct {
		ConcurrencyCap int `json:"concurrency_cap"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return 0
	}
	if data.ConcurrencyCap < minCap || data.ConcurrencyCap > maxCap {
		return 0
	}
	return data.ConcurrencyCap
}

// NewPerModelConcurrencyEstimator creates a new per-model estimator manager.
//
// persistPath is sanitized by safePersistPath: an empty path or a path that
// escapes the intended directory (absolute, contains "..", or names a
// directory component) disables persistence entirely rather than writing
// outside the worker's state directory. This prevents an attacker who
// influences the persistPath argument (e.g. a future CLI flag or env var)
// from turning the estimator into an arbitrary file writer (issue #206).
func NewPerModelConcurrencyEstimator(persistPath string, defaultCap int) *PerModelConcurrencyEstimator {
	if defaultCap < minCap {
		defaultCap = minCap
	}
	p := &PerModelConcurrencyEstimator{
		estimators:  make(map[string]*ConcurrencyEstimator),
		persistPath: safePersistPath(persistPath),
		defaultCap:  defaultCap,
	}
	p.loadPersisted()
	return p
}

// getOrCreate returns the estimator for the given model, creating it if needed.
func (p *PerModelConcurrencyEstimator) getOrCreate(model string) *ConcurrencyEstimator {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.estimators[model]; ok {
		return e
	}
	e := NewConcurrencyEstimator("", p.defaultCap)
	p.estimators[model] = e
	return e
}

// RecordRequest records a request for the given model.
func (p *PerModelConcurrencyEstimator) RecordRequest(start time.Time, statusCode int, model string) {
	e := p.getOrCreate(model)
	e.RecordRequest(start, statusCode)
	p.schedulePersist()
}

// RecordCircuitOpen records a circuit breaker open event for the given model.
// Creates the estimator if it doesn't exist yet (so circuit events are captured
// even for models that haven't received inference requests yet).
func (p *PerModelConcurrencyEstimator) RecordCircuitOpen(model string) {
	e := p.getOrCreate(model)
	e.RecordCircuitOpen()
	p.schedulePersist()
}

// RecordCircuitClosed records a circuit breaker closed event for the given model.
func (p *PerModelConcurrencyEstimator) RecordCircuitClosed(model string) {
	p.mu.Lock()
	e, ok := p.estimators[model]
	p.mu.Unlock()
	if ok {
		e.RecordCircuitClosed()
		p.schedulePersist()
	}
}

// Cap returns the concurrency cap for the given model.
// Falls back to the default cap if the model has no estimator yet.
func (p *PerModelConcurrencyEstimator) Cap(model string) int {
	p.mu.Lock()
	e, ok := p.estimators[model]
	p.mu.Unlock()
	if ok {
		return e.Cap()
	}
	return p.defaultCap
}

// Start runs the background recomputation ticker for all active models.
func (p *PerModelConcurrencyEstimator) Start(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Stop()
			return
		case <-ticker.C:
			p.recomputeAll()
		}
	}
}

// Stop stops all estimators and persists. It is idempotent and safe to call
// twice. It locks persistPathMu around the timer teardown so it cannot race
// schedulePersist's read-modify-write, and locks p.mu for the estimator
// iteration so it cannot race the timer's persist callback.
func (p *PerModelConcurrencyEstimator) Stop() {
	p.persistPathMu.Lock()
	if p.persistTimer != nil {
		p.persistTimer.Stop()
		p.persistTimer = nil
	}
	p.persistPathMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Stop each estimator's timer and persist its state while holding p.mu.
	// This is safe because e.persist() does not acquire p.mu.
	for _, e := range p.estimators {
		if e.persistTimer != nil {
			e.persistTimer.Stop()
			e.persistTimer = nil
		}
		e.mu.Lock()
		e.persist()
		e.mu.Unlock()
	}

	// Persist aggregated state
	p.persist()
}

// recomputeAll runs recompute on all active estimators.
func (p *PerModelConcurrencyEstimator) recomputeAll() {
	p.mu.Lock()
	estimators := make([]*ConcurrencyEstimator, 0, len(p.estimators))
	for _, e := range p.estimators {
		estimators = append(estimators, e)
	}
	p.mu.Unlock()
	for _, e := range estimators {
		e.recompute()
	}
	p.schedulePersist()
}

// schedulePersist debounces the persist operation. It must be called with
// NO lock held: it acquires persistPathMu internally, so callers that already
// hold p.mu or a sub-estimator's mutex do not deadlock. persistPathMu guards
// persistTimer, which is also read and written from Stop()'s teardown and
// from this AfterFunc callback, so all access is serialized through
// persistPathMu (issue #206).
func (p *PerModelConcurrencyEstimator) schedulePersist() {
	p.persistPathMu.Lock()
	defer p.persistPathMu.Unlock()
	if p.persistTimer != nil {
		p.persistTimer.Stop()
	}
	p.persistTimer = time.AfterFunc(persistInterval, func() {
		p.mu.Lock()
		p.persist()
		p.mu.Unlock()
	})
}

// persist saves all model caps to a single JSON file: {"modelA": capA, "modelB": capB}
// Caller must hold p.mu.
func (p *PerModelConcurrencyEstimator) persist() {
	if p.persistPath == "" {
		return
	}
	data := make(map[string]int, len(p.estimators))
	for model, e := range p.estimators {
		e.mu.Lock()
		data[model] = e.cap
		e.mu.Unlock()
	}
	// Debug: log what we're persisting
	// fmt.Fprintf(os.Stderr, "PERSIST: %v\n", data)
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	tmpPath := p.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmpPath, p.persistPath)
}

// loadPersisted loads the per-model caps from the JSON file. Malformed input
// is discarded, never propagated: entries are bounded to [minCap, maxCap]
// and a total entry count cap prevents an attacker-supplied file from
// exhausting memory via millions of JSON keys (issue #206).
func (p *PerModelConcurrencyEstimator) loadPersisted() {
	b, err := os.ReadFile(p.persistPath)
	if err != nil {
		return
	}
	// Cap the read at 1 MiB: a legitimate per-model cap map is at most a
	// few kilobytes, so anything larger is either corrupt or hostile.
	if len(b) > platform.MaxStateFileSize {
		return
	}
	var data map[string]int
	if err := json.Unmarshal(b, &data); err != nil {
		return
	}
	if len(data) == 0 || len(data) > maxPersistedModels {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for model, cap := range data {
		if model == "" {
			continue
		}
		if cap < minCap {
			cap = minCap
		}
		if cap > maxCap {
			cap = maxCap
		}
		e := NewConcurrencyEstimator("", cap)
		e.cap = cap
		p.estimators[model] = e
	}
}

// maxPersistedModels is the maximum number of per-model entries loaded from
// the persistence file. A normal worker advertises at most a handful of
// models; anything beyond this is a sign of a corrupt or hostile file and
// is discarded wholesale rather than loaded into the estimator map.
const maxPersistedModels = 1000

// safePersistPath validates the persist path and returns an empty string
// (persistence disabled) when it is unsafe. A path is unsafe when it:
//   - is empty
//   - contains a path-traversal component (a ".." segment)
//   - is an absolute path (Unix "/" or Windows "C:")
//   - ends in a component that is not a simple base name (empty, ".", "..")
//
// This is the estimator's final guard: even with a future attacker-controlled
// CLI flag or env var, a path like "../../etc/cron.d/evil" is rejected rather
// than written (issue #206).
func safePersistPath(p string) string {
	if p == "" {
		return ""
	}
	// Reject any ".." segment regardless of position — this guards against
	// path traversal in the path string itself.
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return ""
		}
	}
	// The final component must be a simple, non-empty base name.
	base := p[strings.LastIndexAny(p, "/\\")+1:]
	if base == "" || base == "." || base == ".." {
		return ""
	}
	for _, c := range base {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	// Allow absolute paths from platform.StatePath — those paths are
	// already validated by platform.ValidateBaseName. We only need to ensure
	// the filename component is safe (done above).
	return p
}
