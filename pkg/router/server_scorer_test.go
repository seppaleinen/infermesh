package router

import (
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func TestServerScorerWeightsDefault(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	w := srv.ScorerWeights()
	want := schedulerWeights{QuantMatch: 0.40, VRAMFree: 0.25, GPUUtil: 0.15, QueueDepth: 0.10, Latency: 0.10, MaxQueueDepth: 10}
	if w != want {
		t.Errorf("default weights: got %+v, want %+v", w, want)
	}
	if srv.scorer == nil || srv.scorer.Name() != "weighted" {
		t.Fatalf("expected non-nil weighted scorer, got %v", srv.scorer)
	}
}
func TestServerSetScorerWeightsZeroIsNoOp(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	before := srv.ScorerWeights()
	srv.SetScorerWeights(0, 0, 0, 0, 0, 0, 0, 0)
	if after := srv.ScorerWeights(); after != before {
		t.Errorf("all-zero SetScorerWeights changed weights: got %+v, want %+v", after, before)
	}
}
func TestServerSetScorerWeightsPartialZero(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()
	srv := tr.Server()

	srv.SetScorerWeights(0.5, 0.3, 0.1, 0.05, 0.05, 0, 0, 20)
	w := srv.ScorerWeights()
	wantCustom := schedulerWeights{QuantMatch: 0.5, VRAMFree: 0.3, GPUUtil: 0.1, QueueDepth: 0.05, Latency: 0.05, LoadedModelBonus: 0.0, ColdLoadPenalty: 0.0, MaxQueueDepth: 20}
	if w != wantCustom {
		t.Fatalf("custom weights not applied: got %+v, want %+v", w, wantCustom)
	}

	srv.SetScorerWeights(0, 0.6, 0, 0.2, 0, 0, 0, 0)
	w = srv.ScorerWeights()
	want := schedulerWeights{QuantMatch: 0.5, VRAMFree: 0.6, GPUUtil: 0.1, QueueDepth: 0.2, Latency: 0.05, MaxQueueDepth: 20}
	if w != want {
		t.Errorf("partial-zero weights: got %+v, want %+v", w, want)
	}
}
func TestSelectWorkerDeterminism(t *testing.T) {
	tr, srv := scorerTestServer(t)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	first, err := srv.selectWorker("llama-3-8b")
	if err != nil {
		t.Fatalf("selectWorker: %v", err)
	}
	for i := 0; i < 25; i++ {
		w, err := srv.selectWorker("llama-3-8b")
		if err != nil {
			t.Fatalf("selectWorker (iter %d): %v", i, err)
		}
		if w.ID != first.ID {
			t.Fatalf("selection not deterministic: got %s, previously %s", w.ID, first.ID)
		}
	}
	if first.ID != "worker-a" {
		t.Errorf("expected lowest-ID worker to win ties, got %s", first.ID)
	}
}
func TestSelectWorkerRespectsQueueDepth(t *testing.T) {
	tr, srv := scorerTestServer(t)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	workers := scorerTestWorkers()
	busy, idle := workers[0], workers[1] // worker-b (queued), worker-a (idle)

	load := map[string]WorkerQueueStats{
		busy.ID: {WorkerID: busy.ID, QueueDepth: 8, InFlight: 4, AvgWaitMs: 400},
	}
	srv.loadSnapshot = func() map[string]WorkerQueueStats { return load }

	w, err := srv.selectWorker("llama-3-8b")
	if err != nil {
		t.Fatalf("selectWorker: %v", err)
	}
	if w.ID != idle.ID {
		t.Errorf("expected idle worker to be selected over queued worker, got %s", w.ID)
	}

	selBusy, err := srv.scoreCandidates("llama-3-8b", []protocol.WorkerInfo{busy}, load)
	if err != nil {
		t.Fatalf("scoreCandidates (busy): %v", err)
	}
	selIdle, err := srv.scoreCandidates("llama-3-8b", []protocol.WorkerInfo{idle}, load)
	if err != nil {
		t.Fatalf("scoreCandidates (idle): %v", err)
	}
	if selBusy.Score >= selIdle.Score {
		t.Errorf("expected queued worker to score lower: busy %.4f vs idle %.4f", selBusy.Score, selIdle.Score)
	}
}
