package worker

import (
	"sort"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// DefaultInferenceWindow is the default rolling window for inference
// throughput stats. A 10-minute window matches the router's popular-models
// counter window (issue #134) and smooths out bursty short requests.
const DefaultInferenceWindow = 10 * time.Minute

// inferenceSample is a single completed inference call for one model.
type inferenceSample struct {
	model              string
	promptTokens       int
	completionTokens   int
	ttft               time.Duration
	total              time.Duration
	when               time.Time
}

// InferenceTracker records completed inference calls per model over a
// rolling window and aggregates them into per-model throughput snapshots
// (tokens/sec, average TTFT). It is thread-safe: handlers Record on the
// request goroutine, and handleCapabilities reads Snapshots concurrently.
type InferenceTracker struct {
	mu      sync.Mutex
	window  time.Duration
	samples []inferenceSample // oldest first (append order; pruned from the front)
}

// NewInferenceTracker creates a tracker with the given rolling window.
// A window <= 0 falls back to DefaultInferenceWindow (10 minutes).
func NewInferenceTracker(window time.Duration) *InferenceTracker {
	if window <= 0 {
		window = DefaultInferenceWindow
	}
	return &InferenceTracker{window: window}
}

// Record records a completed inference call for the given model. Negative
// token counts are clamped to zero; non-positive durations are recorded as-is
// (they only appear when a test or a degenerate backend produced them).
func (t *InferenceTracker) Record(model string, promptTokens, completionTokens int, ttft, total time.Duration) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.samples = append(t.samples, inferenceSample{
		model:            model,
		promptTokens:     promptTokens,
		completionTokens: completionTokens,
		ttft:             ttft,
		total:            total,
		when:             now,
	})
	t.pruneLocked(now)
}

// Snapshot returns the aggregated stats for one model. The ok flag is false
// when the model has no samples in the current window.
func (t *InferenceTracker) Snapshot(model string) (protocol.InferenceStatsSnapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())

	for _, s := range t.samples {
		if s.model == model {
			return aggregate(t.samples, model), true
		}
	}
	return protocol.InferenceStatsSnapshot{}, false
}

// Snapshots returns one snapshot per model that has samples in the current
// window, sorted by model name for deterministic /capabilities payloads.
// The result is always non-nil so JSON encodes [] rather than null.
func (t *InferenceTracker) Snapshots() []protocol.InferenceStatsSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())

	models := make(map[string]struct{}, len(t.samples))
	for _, s := range t.samples {
		models[s.model] = struct{}{}
	}
	out := make([]protocol.InferenceStatsSnapshot, 0, len(models))
	for model := range models {
		out = append(out, aggregate(t.samples, model))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Prune drops samples older than the rolling window. It is called lazily by
// Record/Snapshot/Snapshots, so no background goroutine is required; the
// tracker is fully passive and safe to drop at any time.
func (t *InferenceTracker) Prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())
}

// pruneLocked removes samples older than the window. Caller holds t.mu.
func (t *InferenceTracker) pruneLocked(now time.Time) {
	threshold := now.Add(-t.window)
	i := 0
	for i < len(t.samples) && t.samples[i].when.Before(threshold) {
		i++
	}
	if i > 0 {
		t.samples = t.samples[i:]
	}
}

// aggregate computes the per-model rolling stats from the (already pruned)
// sample list. It counts only samples matching model.
func aggregate(samples []inferenceSample, model string) protocol.InferenceStatsSnapshot {
	var (
		promptTotal     int
		completionTotal int
		ttftSum         time.Duration
		n               int
	)
	for _, s := range samples {
		if s.model != model {
			continue
		}
		promptTotal += s.promptTokens
		completionTotal += s.completionTokens
		ttftSum += s.ttft
		n++
	}
	snap := protocol.InferenceStatsSnapshot{Model: model, Samples: n}
	if n == 0 {
		return snap
	}
	avgTTFT := float64(ttftSum) / float64(n) / float64(time.Millisecond)
	snap.AvgTTFTMs = avgTTFT
	snap.AvgPromptTokens = float64(promptTotal) / float64(n)
	snap.AvgCompletionTokens = float64(completionTotal) / float64(n)

	// Tokens per second: total completion tokens divided by the total
	// (wall-clock) generation time across all samples. Uses completion
	// tokens only — prompt tokens are pre-generated and do not reflect
	// decode throughput. Samples with zero total time are skipped so a
	// degenerate 0-duration call does not produce +Inf.
	var totalTokens int
	var totalTime time.Duration
	for _, s := range samples {
		if s.model != model {
			continue
		}
		if s.total <= 0 {
			continue
		}
		totalTokens += s.completionTokens
		totalTime += s.total
	}
	if totalTime > 0 && totalTokens > 0 {
		snap.TokensPerSecond = float64(totalTokens) / totalTime.Seconds()
	}
	return snap
}
