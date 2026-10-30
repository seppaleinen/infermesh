// Package mock provides a mock router for E2E tests.
// This file contains contract tests that validate the mock fixtures
// against the real pkg/router types to prevent drift.
package mock

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/router"
)

// TestMockFixturesContract validates that all mock fixtures decode into
// the real pkg/router types. Any change to the router structs that changes
// JSON keys or types will cause this test to fail, preventing silent drift
// between the mock and the real router.
func TestMockFixturesContract(t *testing.T) {
	// Locate fixtures directory relative to this test file
	fixturesDir := filepath.Join("..", "frontend", "e2e", "fixtures")

	tests := []struct {
		name     string
		filename string
		validate func(t *testing.T, path string)
	}{
		{"workers", "workers.json", validateWorkers},
		{"popular_models", "models.json", validatePopularModels},
		{"inflight_requests", "requests.json", validateInflightRequests},
		{"inference_stats", "inference-stats.json", validateInferenceStats},
		{"aliases", "aliases.json", validateAliases},
		{"auto_routing", "auto-routing.json", validateAutoRouting},
		{"connections", "connections.json", validateConnections},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(fixturesDir, tt.filename)

			// Check file exists
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Fatalf("fixture file missing: %s", path)
			}

			// Run validation
			tt.validate(t, path)
		})
	}
}

// validateWorkers decodes workers.json into router.WorkersResponse
func validateWorkers(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var resp router.WorkersResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Errorf("decode workers.json into router.WorkersResponse: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Verify at least one worker is present
	if len(resp.Workers) == 0 {
		t.Error("workers.json: expected at least one worker")
	}

	// Validate each worker has required fields
	for i, w := range resp.Workers {
		if w.ID == "" {
			t.Errorf("worker[%d]: missing required field 'id'", i)
		}
		if w.Port <= 0 || w.Port > 65535 {
			t.Errorf("worker[%d]: invalid port %d", i, w.Port)
		}
		if w.Status == "" {
			t.Errorf("worker[%d]: missing required field 'status'", i)
		}
		if w.LoadedModels == nil {
			t.Errorf("worker[%d]: loaded_models should be array (got null)", i)
		}
	}
}

// validatePopularModels decodes models.json into router.PopularModelsResponse
func validatePopularModels(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var resp router.PopularModelsResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Errorf("decode models.json into router.PopularModelsResponse: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Verify at least one model is present
	if len(resp.Models) == 0 {
		t.Error("models.json: expected at least one model")
	}

	// Validate each model has required fields
	for i, m := range resp.Models {
		if m.Model == "" {
			t.Errorf("model[%d]: missing required field 'model'", i)
		}
		if m.WorkerCount < 0 {
			t.Errorf("model[%d]: worker_count cannot be negative", i)
		}
		if m.LoadedWorkerCount < 0 {
			t.Errorf("model[%d]: loaded_worker_count cannot be negative", i)
		}
		if m.CallCount < 0 {
			t.Errorf("model[%d]: call_count cannot be negative", i)
		}
	}
}

// validateInflightRequests decodes requests.json into router.InflightRequestsResponse
func validateInflightRequests(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var resp router.InflightRequestsResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Errorf("decode requests.json into router.InflightRequestsResponse: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Verify at least one request is present
	if len(resp.Requests) == 0 {
		t.Error("requests.json: expected at least one request")
	}

	// Validate each request has required fields
	for i, r := range resp.Requests {
		if r.RequestID == "" {
			t.Errorf("request[%d]: missing required field 'request_id'", i)
		}
		if r.Model == "" {
			t.Errorf("request[%d]: missing required field 'model'", i)
		}
		if r.WorkerID == "" {
			t.Errorf("request[%d]: missing required field 'worker_id'", i)
		}
		if r.Kind == "" {
			t.Errorf("request[%d]: missing required field 'kind'", i)
		}
		if r.Status == "" {
			t.Errorf("request[%d]: missing required field 'status'", i)
		}
		if r.StartedAt.IsZero() {
			t.Errorf("request[%d]: missing or invalid 'started_at'", i)
		}
	}
}

// validateInferenceStats decodes inference-stats.json into router.InferenceStatsResponse
func validateInferenceStats(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var resp router.InferenceStatsResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Errorf("decode inference-stats.json into router.InferenceStatsResponse: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Verify at least one worker stat is present
	if len(resp.Workers) == 0 {
		t.Error("inference-stats.json: expected at least one worker stat")
	}

	// Validate each worker stat has required fields
	for i, w := range resp.Workers {
		if w.WorkerID == "" {
			t.Errorf("worker_stat[%d]: missing required field 'worker_id'", i)
		}
		if w.Model == "" {
			t.Errorf("worker_stat[%d]: missing required field 'model'", i)
		}
		// tokens_per_second, avg_ttft_ms, samples are optional/zeroable
	}
}

// validateAliases decodes aliases.json
func validateAliases(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Aliases fixture matches router.SetAliasesRequest (the POST body shape)
	var req router.SetAliasesRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Errorf("decode aliases.json into router.SetAliasesRequest: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Verify at least one alias is present
	if len(req.Aliases) == 0 {
		t.Error("aliases.json: expected at least one alias")
	}

	// Validate no empty keys or values
	for k, v := range req.Aliases {
		if k == "" {
			t.Error("aliases.json: empty alias key")
		}
		if v == "" {
			t.Errorf("aliases.json: empty value for key %q", k)
		}
	}
}

// validateAutoRouting decodes auto-routing.json
func validateAutoRouting(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Auto-routing fixture matches router.AutoRoutingConfigRequest
	var cfg router.AutoRoutingConfigRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Errorf("decode auto-routing.json into router.AutoRoutingConfigRequest: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	// Validate structure
	if cfg.Models == nil {
		t.Error("auto-routing.json: models map should not be null")
	}

	// Validate tier names are recognized
	validTiers := map[string]bool{
		"simple": true, "medium": true, "hard": true, "hardest": true, "complex": true,
	}
	for tier := range cfg.Models {
		if !validTiers[tier] {
			t.Errorf("auto-routing.json: unrecognized tier %q", tier)
		}
	}
}

// validateConnections decodes connections.json
func validateConnections(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Connections fixture is {"workers": int}, mirroring the
	// anonymous struct the router encodes in handleConnectionCount.
	var resp struct {
		Workers int `json:"workers"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Errorf("decode connections.json: %v", err)
		t.Logf("body snippet: %.200s", string(data))
	}

	if resp.Workers < 0 {
		t.Error("connections.json: workers count cannot be negative")
	}
}
