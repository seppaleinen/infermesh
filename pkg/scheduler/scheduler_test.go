package scheduler

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// testWorker creates a WorkerInfo with sensible defaults for testing.
func testWorker(id string) WorkerInfo {
	now := time.Now()
	return WorkerInfo{
		ID:           id,
		Addr:         "127.0.0.1:8080",
		VRAMTotalMB:  24576, // 24 GB
		VRAMFreeMB:   16384, // 16 GB free
		GPUUtilPct:   10.0,
		QueueDepth:   0,
		AvgLatencyMS: 50.0,
		LastHeartbeat: now,
		Models: []protocol.ModelInfo{
			{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			{Name: "mistral-7b", Quantization: "Q4", Size: 4100000000, Loaded: true},
		},
	}
}

// TestWeightedScorerDefaultWeights verifies the default weights are set correctly.
func TestWeightedScorerDefaultWeights(t *testing.T) {
	s := NewWeightedScorer()
	if s.QuantMatchWeight != 0.40 {
		t.Errorf("QuantMatchWeight: got %f, want 0.40", s.QuantMatchWeight)
	}
	if s.VRAMFreeWeight != 0.25 {
		t.Errorf("VRAMFreeWeight: got %f, want 0.25", s.VRAMFreeWeight)
	}
	if s.GPUUtilWeight != 0.15 {
		t.Errorf("GPUUtilWeight: got %f, want 0.15", s.GPUUtilWeight)
	}
	if s.QueueDepthWeight != 0.10 {
		t.Errorf("QueueDepthWeight: got %f, want 0.10", s.QueueDepthWeight)
	}
	if s.LatencyWeight != 0.10 {
		t.Errorf("LatencyWeight: got %f, want 0.10", s.LatencyWeight)
	}
	if s.unavailableTTL != defaultUnavailableTTL {
		t.Errorf("unavailableTTL: got %v, want %v", s.unavailableTTL, defaultUnavailableTTL)
	}
}

// TestWeightedScorerName verifies the algorithm name.
func TestWeightedScorerName(t *testing.T) {
	s := NewWeightedScorer()
	if s.Name() != "weighted" {
		t.Errorf("Name: got %s, want 'weighted'", s.Name())
	}
}

// TestIsQuantizationCompatible tests the quantization compatibility logic.
func TestIsQuantizationCompatible(t *testing.T) {
	tests := []struct {
		name     string
		requested string
		actual    string
		want     bool
	}{
		{"exact match Q4", "Q4", "Q4", true},
		{"exact match FP16", "FP16", "FP16", true},
		{"no preference", "", "Q4", true},
		{"no preference FP16", "", "FP16", true},
		{"Q4 request, Q8 actual (compatible)", "Q4", "Q8", true},
		{"Q4 request, FP16 actual (compatible)", "Q4", "FP16", true},
		{"Q5 request, Q6 actual (compatible)", "Q5", "Q6", true},
		{"Q8 request, FP16 actual (compatible)", "Q8", "FP16", true},
		{"Q8 request, Q4 actual (incompatible)", "Q8", "Q4", false},
		{"FP16 request, Q8 actual (incompatible)", "FP16", "Q8", false},
		{"unknown requested", "Q2", "Q4", false},
		{"unknown actual", "Q4", "Q2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isQuantizationCompatible(tt.requested, tt.actual)
			if got != tt.want {
				t.Errorf("isQuantizationCompatible(%q, %q) = %v, want %v", tt.requested, tt.actual, got, tt.want)
			}
		})
	}
}

// TestQuantizationCompatibilityScore tests the quantization compatibility scoring.
func TestQuantizationCompatibilityScore(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		actual    string
		want      float64
	}{
		{"exact match Q4", "Q4", "Q4", 1.0},
		{"exact match FP16", "FP16", "FP16", 1.0},
		{"Q4 request, Q8 actual (3 levels up)", "Q4", "Q8", 0.6},
		{"Q4 request, Q6 actual (2 levels up)", "Q4", "Q6", 0.4},
		{"Q4 request, Q5 actual (1 level up)", "Q4", "Q5", 0.2},
		{"Q4 request, FP16 actual (4 levels up)", "Q4", "FP16", 0.8}, // capped at 0.8
		{"Q5 request, FP16 actual (3 levels up)", "Q5", "FP16", 0.6},
		{"Q8 request, FP16 actual (1 level up)", "Q8", "FP16", 0.2},
		{"Q8 request, Q4 actual (incompatible)", "Q8", "Q4", 0.0},
		{"FP16 request, Q8 actual (incompatible)", "FP16", "Q8", 0.0},
		{"unknown requested", "Q2", "Q4", 0.0},
		{"unknown actual", "Q4", "Q2", 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quantizationCompatibilityScore(tt.requested, tt.actual)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("quantizationCompatibilityScore(%q, %q) = %f, want %f", tt.requested, tt.actual, got, tt.want)
			}
		})
	}
}

// TestWeightedScorerScore tests the Score method with various scenarios.
func TestWeightedScorerScore(t *testing.T) {
	s := NewWeightedScorer()
	ctx := context.Background()

	tests := []struct {
		name        string
		request     ModelRequest
		worker      WorkerInfo
		wantErr     bool
		wantScore   float64 // approximate expected score
		scoreTolerance float64
	}{
		{
			name: "exact match, healthy worker",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: testWorker("worker-1"),
			wantErr: false,
			// Score components:
			// quantMatch=1.0 (exact), vramFree=16384/24576≈0.667, gpuUtil=1.0-0.1=0.9, queue=1.0, latency=1.0-0.05=0.95
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.9 + 0.10*1.0 + 0.10*0.95
			//       = 0.40 + 0.16675 + 0.135 + 0.10 + 0.095 = 0.89675
			wantScore:      0.90,
			scoreTolerance: 0.05,
		},
		{
			name: "no quant preference, exact match available",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "",
			},
			worker: testWorker("worker-1"),
			wantErr: false,
			wantScore:      0.90,
			scoreTolerance: 0.05,
		},
		{
			name: "compatible quant (Q4 req, Q8 actual)",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   0,
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q8", Size: 8000000000, Loaded: true},
				},
			},
			wantErr: false,
			// quantMatch=0.6 (Q4→Q8 is 3 levels up, min(0.8, 3*0.2)=0.6), vramFree=0.667, gpuUtil=0.9, queue=1.0, latency=0.95
			// score = 0.40*0.6 + 0.25*0.667 + 0.15*0.9 + 0.10*1.0 + 0.10*0.95
			//       = 0.24 + 0.16675 + 0.135 + 0.10 + 0.095 = 0.73675
			wantScore:      0.74,
			scoreTolerance: 0.05,
		},
		{
			name: "incompatible quant (Q8 req, Q4 actual)",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q8",
			},
			worker: testWorker("worker-1"),
			wantErr: true,
		},
		{
			name: "model not loaded",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   0,
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: false},
				},
			},
			wantErr: true,
		},
		{
			name: "model not found on worker",
			request: ModelRequest{
				Model:        "llama3-70b",
				Quantization: "Q4",
			},
			worker: testWorker("worker-1"),
			wantErr: true,
		},
		{
			name: "worker unavailable (stale heartbeat)",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   0,
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now().Add(-40 * time.Second), // older than default 30s TTL
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: true,
		},
		{
			name: "insufficient VRAM (MaxVRAMMB constraint)",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
				MaxVRAMMB:    20000, // need 20 GB free
			},
			worker: testWorker("worker-1"), // has 16 GB free
			wantErr: true,
		},
		{
			name: "insufficient VRAM (MinVRAMMB constraint)",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
				MinVRAMMB:    20000, // need at least 20 GB free
			},
			worker: testWorker("worker-1"), // has 16 GB free
			wantErr: true,
		},
		{
			name: "high GPU utilization reduces score",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   90.0, // very high utilization
				QueueDepth:   0,
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// gpuUtilScore = 1.0 - 0.9 = 0.1
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.1 + 0.10*1.0 + 0.10*0.95
			//       = 0.40 + 0.16675 + 0.015 + 0.10 + 0.095 = 0.77675
			wantScore:      0.78,
			scoreTolerance: 0.05,
		},
		{
			name: "high queue depth reduces score",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   10, // high queue depth
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// queueDepthScore = 1.0 - 1.0 = 0.0 (capped)
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.9 + 0.10*0.0 + 0.10*0.95
			//       = 0.40 + 0.16675 + 0.135 + 0.0 + 0.095 = 0.79675
			wantScore:      0.80,
			scoreTolerance: 0.05,
		},
		{
			name: "high latency reduces score",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   0,
				AvgLatencyMS: 1000.0, // high latency
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// latencyScore = 1.0 - 1.0 = 0.0 (capped)
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.9 + 0.10*1.0 + 0.10*0.0
			//       = 0.40 + 0.16675 + 0.135 + 0.10 + 0.0 = 0.80175
			wantScore:      0.80,
			scoreTolerance: 0.05,
		},
		{
			name: "unknown GPU util treated as neutral",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   -1.0, // unknown
				QueueDepth:   0,
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// gpuUtilScore = 0.5 (neutral)
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.5 + 0.10*1.0 + 0.10*0.95
			//       = 0.40 + 0.16675 + 0.075 + 0.10 + 0.095 = 0.83675
			wantScore:      0.84,
			scoreTolerance: 0.05,
		},
		{
			name: "unknown queue depth treated as neutral",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   -1, // unknown
				AvgLatencyMS: 50.0,
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// queueDepthScore = 0.5 (neutral)
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.9 + 0.10*0.5 + 0.10*0.95
			//       = 0.40 + 0.16675 + 0.135 + 0.05 + 0.095 = 0.84675
			wantScore:      0.85,
			scoreTolerance: 0.05,
		},
		{
			name: "unknown latency treated as neutral",
			request: ModelRequest{
				Model:        "llama3-8b",
				Quantization: "Q4",
			},
			worker: WorkerInfo{
				ID:           "worker-1",
				Addr:         "127.0.0.1:8080",
				VRAMTotalMB:  24576,
				VRAMFreeMB:   16384,
				GPUUtilPct:   10.0,
				QueueDepth:   0,
				AvgLatencyMS: -1.0, // unknown
				LastHeartbeat: time.Now(),
				Models: []protocol.ModelInfo{
					{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
				},
			},
			wantErr: false,
			// latencyScore = 0.5 (neutral)
			// score = 0.40*1.0 + 0.25*0.667 + 0.15*0.9 + 0.10*1.0 + 0.10*0.5
			//       = 0.40 + 0.16675 + 0.135 + 0.10 + 0.05 = 0.85175
			wantScore:      0.85,
			scoreTolerance: 0.05,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, err := s.Score(ctx, tt.request, tt.worker)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error but got score %f", score)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if score < tt.wantScore-tt.scoreTolerance || score > tt.wantScore+tt.scoreTolerance {
				t.Errorf("score = %f, want ~%f (tolerance ±%f)", score, tt.wantScore, tt.scoreTolerance)
			}
		})
	}
}

// TestFilterUnavailable tests filtering workers by heartbeat age.
func TestFilterUnavailable(t *testing.T) {
	now := time.Now()
	workers := []WorkerInfo{
		{ID: "fresh", LastHeartbeat: now},
		{ID: "old", LastHeartbeat: now.Add(-40 * time.Second)},
		{ID: "very-old", LastHeartbeat: now.Add(-100 * time.Second)},
	}

	// Default TTL = 30s
	filtered := FilterUnavailable(workers, defaultUnavailableTTL)
	if len(filtered) != 1 {
		t.Errorf("expected 1 available worker, got %d", len(filtered))
	}
	if filtered[0].ID != "fresh" {
		t.Errorf("expected 'fresh' worker, got %s", filtered[0].ID)
	}

	// Custom TTL = 60s
	filtered = FilterUnavailable(workers, 60*time.Second)
	if len(filtered) != 2 {
		t.Errorf("expected 2 available workers with 60s TTL, got %d", len(filtered))
	}
}

// TestFilterByModel tests filtering workers by model availability.
func TestFilterByModel(t *testing.T) {
	workers := []WorkerInfo{
		{
			ID: "worker-1",
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Loaded: true},
				{Name: "mistral-7b", Quantization: "Q4", Loaded: true},
			},
		},
		{
			ID: "worker-2",
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q8", Loaded: true},
			},
		},
		{
			ID: "worker-3",
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Loaded: false}, // not loaded
			},
		},
	}

	filtered := FilterByModel(workers, "llama3-8b")
	if len(filtered) != 2 {
		t.Errorf("expected 2 workers with llama3-8b loaded, got %d", len(filtered))
	}
	// worker-3 should not be included (model not loaded)
	for _, w := range filtered {
		if w.ID == "worker-3" {
			t.Error("worker-3 should not be included (model not loaded)")
		}
	}

	filtered = FilterByModel(workers, "mistral-7b")
	if len(filtered) != 1 {
		t.Errorf("expected 1 worker with mistral-7b loaded, got %d", len(filtered))
	}
	if filtered[0].ID != "worker-1" {
		t.Errorf("expected worker-1, got %s", filtered[0].ID)
	}

	filtered = FilterByModel(workers, "nonexistent")
	if len(filtered) != 0 {
		t.Errorf("expected 0 workers with nonexistent model, got %d", len(filtered))
	}
}

// TestFilterByVRAM tests filtering workers by VRAM constraints.
func TestFilterByVRAM(t *testing.T) {
	workers := []WorkerInfo{
		{ID: "worker-1", VRAMFreeMB: 16384},
		{ID: "worker-2", VRAMFreeMB: 8192},
		{ID: "worker-3", VRAMFreeMB: 4096},
	}

	// MaxVRAMMB = 10000 (need at least 10GB)
	filtered := FilterByVRAM(ModelRequest{MaxVRAMMB: 10000}, workers)
	if len(filtered) != 1 {
		t.Errorf("expected 1 worker with >=10GB VRAM, got %d", len(filtered))
	}
	if filtered[0].ID != "worker-1" {
		t.Errorf("expected worker-1, got %s", filtered[0].ID)
	}

	// MinVRAMMB = 10000 (same test)
	filtered = FilterByVRAM(ModelRequest{MinVRAMMB: 10000}, workers)
	if len(filtered) != 1 {
		t.Errorf("expected 1 worker with >=10GB VRAM, got %d", len(filtered))
	}

	// MaxVRAMMB = 5000
	filtered = FilterByVRAM(ModelRequest{MaxVRAMMB: 5000}, workers)
	if len(filtered) != 2 {
		t.Errorf("expected 2 workers with >=5GB VRAM, got %d", len(filtered))
	}
	if filtered[0].ID != "worker-1" || filtered[1].ID != "worker-2" {
		t.Errorf("expected worker-1 and worker-2, got %s and %s", filtered[0].ID, filtered[1].ID)
	}

	// No constraints
	filtered = FilterByVRAM(ModelRequest{}, workers)
	if len(filtered) != 3 {
		t.Errorf("expected 3 workers with no VRAM constraints, got %d", len(filtered))
	}
}

// TestSelectWorker tests the full worker selection logic.
func TestSelectWorker(t *testing.T) {
	s := NewWeightedScorer()
	ctx := context.Background()

	workers := []WorkerInfo{
		{
			ID:           "worker-1",
			Addr:         "127.0.0.1:8080",
			VRAMTotalMB:  24576,
			VRAMFreeMB:   16384,
			GPUUtilPct:   10.0,
			QueueDepth:   0,
			AvgLatencyMS: 50.0,
			LastHeartbeat: time.Now(),
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			},
		},
		{
			ID:           "worker-2",
			Addr:         "127.0.0.1:8081",
			VRAMTotalMB:  24576,
			VRAMFreeMB:   20480, // more free VRAM
			GPUUtilPct:   5.0,   // lower utilization
			QueueDepth:   0,
			AvgLatencyMS: 40.0,  // lower latency
			LastHeartbeat: time.Now(),
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			},
		},
		{
			ID:           "worker-3",
			Addr:         "127.0.0.1:8082",
			VRAMTotalMB:  16384,
			VRAMFreeMB:   4096,  // less free VRAM
			GPUUtilPct:   80.0,  // high utilization
			QueueDepth:   5,     // queue depth
			AvgLatencyMS: 200.0, // higher latency
			LastHeartbeat: time.Now(),
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			},
		},
	}

	// Test exact match - worker-2 should win (more VRAM, lower util, lower latency)
	selected, err := SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b", Quantization: "Q4"}, workers, defaultUnavailableTTL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != "worker-2" {
		t.Errorf("expected worker-2 (best score), got %s", selected.ID)
	}
	if selected.Score <= 0 {
		t.Errorf("expected positive score, got %f", selected.Score)
	}

	// Test with Q4 request but worker-2 has Q8 (compatible)
	workers[1].Models[0].Quantization = "Q8"
	selected, err = SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b", Quantization: "Q4"}, workers, defaultUnavailableTTL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// worker-1 has exact Q4 match, worker-2 has compatible Q8
	// worker-1 gets quantMatchScore=1.0, worker-2 gets 0.6
	// worker-1 should win due to exact quant match bonus
	if selected.ID != "worker-1" {
		t.Errorf("expected worker-1 (exact quant match), got %s", selected.ID)
	}

	// Test empty workers
	_, err = SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b"}, []WorkerInfo{}, defaultUnavailableTTL)
	if err != ErrNoMatchingWorkers {
		t.Errorf("expected ErrNoMatchingWorkers, got %v", err)
	}

	// Test all workers unavailable
	staleWorkers := []WorkerInfo{
		{ID: "worker-1", LastHeartbeat: time.Now().Add(-40 * time.Second), Models: []protocol.ModelInfo{{Name: "llama3-8b", Loaded: true}}},
	}
	_, err = SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b"}, staleWorkers, defaultUnavailableTTL)
	if err != ErrAllWorkersUnavailable {
		t.Errorf("expected ErrAllWorkersUnavailable, got %v", err)
	}

	// Test all workers filtered out by quantization
	incompatibleWorkers := []WorkerInfo{
		{ID: "worker-1", LastHeartbeat: time.Now(), Models: []protocol.ModelInfo{{Name: "llama3-8b", Quantization: "Q4", Loaded: true}}},
	}
	_, err = SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b", Quantization: "Q8"}, incompatibleWorkers, defaultUnavailableTTL)
	if err != ErrNoMatchingWorkers {
		t.Errorf("expected ErrNoMatchingWorkers for incompatible quant, got %v", err)
	}
}

// TestDefaultScoringAlgorithm tests the default algorithm factory.
func TestDefaultScoringAlgorithm(t *testing.T) {
	algo := DefaultScoringAlgorithm()
	if algo == nil {
		t.Fatal("DefaultScoringAlgorithm returned nil")
	}
	if algo.Name() != "weighted" {
		t.Errorf("expected 'weighted', got %s", algo.Name())
	}
}

// TestSelectWorkerDeterminism verifies that the same request returns the same worker.
func TestSelectWorkerDeterminism(t *testing.T) {
	s := NewWeightedScorer()
	ctx := context.Background()

	workers := []WorkerInfo{
		{
			ID:           "worker-1",
			Addr:         "127.0.0.1:8080",
			VRAMTotalMB:  24576,
			VRAMFreeMB:   16384,
			GPUUtilPct:   10.0,
			QueueDepth:   0,
			AvgLatencyMS: 50.0,
			LastHeartbeat: time.Now(),
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			},
		},
		{
			ID:           "worker-2",
			Addr:         "127.0.0.1:8081",
			VRAMTotalMB:  24576,
			VRAMFreeMB:   16384,
			GPUUtilPct:   10.0,
			QueueDepth:   0,
			AvgLatencyMS: 50.0,
			LastHeartbeat: time.Now(),
			Models: []protocol.ModelInfo{
				{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
			},
		},
	}

	// Run selection multiple times
	var lastID string
	for i := 0; i < 10; i++ {
		selected, err := SelectWorker(ctx, s, ModelRequest{Model: "llama3-8b", Quantization: "Q4"}, workers, defaultUnavailableTTL)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastID == "" {
			lastID = selected.ID
		} else if selected.ID != lastID {
			t.Errorf("selection not deterministic: got %s, previously %s", selected.ID, lastID)
		}
	}
}

// TestCustomWeightedScorer tests creating a scorer with custom weights.
func TestCustomWeightedScorer(t *testing.T) {
	s := NewWeightedScorerWithConfig(0.5, 0.2, 0.1, 0.1, 0.1, 60*time.Second)

	if s.QuantMatchWeight != 0.5 {
		t.Errorf("QuantMatchWeight: got %f, want 0.5", s.QuantMatchWeight)
	}
	if s.VRAMFreeWeight != 0.2 {
		t.Errorf("VRAMFreeWeight: got %f, want 0.2", s.VRAMFreeWeight)
	}
	if s.GPUUtilWeight != 0.1 {
		t.Errorf("GPUUtilWeight: got %f, want 0.1", s.GPUUtilWeight)
	}
	if s.QueueDepthWeight != 0.1 {
		t.Errorf("QueueDepthWeight: got %f, want 0.1", s.QueueDepthWeight)
	}
	if s.LatencyWeight != 0.1 {
		t.Errorf("LatencyWeight: got %f, want 0.1", s.LatencyWeight)
	}
	if s.GetUnavailableTTL() != 60*time.Second {
		t.Errorf("UnavailableTTL: got %v, want 60s", s.GetUnavailableTTL())
	}
}

// TestScoreEdgeCases tests edge cases in scoring.
func TestScoreEdgeCases(t *testing.T) {
	s := NewWeightedScorer()
	ctx := context.Background()

	// Test with zero VRAM total (unknown)
	worker := WorkerInfo{
		ID:           "worker-1",
		Addr:         "127.0.0.1:8080",
		VRAMTotalMB:  0,
		VRAMFreeMB:   0,
		GPUUtilPct:   10.0,
		QueueDepth:   0,
		AvgLatencyMS: 50.0,
		LastHeartbeat: time.Now(),
		Models: []protocol.ModelInfo{
			{Name: "llama3-8b", Quantization: "Q4", Size: 4660000000, Loaded: true},
		},
	}

	score, err := s.Score(ctx, ModelRequest{Model: "llama3-8b", Quantization: "Q4"}, worker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// vramFreeRatio should be 0.5 (neutral)
	// score = 0.40*1.0 + 0.25*0.5 + 0.15*0.9 + 0.10*1.0 + 0.10*0.95
	//       = 0.40 + 0.125 + 0.135 + 0.10 + 0.095 = 0.855
	if score < 0.80 || score > 0.95 {
		t.Errorf("score with unknown VRAM = %f, want ~0.86 (clamped to 1.0)", score)
	}
}