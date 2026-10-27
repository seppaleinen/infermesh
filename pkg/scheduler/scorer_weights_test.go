package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// Phase 1 of issue #174 ("enable & extend" load-aware + speed-ranked routing):
// the router's default scorer now carries a loaded-model bonus, a cold-load
// penalty, and a throughput weight. These tests pin the resulting semantics
// with RELATIVE ordering assertions only — no absolute score values are
// asserted, so they survive formula constant changes as long as the load-
// aware behavior holds.

const phase1Model = "phase1-model"

// phase1Worker builds a WorkerInfo advertising one model with a fresh
// heartbeat (well within the 30s TTL). Unknown signals stay at their neutral
// sentinels: GPUUtilPct/QueueDepth/AvgLatencyMS < 0 score as 0.5 and
// TokensPerSecond -1 scores as 0.5 when the throughput weight is > 0, so two
// workers built here are otherwise exactly comparable.
func phase1Worker(id string, loaded bool, inFlight, maxInFlight int) WorkerInfo {
	return WorkerInfo{
		ID:              id,
		Addr:            "127.0.0.1:8080",
		VRAMTotalMB:     24576,
		VRAMFreeMB:      16384,
		GPUUtilPct:      -1, // unknown → neutral 0.5
		QueueDepth:      -1, // unknown → neutral 0.5
		InFlight:        inFlight,
		MaxInFlight:     maxInFlight,
		AvgLatencyMS:    -1, // unknown → neutral 0.5
		TokensPerSecond: -1, // unknown → neutral when throughput weight > 0
		LastHeartbeat:   time.Now(),
		Models: []protocol.ModelInfo{
			{Name: phase1Model, Quantization: "Q4", Size: 4660000000, Loaded: loaded},
		},
	}
}

// phase1Scorer builds a weighted scorer with explicit weights for the test
// cases. maxQueueDepth 0 falls back to the built-in default.
func phase1Scorer(quantMatch, vramFree, gpuUtil, queueDepth, latency, bonus, penalty, throughput float64) *WeightedScorer {
	return NewWeightedScorerWithConfig(quantMatch, vramFree, gpuUtil, queueDepth, latency, bonus, penalty, throughput, DefaultUnavailableTTL(), 0)
}

// TestLoadAwareLoadedBeatsUnloadedBusy (U1): under the Phase 1 defaults a
// loaded+idle worker beats an unloaded+busy worker for the same model. The
// UNLOADED worker gets the lower ID and is listed first, so it would win any
// tie-break; the assertion is adversarial to ordering luck.
func TestLoadAwareLoadedBeatsUnloadedBusy(t *testing.T) {
	s := phase1Scorer(0.40, 0.25, 0.15, 0.10, 0.10, 0.15, 0.10, 0.10)
	req := ModelRequest{Model: phase1Model}

	unloadedBusy := phase1Worker("worker-1", false, 4, 4) // lower ID, listed first
	loadedIdle := phase1Worker("worker-2", true, 0, 4)
	workers := []WorkerInfo{unloadedBusy, loadedIdle}

	ctx := context.Background()
	scoreLoaded, err := s.Score(ctx, req, loadedIdle)
	if err != nil {
		t.Fatalf("Score(loaded+idle): %v", err)
	}
	scoreUnloaded, err := s.Score(ctx, req, unloadedBusy)
	if err != nil {
		t.Fatalf("Score(unloaded+busy): %v", err)
	}
	if !(scoreLoaded > scoreUnloaded) {
		t.Errorf("loaded+idle score %v must exceed unloaded+busy score %v under Phase 1 weights",
			scoreLoaded, scoreUnloaded)
	}

	selected, err := SelectWorker(ctx, s, req, workers, DefaultUnavailableTTL())
	if err != nil {
		t.Fatalf("SelectWorker: %v", err)
	}
	if selected.ID != loadedIdle.ID {
		t.Errorf("SelectWorker picked %q, want the loaded worker %q (unloaded busy has the lower ID and would win a tie)",
			selected.ID, loadedIdle.ID)
	}
}

// TestLoadAwareBothZeroRejectsUnloaded (U2): with bonus=0 AND penalty=0 the
// pre-Phase-1 rejection semantics apply — a pool of only unloaded workers
// yields ErrNoMatchingWorkers.
func TestLoadAwareBothZeroRejectsUnloaded(t *testing.T) {
	s := phase1Scorer(0.40, 0.25, 0.15, 0.10, 0.10, 0, 0, 0)
	req := ModelRequest{Model: phase1Model}

	workers := []WorkerInfo{
		phase1Worker("worker-1", false, 0, 4),
		phase1Worker("worker-2", false, 2, 4),
	}

	_, err := SelectWorker(context.Background(), s, req, workers, DefaultUnavailableTTL())
	if !errors.Is(err, ErrNoMatchingWorkers) {
		t.Fatalf("SelectWorker with only unloaded workers: got %v, want ErrNoMatchingWorkers", err)
	}
}

// TestLoadAwareExplicitZeroDisablesNewSignals (U3): all three new weights at
// 0 → ordering among loaded candidates is driven by the base components
// only, and an unloaded advertiser is rejected even when its base score
// would be the highest in the pool.
func TestLoadAwareExplicitZeroDisablesNewSignals(t *testing.T) {
	s := phase1Scorer(0.40, 0.25, 0.15, 0.10, 0.10, 0, 0, 0)
	req := ModelRequest{Model: phase1Model}

	loadedHi := phase1Worker("worker-hi", true, 0, 4)
	loadedHi.VRAMFreeMB = 20480                       // free ratio ~0.83 → best loaded base score
	loadedLo := phase1Worker("worker-lo", true, 0, 4) // default free ratio ~0.67

	unloadedAdv := phase1Worker("worker-adv", false, 0, 4)
	unloadedAdv.VRAMFreeMB = 24576 // free ratio 1.0 → highest base score overall; must be rejected

	workers := []WorkerInfo{unloadedAdv, loadedLo, loadedHi}

	selected, err := SelectWorker(context.Background(), s, req, workers, DefaultUnavailableTTL())
	if err != nil {
		t.Fatalf("SelectWorker: %v", err)
	}
	if selected.ID != loadedHi.ID {
		t.Errorf("SelectWorker picked %q, want %q (highest-VRAM loaded worker; the unloaded advertiser must be rejected)",
			selected.ID, loadedHi.ID)
	}
}

// TestLoadAwareClampTieReturnsLowerID (U4): two perfect loaded workers whose
// raw weighted sums both exceed 1 and pin at the [0,1] clamp. SelectWorker
// must return the lower-ID candidate deterministically across repeated calls.
func TestLoadAwareClampTieReturnsLowerID(t *testing.T) {
	s := phase1Scorer(0.40, 0.25, 0.15, 0.10, 0.10, 0.15, 0.10, 0.10)
	req := ModelRequest{Model: phase1Model, Quantization: "Q4"}

	perfect := func(id string) WorkerInfo {
		w := phase1Worker(id, true, 0, 4)
		w.VRAMFreeMB = w.VRAMTotalMB // free ratio 1.0
		w.GPUUtilPct = 0             // score 1.0
		w.QueueDepth = 0             // score 1.0
		w.AvgLatencyMS = 0           // score 1.0
		w.TokensPerSecond = 200      // throughput score 1.0 (clamped)
		return w
	}

	lower := perfect("worker-1")
	higher := perfect("worker-2")
	workers := []WorkerInfo{lower, higher}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		selected, err := SelectWorker(ctx, s, req, workers, DefaultUnavailableTTL())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if selected.ID != lower.ID {
			t.Fatalf("call %d: picked %q on a clamp tie, want the lower-ID candidate %q", i, selected.ID, lower.ID)
		}
	}
}

// TestLoadAwarePenaltyScalesWithInFlightRatio (U5): with bonus=0 and
// penalty>0, an unloaded+idle worker scores strictly above an
// unloaded+busy one for the same model — the cold-load penalty compounds
// with the in-flight ratio.
func TestLoadAwarePenaltyScalesWithInFlightRatio(t *testing.T) {
	s := phase1Scorer(0.40, 0.25, 0.15, 0.10, 0.10, 0, 0.10, 0)
	req := ModelRequest{Model: phase1Model}

	busy := phase1Worker("worker-busy", false, 4, 4)
	idle := phase1Worker("worker-idle", false, 0, 4)
	workers := []WorkerInfo{busy, idle}

	ctx := context.Background()
	scoreIdle, err := s.Score(ctx, req, idle)
	if err != nil {
		t.Fatalf("Score(unloaded+idle): %v", err)
	}
	scoreBusy, err := s.Score(ctx, req, busy)
	if err != nil {
		t.Fatalf("Score(unloaded+busy): %v", err)
	}
	if !(scoreIdle > scoreBusy) {
		t.Errorf("unloaded+idle score %v must exceed unloaded+busy score %v (penalty compounds with in-flight ratio)",
			scoreIdle, scoreBusy)
	}

	selected, err := SelectWorker(ctx, s, req, workers, DefaultUnavailableTTL())
	if err != nil {
		t.Fatalf("SelectWorker: %v", err)
	}
	if selected.ID != idle.ID {
		t.Errorf("SelectWorker picked %q, want the unloaded+idle worker %q", selected.ID, idle.ID)
	}
}
