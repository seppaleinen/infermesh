package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// ScoringAlgorithm defines the interface for scoring workers based on a model request.
type ScoringAlgorithm interface {
	// Name returns the name of the algorithm.
	Name() string
	// Score returns a score for the given worker and model request.
	// Higher score is better. Returns error if worker cannot be scored.
	Score(ctx context.Context, request ModelRequest, worker WorkerInfo) (float64, error)
	// GetUnavailableTTL returns the maximum time since last heartbeat before a worker is considered unavailable.
	GetUnavailableTTL() time.Duration
}

// ModelRequest represents a request for a model from the router.
type ModelRequest struct {
	Model         string // e.g., "llama3-8b"
	Quantization  string // e.g., "Q4", "Q8", "" (any)
	MaxVRAMMB     int    // Optional VRAM constraint in MB
	MinVRAMMB     int    // Optional VRAM constraint in MB
}

// WorkerInfo represents a worker with the fields needed for scoring.
// This is a subset of protocol.WorkerInfo to avoid coupling.
type WorkerInfo struct {
	ID           string
	Addr         string
	Models       []protocol.ModelInfo
	VRAMTotalMB  int
	VRAMFreeMB   int
	GPUUtilPct   float64 // 0-100
	QueueDepth   int
	AvgLatencyMS float64 // Moving average
	LastHeartbeat time.Time
}

// SelectedWorker represents the chosen worker along with its score.
type SelectedWorker struct {
	WorkerInfo
	Score       float64
	Reason      string // e.g., "best score: 0.87"
}

// Errors
var (
	ErrNoMatchingWorkers   = errors.New("no worker has the requested model")
	ErrAllWorkersUnavailable = errors.New("workers found but all are unhealthy")
	ErrNoCapacity          = errors.New("workers have model but insufficient VRAM")
	ErrWorkerUnavailable   = errors.New("worker is unavailable or stale")
)

// defaultUnavailableTTL is how long since last heartbeat before a worker is considered unavailable.
const defaultUnavailableTTL = 30 * time.Second



// WeightedScorer implements a deterministic weighted scoring algorithm.
// Formula: score = w1*quantMatch + w2*vramFreeRatio - w3*gpuUtil - w4*queueDepth - w5*latencyNorm
type WeightedScorer struct {
	// Weights for the scoring formula (must sum to 1.0)
	QuantMatchWeight float64 // weight for exact quantization match bonus
	VRAMFreeWeight   float64 // weight for free VRAM ratio
	GPUUtilWeight    float64 // weight for GPU utilization (inverted)
	QueueDepthWeight float64 // weight for queue depth (inverted)
	LatencyWeight    float64 // weight for latency (inverted)
	// unavailableTTL is how long since last heartbeat before worker is unavailable
	unavailableTTL time.Duration

	// maxQueueDepth normalizes the queue-depth term. A worker with
	// QueueDepth >= maxQueueDepth scores 0.0 for the queue component.
	// Default 10. Configurable via --scorer-max-queue-depth.
	maxQueueDepth int
}

// NewWeightedScorer creates a new WeightedScorer with default weights.
	// Default weights: QuantMatch=0.40, VRAMFree=0.25, GPUUtil=0.15, QueueDepth=0.10, Latency=0.10
	// Default maxQueueDepth: 10.
	func NewWeightedScorer() *WeightedScorer {
		return &WeightedScorer{
			QuantMatchWeight: 0.40,
			VRAMFreeWeight:   0.25,
			GPUUtilWeight:    0.15,
			QueueDepthWeight: 0.10,
			LatencyWeight:    0.10,
			unavailableTTL:   defaultUnavailableTTL,
			maxQueueDepth:    10,
		}
	}

	// NewWeightedScorerWithConfig creates a new WeightedScorer with custom weights.
	// Weights should sum to 1.0 for predictable scoring, but this is not enforced.
	// maxQueueDepth normalizes the queue-depth term: a worker with
	// QueueDepth >= maxQueueDepth scores 0.0 for the queue component.
	// A value <= 0 falls back to the default of 10.
	func NewWeightedScorerWithConfig(quantMatch, vramFree, gpuUtil, queueDepth, latency float64, unavailableTTL time.Duration, maxQueueDepth int) *WeightedScorer {
		if maxQueueDepth <= 0 {
			maxQueueDepth = 10
		}
		return &WeightedScorer{
			QuantMatchWeight: quantMatch,
			VRAMFreeWeight:   vramFree,
			GPUUtilWeight:    gpuUtil,
			QueueDepthWeight: queueDepth,
			LatencyWeight:    latency,
			unavailableTTL:   unavailableTTL,
			maxQueueDepth:    maxQueueDepth,
		}
	}

// Name returns the name of the algorithm.
func (s *WeightedScorer) Name() string {
	return "weighted"
}

// GetUnavailableTTL returns the unavailable TTL.
func (s *WeightedScorer) GetUnavailableTTL() time.Duration {
	return s.unavailableTTL
}

// MaxQueueDepth returns the queue-depth normalization threshold.
func (s *WeightedScorer) MaxQueueDepth() int {
	if s.maxQueueDepth <= 0 {
		return 10
	}
	return s.maxQueueDepth
}

// Score implements the weighted scoring algorithm.
// Returns a score between 0 and 1 (higher is better).
func (s *WeightedScorer) Score(ctx context.Context, request ModelRequest, worker WorkerInfo) (float64, error) {
	// Check if worker is available (heartbeat recent enough)
	if time.Since(worker.LastHeartbeat) > s.unavailableTTL {
		return 0, fmt.Errorf("%w: last heartbeat %v ago", ErrWorkerUnavailable, time.Since(worker.LastHeartbeat))
	}

	// Find the requested model in worker's models
	var model *protocol.ModelInfo
	for i := range worker.Models {
		if worker.Models[i].Name == request.Model {
			model = &worker.Models[i]
			break
		}
	}
	if model == nil {
		return 0, fmt.Errorf("%w: model %s not found", ErrNoMatchingWorkers, request.Model)
	}

	// Check if model is loaded
	if !model.Loaded {
		return 0, fmt.Errorf("%w: model %s not loaded", ErrNoMatchingWorkers, request.Model)
	}

	// Check quantization compatibility
	if !isQuantizationCompatible(request.Quantization, model.Quantization) {
		return 0, fmt.Errorf("%w: quantization %s not compatible with model quantization %s", ErrNoMatchingWorkers, request.Quantization, model.Quantization)
	}

	// Check VRAM constraints
	if request.MaxVRAMMB > 0 && worker.VRAMFreeMB < request.MaxVRAMMB {
		return 0, fmt.Errorf("%w: insufficient VRAM (need %d MB, have %d MB)", ErrNoCapacity, request.MaxVRAMMB, worker.VRAMFreeMB)
	}
	if request.MinVRAMMB > 0 && worker.VRAMFreeMB < request.MinVRAMMB {
		return 0, fmt.Errorf("%w: insufficient VRAM (need at least %d MB, have %d MB)", ErrNoCapacity, request.MinVRAMMB, worker.VRAMFreeMB)
	}

	// Calculate individual score components (0-1 range where 1 is best)

	// 1. Quantization match bonus (exact match gets full bonus)
	quantMatchScore := 0.0
	if request.Quantization == "" || request.Quantization == model.Quantization {
		quantMatchScore = 1.0 // Exact match or no preference
	} else {
		// Compatible but not exact gets partial bonus based on quantization hierarchy
		// More capable quantizations (higher bitrate) get some credit
		quantMatchScore = quantizationCompatibilityScore(request.Quantization, model.Quantization)
	}

	// 2. Free VRAM ratio (more free VRAM is better)
	vramFreeRatio := 0.0
	if worker.VRAMTotalMB > 0 {
		vramFreeRatio = float64(worker.VRAMFreeMB) / float64(worker.VRAMTotalMB)
		// Clamp to 0-1 range
		if vramFreeRatio > 1.0 {
			vramFreeRatio = 1.0
		}
	} else {
		// If total VRAM unknown, assume 50% free as neutral
		vramFreeRatio = 0.5
	}

	// 3. GPU utilization score (lower utilization is better, so invert)
	gpuUtilScore := 0.0
	if worker.GPUUtilPct >= 0 {
		// Convert utilization (0-100) to score (1-0, inverted): 0% util = 1.0, 100% util = 0.0
		gpuUtilScore = 1.0 - (worker.GPUUtilPct / 100.0)
		if gpuUtilScore < 0 {
			gpuUtilScore = 0
		}
		if gpuUtilScore > 1 {
			gpuUtilScore = 1
		}
	} else {
		// If utilization unknown, assume 50% as neutral
		gpuUtilScore = 0.5
	}

	// 4. Queue depth score (lower depth is better, so invert)
	// Normalize queue depth against maxQueueDepth: depth 0 scores 1.0,
	// depth >= maxQueueDepth scores 0.0.
	queueDepthScore := 0.0
	if worker.QueueDepth >= 0 {
		norm := s.maxQueueDepth
		if norm <= 0 {
			norm = 10
		}
		queueDepthScore = 1.0 - math.Min(float64(worker.QueueDepth)/float64(norm), 1.0)
		if queueDepthScore < 0 {
			queueDepthScore = 0
		}
		if queueDepthScore > 1 {
			queueDepthScore = 1
		}
	} else {
		// If queue depth unknown, assume neutral
		queueDepthScore = 0.5
	}

	// 5. Latency score (lower latency is better, so invert)
	// Normalize latency: assume 0-1000ms range, where 0ms is best, 1000ms is worst
	latencyScore := 0.0
	if worker.AvgLatencyMS >= 0 {
		// Normalize to 0-1 range where 1 is best (0ms), 0 is worst (>=1000ms)
		latencyScore = 1.0 - math.Min(worker.AvgLatencyMS/1000.0, 1.0)
		if latencyScore < 0 {
			latencyScore = 0
		}
		if latencyScore > 1 {
			latencyScore = 1
		}
	} else {
		// If latency unknown, assume 500ms as neutral
		latencyScore = 0.5
	}

	// Calculate weighted sum: all components are 0-1 where higher is better.
	// Formula: score = w1*quantMatch + w2*vramFree + w3*gpuUtilScore + w4*queueDepthScore + w5*latencyScore
	score := s.QuantMatchWeight*quantMatchScore +
		s.VRAMFreeWeight*vramFreeRatio +
		s.GPUUtilWeight*gpuUtilScore +
		s.QueueDepthWeight*queueDepthScore +
		s.LatencyWeight*latencyScore

	// Ensure score is in reasonable range (0-1)
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score, nil
}

// isQuantizationCompatible checks if the requested quantization can use the model's quantization.
// More bits = less quantized = more capable and backwards compatible.
// Hierarchy: FP16 > Q8 > Q6 > Q5 > Q4
func isQuantizationCompatible(requested, actual string) bool {
	// If no preference, any quantization works
	if requested == "" {
		return true
	}

	// Exact match always works
	if requested == actual {
		return true
	}

	// Define quantization capability hierarchy (higher value = more capable)
	quantCapability := map[string]int{
		"FP16": 5,
		"Q8":   4,
		"Q6":   3,
		"Q5":   2,
		"Q4":   1,
		// Default to 0 for unknown quantizations
	}

	reqCap := quantCapability[requested]
	actCap := quantCapability[actual]

	// If unknown quantization, be conservative and say it's not compatible
	if reqCap == 0 || actCap == 0 {
		return false
	}

	// Requested can use actual if actual is at least as capable as requested
	return actCap >= reqCap
}

// quantizationCompatibilityScore returns a score (0-1) for how well the actual quantization
// matches the requested quantization, based on capability hierarchy.
func quantizationCompatibilityScore(requested, actual string) float64 {
	// If exact match, return 1.0 (handled by caller, but just in case)
	if requested == actual {
		return 1.0
	}

	// Define quantization capability hierarchy
	quantCapability := map[string]int{
		"FP16": 5,
		"Q8":   4,
		"Q6":   3,
		"Q5":   2,
		"Q4":   1,
	}

	reqCap := quantCapability[requested]
	actCap := quantCapability[actual]

	// If unknown quantization, return 0
	if reqCap == 0 || actCap == 0 {
		return 0.0
	}

	// If actual is more capable than requested, give partial credit
	// The more capable it is, the higher the score (but less than exact match)
	if actCap > reqCap {
		// Score based on how much more capable, capped at 0.8 for non-exact match
		extra := actCap - reqCap
		return math.Min(0.8, float64(extra)*0.2) // Each level of extra capability adds 0.2, max 0.8
	}

	// If actual is less capable than requested, it's not compatible (should be caught by isQuantizationCompatible)
	return 0.0
}

// FilterUnavailable removes workers that are unavailable or stale.
func FilterUnavailable(workers []WorkerInfo, unavailableTTL time.Duration) []WorkerInfo {
	if unavailableTTL <= 0 {
		unavailableTTL = defaultUnavailableTTL
	}
	var result []WorkerInfo
	for _, w := range workers {
		if time.Since(w.LastHeartbeat) <= unavailableTTL {
			result = append(result, w)
		}
	}
	return result
}

// FilterByModel returns workers that have the specified model loaded.
func FilterByModel(workers []WorkerInfo, modelName string) []WorkerInfo {
	var result []WorkerInfo
	for _, w := range workers {
		for _, m := range w.Models {
			if m.Name == modelName && m.Loaded {
				result = append(result, w)
				break
			}
		}
	}
	return result
}

// FilterByVRAM returns workers that have sufficient free VRAM for the request.
func FilterByVRAM(request ModelRequest, workers []WorkerInfo) []WorkerInfo {
	var result []WorkerInfo
	for _, w := range workers {
		if request.MaxVRAMMB > 0 && w.VRAMFreeMB < request.MaxVRAMMB {
			continue
		}
		if request.MinVRAMMB > 0 && w.VRAMFreeMB < request.MinVRAMMB {
			continue
		}
		result = append(result, w)
	}
	return result
}

// SelectWorker chooses the best worker from a list using the scoring algorithm.
// Returns the worker with the highest score, or an error if no workers can be scored.
// unavailableTTL is the maximum time since last heartbeat before a worker is considered unavailable.
func SelectWorker(ctx context.Context, algo ScoringAlgorithm, request ModelRequest, workers []WorkerInfo, unavailableTTL time.Duration) (*SelectedWorker, error) {
	if len(workers) == 0 {
		return nil, ErrNoMatchingWorkers
	}

	// Filter unavailable workers
	available := FilterUnavailable(workers, unavailableTTL)
	if len(available) == 0 {
		return nil, ErrAllWorkersUnavailable
	}

	var best WorkerInfo
	var bestScore float64
	var found bool

	for _, w := range available {
		score, err := algo.Score(ctx, request, w)
		if err != nil {
			// Worker cannot be scored (missing model, incompatible quantization, etc.), skip it
			continue
		}
		if !found || score > bestScore {
			bestScore = score
			best = w
			found = true
		}
	}

	if !found {
		// All workers were filtered out by scoring (no model, incompatible quant, etc.)
		return nil, ErrNoMatchingWorkers
	}

	reason := fmt.Sprintf("best score: %.2f", bestScore)
	return &SelectedWorker{
		WorkerInfo: best,
		Score:      bestScore,
		Reason:     reason,
	}, nil
}

// DefaultScoringAlgorithm returns the default scoring algorithm to use.
func DefaultScoringAlgorithm() ScoringAlgorithm {
	return NewWeightedScorer()
}

// DefaultUnavailableTTL returns the default TTL for worker unavailability.
func DefaultUnavailableTTL() time.Duration {
	return defaultUnavailableTTL
}