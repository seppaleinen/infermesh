package router

import (
	"testing"
)

func TestAutoRoutingConfig_DefaultConfig(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()

	if cfg.Models == nil {
		t.Fatal("Models map should not be nil")
	}
	if len(cfg.Models) != 0 {
		t.Errorf("Models map should be empty by default, got %v", cfg.Models)
	}
	if cfg.SimpleMaxTokens != 200 {
		t.Errorf("SimpleMaxTokens should be 200, got %d", cfg.SimpleMaxTokens)
	}
	if cfg.MediumMaxTokens != 600 {
		t.Errorf("MediumMaxTokens should be 600, got %d", cfg.MediumMaxTokens)
	}
	if cfg.ReasoningKeywords != nil {
		t.Errorf("ReasoningKeywords should be nil by default, got %v", cfg.ReasoningKeywords)
	}
}

func TestAutoRoutingConfig_ResolveModel_Disabled(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()

	model, ok := cfg.ResolveModel(TierSimple)
	if ok {
		t.Errorf("expected disabled config to return false, got model=%s ok=%v", model, ok)
	}
	if model != "" {
		t.Errorf("expected empty model, got %s", model)
	}
}

func TestAutoRoutingConfig_ResolveModel_PartialConfig(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small-model",
		TierMedium:  "",
		TierComplex: "large-model",
	}

	// Simple configured
	model, ok := cfg.ResolveModel(TierSimple)
	if !ok || model != "small-model" {
		t.Errorf("expected small-model, got %s ok=%v", model, ok)
	}

	// Medium not configured
	model, ok = cfg.ResolveModel(TierMedium)
	if ok || model != "" {
		t.Errorf("expected empty for unconfigured tier, got %s ok=%v", model, ok)
	}

	// Complex configured
	model, ok = cfg.ResolveModel(TierComplex)
	if !ok || model != "large-model" {
		t.Errorf("expected large-model, got %s ok=%v", model, ok)
	}
}

func TestAutoRoutingConfig_Classify_Simple(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}

	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Hi"},
		},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierSimple {
		t.Errorf("expected TierSimple for short prompt, got %s", tier)
	}
}

func TestAutoRoutingConfig_Classify_Medium(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}

	// ~300 tokens (1200 chars / 4)
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: string(make([]byte, 1200))},
		},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for ~300 tokens, got %s", tier)
	}
}

func TestAutoRoutingConfig_Classify_Complex(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}

	// ~800 tokens (3200 chars / 4)
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: string(make([]byte, 3200))},
		},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierComplex {
		t.Errorf("expected TierComplex for ~800 tokens, got %s", tier)
	}
}

func TestAutoRoutingConfig_Classify_MaxTokensSignal(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}

	// Short prompt but large max_tokens -> should bump to TierMedium
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Hi"},
		},
		MaxTokens: 1000,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium due to large max_tokens, got %s", tier)
	}
}

func TestAutoRoutingConfig_Classify_ReasoningKeyword(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}
	cfg.ReasoningKeywords = []string{"think", "reason", "step by step"}

	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Please think step by step about this problem."},
		},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium due to reasoning keyword, got %s", tier)
	}
}

func TestAutoRoutingConfig_Classify_ReasoningKeywordCappedAtComplex(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}
	cfg.ReasoningKeywords = []string{"think"}

	// Long prompt (~800 tokens) -> TierComplex. Keyword should not go past Complex.
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: string(make([]byte, 3200)) + " please think"},
		},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierComplex {
		t.Errorf("expected TierComplex (capped), got %s", tier)
	}
}

func TestAutoRoutingConfig_ClassifyCompletion(t *testing.T) {
	cfg := DefaultAutoRoutingConfig()
	cfg.Models = map[ComplexityTier]string{
		TierSimple:  "small",
		TierMedium:  "medium",
		TierComplex: "large",
	}

	req := CompletionRequest{
		Prompt:    "Short prompt",
		MaxTokens: 100,
	}
	tier := cfg.ClassifyCompletion(req)
	if tier != TierSimple {
		t.Errorf("expected TierSimple, got %s", tier)
	}

	req = CompletionRequest{
		Prompt:    string(make([]byte, 3200)),
		MaxTokens: 100,
	}
	tier = cfg.ClassifyCompletion(req)
	if tier != TierComplex {
		t.Errorf("expected TierComplex, got %s", tier)
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"abcdefgh", 2},
		{string(make([]byte, 400)), 100},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := estimateTokens(tt.input); got != tt.expected {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}