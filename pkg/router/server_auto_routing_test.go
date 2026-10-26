package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestHandleAutoRouting_GET verifies GET /meta/auto-routing returns the current
// config as a DTO with string tier names.
func TestHandleAutoRouting_GET(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *AutoRoutingConfig
		wantStatus int
		wantJSON   string
	}{
		{
			name:       "nil config returns enabled=false",
			cfg:        nil,
			wantStatus: http.StatusOK,
			wantJSON:   `{"enabled":false,"use_four_tier":false,"models":{},"simple_max_tokens":0,"medium_max_tokens":0}`,
		},
		{
			name: "configured config returns enabled=true with string tier names",
			cfg: &AutoRoutingConfig{
				Models: map[ComplexityTier]string{
					TierSimple:  "small-model",
					TierMedium:  "medium-model",
					TierHard:    "hard-model",
					TierHardest: "hardest-model",
				},
				SimpleMaxTokens: 150,
				MediumMaxTokens: 500,
				UseFourTierMode: true,
			},
			wantStatus: http.StatusOK,
			wantJSON:   `{"enabled":true,"use_four_tier":true,"models":{"simple":"small-model","medium":"medium-model","hard":"hard-model","hardest":"hardest-model"},"simple_max_tokens":150,"medium_max_tokens":500}`,
		},
		{
			name: "3-tier config uses complex alias as hardest",
			cfg: &AutoRoutingConfig{
				Models: map[ComplexityTier]string{
					TierSimple:  "small",
					TierMedium:  "medium",
					TierComplex: "large",
				},
			},
			wantStatus: http.StatusOK,
			wantJSON:   `{"enabled":true,"use_four_tier":false,"models":{"simple":"small","medium":"medium","hardest":"large"},"simple_max_tokens":200,"medium_max_tokens":600}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})
			server.SetAutoRouting(tt.cfg)

			req := httptest.NewRequest(http.MethodGet, "/meta/auto-routing", nil)
			w := httptest.NewRecorder()

			server.handleAutoRouting(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}

			// Normalize JSON for comparison
			var got, want AutoRoutingConfigRequest
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.wantJSON), &want); err != nil {
				t.Fatalf("failed to unmarshal want JSON: %v", err)
			}

			if got.Enabled != want.Enabled {
				t.Errorf("enabled = %v, want %v", got.Enabled, want.Enabled)
			}
			if got.UseFourTier != want.UseFourTier {
				t.Errorf("use_four_tier = %v, want %v", got.UseFourTier, want.UseFourTier)
			}
			if got.SimpleMaxTokens != want.SimpleMaxTokens {
				t.Errorf("simple_max_tokens = %d, want %d", got.SimpleMaxTokens, want.SimpleMaxTokens)
			}
			if got.MediumMaxTokens != want.MediumMaxTokens {
				t.Errorf("medium_max_tokens = %d, want %d", got.MediumMaxTokens, want.MediumMaxTokens)
			}
			if len(got.Models) != len(want.Models) {
				t.Errorf("models length = %d, want %d", len(got.Models), len(want.Models))
			}
			for k, v := range want.Models {
				if got.Models[k] != v {
					t.Errorf("models[%q] = %q, want %q", k, got.Models[k], v)
				}
			}
		})
	}
}

// TestHandleAutoRouting_POST_valid verifies POST /meta/auto-routing replaces
// config atomically and returns {"ok": true}.
func TestHandleAutoRouting_POST_Valid(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	reqBody := AutoRoutingConfigRequest{
		Enabled:         true,
		UseFourTier:     true,
		Models:          map[string]string{"simple": "small", "medium": "medium", "hard": "hard", "hardest": "hardest"},
		SimpleMaxTokens: 150,
		MediumMaxTokens: 500,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp AutoRoutingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got %v", resp.OK)
	}

	// Verify config was stored
	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg == nil {
		t.Fatal("config should be set")
	}
	if !cfg.UseFourTierMode {
		t.Error("UseFourTierMode should be true")
	}
	if cfg.SimpleMaxTokens != 150 {
		t.Errorf("SimpleMaxTokens = %d, want 150", cfg.SimpleMaxTokens)
	}
	if cfg.MediumMaxTokens != 500 {
		t.Errorf("MediumMaxTokens = %d, want 500", cfg.MediumMaxTokens)
	}

	model, ok := cfg.Models[TierSimple]
	if !ok || model != "small" {
		t.Errorf("TierSimple model = %q ok=%v, want 'small' true", model, ok)
	}
}

// TestHandleAutoRouting_POST_EmptyBody clears config.
func TestHandleAutoRouting_POST_EmptyBody(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	// First set a config
	server.SetAutoRouting(&AutoRoutingConfig{
		Models: map[ComplexityTier]string{TierSimple: "small"},
	})

	// POST empty body
	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader([]byte{}))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Config should be cleared
	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg != nil {
		t.Errorf("config should be nil after empty body, got %+v", cfg)
	}
}

// TestHandleAutoRouting_POST_EnabledFalse clears config.
func TestHandleAutoRouting_POST_EnabledFalse(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	// First set a config
	server.SetAutoRouting(&AutoRoutingConfig{
		Models: map[ComplexityTier]string{TierSimple: "small"},
	})

	reqBody := AutoRoutingConfigRequest{
		Enabled: false,
		Models:  map[string]string{"simple": "small"},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Config should be cleared
	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg != nil {
		t.Errorf("config should be nil when enabled=false, got %+v", cfg)
	}
}

// TestHandleAutoRouting_POST_InvalidJSON returns 400.
func TestHandleAutoRouting_POST_InvalidJSON(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleAutoRouting_POST_NoModels clears config.
func TestHandleAutoRouting_POST_NoModels(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	// First set a config
	server.SetAutoRouting(&AutoRoutingConfig{
		Models: map[ComplexityTier]string{TierSimple: "small"},
	})

	// POST with enabled=true but no models
	reqBody := AutoRoutingConfigRequest{
		Enabled: true,
		Models:  map[string]string{},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Config should be cleared (no non-empty tier model)
	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg != nil {
		t.Errorf("config should be nil when no models provided, got %+v", cfg)
	}
}

// TestHandleAutoRouting_POST_InvalidTierName skips invalid tier names.
func TestHandleAutoRouting_POST_InvalidTierName(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	reqBody := AutoRoutingConfigRequest{
		Enabled: true,
		Models: map[string]string{
			"simple":  "small-model",
			"invalid": "bad-model",
			"unknown": "worse-model",
		},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Only valid tier should be stored
	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg == nil {
		t.Fatal("config should not be nil")
	}
	if len(cfg.Models) != 1 {
		t.Errorf("expected 1 model (simple), got %d", len(cfg.Models))
	}
	model, ok := cfg.Models[TierSimple]
	if !ok || model != "small-model" {
		t.Errorf("TierSimple model = %q ok=%v, want 'small-model' true", model, ok)
	}
}

// TestHandleAutoRouting_POST_ComplexAlias verifies "complex" alias maps to hardest.
func TestHandleAutoRouting_POST_ComplexAlias(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	reqBody := AutoRoutingConfigRequest{
		Enabled: true,
		Models: map[string]string{
			"simple":  "small",
			"medium":  "medium",
			"complex": "large",
		},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg == nil {
		t.Fatal("config should not be nil")
	}
	// "complex" should map to TierHardest (value 3)
	model, ok := cfg.Models[TierHardest]
	if !ok || model != "large" {
		t.Errorf("TierHardest model = %q ok=%v, want 'large' true", model, ok)
	}
}

// TestHandleAutoRouting_POST_EmptyModelSkips verifies entries with empty model
// names are skipped.
func TestHandleAutoRouting_POST_EmptyModelSkips(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	reqBody := AutoRoutingConfigRequest{
		Enabled: true,
		Models: map[string]string{
			"simple": "small",
			"medium": "",
			"hard":   "   ",
		},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/meta/auto-routing", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	server.autoRoutingMu.RLock()
	cfg := server.autoRouting
	server.autoRoutingMu.RUnlock()

	if cfg == nil {
		t.Fatal("config should not be nil")
	}
	if len(cfg.Models) != 1 {
		t.Errorf("expected 1 model (simple), got %d: %v", len(cfg.Models), cfg.Models)
	}
}

// TestHandleAutoRouting_MethodNotAllowed verifies unsupported methods return 405.
func TestHandleAutoRouting_MethodNotAllowed(t *testing.T) {
	server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	req := httptest.NewRequest(http.MethodPut, "/meta/auto-routing", nil)
	w := httptest.NewRecorder()

	server.handleAutoRouting(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestTierToString verifies the tierToString helper returns correct string names.
func TestTierToString(t *testing.T) {
	tests := []struct {
		tier     ComplexityTier
		expected string
	}{
		{TierSimple, "simple"},
		{TierMedium, "medium"},
		{TierHard, "hard"},
		{TierHardest, "hardest"},
		{TierComplex, "hardest"}, // alias for TierHardest
		{ComplexityTier(99), ""},
	}
	for _, tt := range tests {
		if got := tierToString(tt.tier); got != tt.expected {
			t.Errorf("tierToString(%d) = %q, want %q", int(tt.tier), got, tt.expected)
		}
	}
}

// TestStringToTier verifies the stringToTier helper parses string names correctly.
func TestStringToTier(t *testing.T) {
	tests := []struct {
		name     string
		expected ComplexityTier
		wantOK   bool
	}{
		{"simple", TierSimple, true},
		{"medium", TierMedium, true},
		{"hard", TierHard, true},
		{"hardest", TierHardest, true},
		{"complex", TierHardest, true}, // alias
		{"invalid", TierSimple, false},
		{"", TierSimple, false},
	}
	for _, tt := range tests {
		got, ok := stringToTier(tt.name)
		if ok != tt.wantOK {
			t.Errorf("stringToTier(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
		}
		if got != tt.expected {
			t.Errorf("stringToTier(%q) = %d, want %d", tt.name, got, tt.expected)
		}
	}
}

// TestBuildAutoRoutingConfig_NilRequest verifies nil request returns nil config.
func TestBuildAutoRoutingConfig_NilRequest(t *testing.T) {
	cfg := buildAutoRoutingConfig(nil)
	if cfg != nil {
		t.Errorf("expected nil config for nil request, got %+v", cfg)
	}
}

// TestBuildAutoRoutingConfig_Disabled verifies enabled=false returns nil.
func TestBuildAutoRoutingConfig_Disabled(t *testing.T) {
	req := &AutoRoutingConfigRequest{
		Enabled: false,
		Models:  map[string]string{"simple": "small"},
	}
	cfg := buildAutoRoutingConfig(req)
	if cfg != nil {
		t.Errorf("expected nil config for enabled=false, got %+v", cfg)
	}
}

// TestBuildAutoRoutingConfig_NilModels verifies nil models returns nil.
func TestBuildAutoRoutingConfig_NilModels(t *testing.T) {
	req := &AutoRoutingConfigRequest{
		Enabled: true,
		Models:  nil,
	}
	cfg := buildAutoRoutingConfig(req)
	if cfg != nil {
		t.Errorf("expected nil config for nil models, got %+v", cfg)
	}
}

// TestToAutoRoutingConfigDTO_NilConfig verifies nil config returns zero-value DTO.
func TestToAutoRoutingConfigDTO_NilConfig(t *testing.T) {
	dto := toAutoRoutingConfigDTO(nil)
	if dto.Enabled {
		t.Error("expected enabled=false")
	}
	if dto.UseFourTier {
		t.Error("expected use_four_tier=false")
	}
	if len(dto.Models) != 0 {
		t.Errorf("expected empty models, got %v", dto.Models)
	}
	if dto.SimpleMaxTokens != 0 {
		t.Errorf("expected simple_max_tokens=0, got %d", dto.SimpleMaxTokens)
	}
	if dto.MediumMaxTokens != 0 {
		t.Errorf("expected medium_max_tokens=0, got %d", dto.MediumMaxTokens)
	}
}

// TestToAutoRoutingConfigDTO_EmptyModels verifies config with an empty models
// map returns enabled=true with empty models map (the DTO reflects the
// internal config state — autoRoutingActive handles the "is it really
// active?" check separately).
func TestToAutoRoutingConfigDTO_EmptyModels(t *testing.T) {
	cfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{},
	}
	dto := toAutoRoutingConfigDTO(cfg)
	if !dto.Enabled {
		t.Error("expected enabled=true for non-nil config with empty models map")
	}
	if len(dto.Models) != 0 {
		t.Errorf("expected empty models, got %v", dto.Models)
	}
}

// TestAutoRoutingActive_NilConfig verifies autoRoutingActive returns false for nil.
func TestAutoRoutingActive_NilConfig(t *testing.T) {
	if autoRoutingActive(nil) {
		t.Error("expected false for nil config")
	}
}

// TestAutoRoutingActive_NilModels verifies autoRoutingActive returns false for nil models.
func TestAutoRoutingActive_NilModels(t *testing.T) {
	cfg := &AutoRoutingConfig{Models: nil}
	if autoRoutingActive(cfg) {
		t.Error("expected false for nil models")
	}
}

// TestAutoRoutingActive_EmptyModels verifies autoRoutingActive returns false for empty models.
func TestAutoRoutingActive_EmptyModels(t *testing.T) {
	cfg := &AutoRoutingConfig{Models: map[ComplexityTier]string{}}
	if autoRoutingActive(cfg) {
		t.Error("expected false for empty models")
	}
}

// TestAutoRoutingActive_EmptyModelValues verifies autoRoutingActive returns
// false when all model values are empty strings (the check is `m != ""`,
// without whitespace trimming — only truly empty values are ignored).
func TestAutoRoutingActive_EmptyModelValues(t *testing.T) {
	cfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "",
			TierMedium: "",
		},
	}
	if autoRoutingActive(cfg) {
		t.Error("expected false when all model values are empty strings")
	}
}

// TestAutoRoutingActive_WithModel verifies autoRoutingActive returns true when
// at least one tier has a non-empty model.
func TestAutoRoutingActive_WithModel(t *testing.T) {
	cfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
	}
	if !autoRoutingActive(cfg) {
		t.Error("expected true when at least one tier has a non-empty model")
	}
}
