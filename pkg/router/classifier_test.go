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

func TestParseModelAliases_EmptyReturnsNil(t *testing.T) {
	if cfg := ParseModelAliases(""); cfg != nil {
		t.Errorf("expected nil for empty string")
	}
	if cfg := ParseModelAliases("   "); cfg != nil {
		t.Errorf("expected nil for whitespace string")
	}
}

func TestParseModelAliases_SingleEntry(t *testing.T) {
	cfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	if cfg == nil || len(cfg.Aliases) != 1 {
		t.Fatalf("expected 1 entry, got %v", cfg)
	}
	if cfg.Aliases["gemma-4-12b"].Canonical != "google/gemma-4-12b" {
		t.Errorf("unexpected canonical: %s", cfg.Aliases["gemma-4-12b"].Canonical)
	}
}

func TestParseModelAliases_MultipleEntries(t *testing.T) {
	cfg := ParseModelAliases("a=b,c=d")
	if cfg == nil || len(cfg.Aliases) != 2 {
		t.Fatalf("expected 2 entries, got %v", cfg)
	}
	if cfg.Aliases["a"].Canonical != "b" || cfg.Aliases["c"].Canonical != "d" {
		t.Errorf("unexpected entries: %v", cfg.Aliases)
	}
}

func TestParseModelAliases_EmptyPartSkipped(t *testing.T) {
	cfg := ParseModelAliases("a=b,,c=d")
	if cfg == nil || len(cfg.Aliases) != 2 {
		t.Fatalf("expected 2 entries after skipping empty, got %v", cfg)
	}
}

func TestParseModelAliases_WhitespaceTrimmed(t *testing.T) {
	cfg := ParseModelAliases(" a = b , c = d ")
	if cfg == nil || len(cfg.Aliases) != 2 {
		t.Fatalf("expected 2 entries, got %v", cfg)
	}
	if cfg.Aliases["a"].Canonical != "b" || cfg.Aliases["c"].Canonical != "d" {
		t.Errorf("whitespace not trimmed correctly: %v", cfg.Aliases)
	}
}

func TestParseModelAliases_ExtraEqualsPreserved(t *testing.T) {
	cfg := ParseModelAliases("a=b=c")
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Aliases["a"].Canonical != "b=c" {
		t.Errorf("expected canonical 'b=c', got %s", cfg.Aliases["a"].Canonical)
	}
}

func TestParseModelAliases_EmptyKeyDropped(t *testing.T) {
	cfg := ParseModelAliases("=b")
	if cfg != nil {
		t.Errorf("expected nil when key empty, got %v", cfg)
	}
}

func TestParseModelAliases_EmptyCanonicalDropped(t *testing.T) {
	cfg := ParseModelAliases("a=")
	if cfg != nil {
		t.Errorf("expected nil when canonical empty, got %v", cfg)
	}
}

func TestParseModelAliases_DuplicateKeyLastWins(t *testing.T) {
	cfg := ParseModelAliases("a=b,a=c")
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Aliases["a"].Canonical != "c" {
		t.Errorf("expected last wins 'c', got %s", cfg.Aliases["a"].Canonical)
	}
}

func TestParseModelAliases_QuantPinPreserved(t *testing.T) {
	cfg := ParseModelAliases("qwen-coder-7b=qwen2.5-coder-7b-instruct-mlx@4bit")
	if cfg == nil {
		t.Fatal("expected non-nil")
	}
	if cfg.Aliases["qwen-coder-7b"].Canonical != "qwen2.5-coder-7b-instruct-mlx@4bit" {
		t.Errorf("canonical not preserved: %s", cfg.Aliases["qwen-coder-7b"].Canonical)
	}
}

func TestParseModelAliases_MixedQuantAndPlain(t *testing.T) {
	cfg := ParseModelAliases("a=b@c,d=e@f")
	if cfg == nil || len(cfg.Aliases) != 2 {
		t.Fatalf("expected 2 entries, got %v", cfg)
	}
	if cfg.Aliases["a"].Canonical != "b@c" {
		t.Errorf("expected a->b@c, got %s", cfg.Aliases["a"].Canonical)
	}
	if cfg.Aliases["d"].Canonical != "e@f" {
		t.Errorf("expected d->e@f, got %s", cfg.Aliases["d"].Canonical)
	}
}

func TestModelAliasesActive_NilConfig(t *testing.T) {
	if modelAliasesActive(nil) {
		t.Errorf("expected false for nil config")
	}
}

func TestModelAliasesActive_EmptyMap(t *testing.T) {
	cfg := &ModelAliasConfig{Aliases: map[string]AliasTarget{}}
	if modelAliasesActive(cfg) {
		t.Errorf("expected false for empty map")
	}
}

func TestModelAliasesActive_EmptyCanonical(t *testing.T) {
	cfg := &ModelAliasConfig{Aliases: map[string]AliasTarget{"a": {Canonical: ""}}}
	if modelAliasesActive(cfg) {
		t.Errorf("expected false for empty canonical")
	}
}

func TestModelAliasesActive_NonEmpty(t *testing.T) {
	cfg := &ModelAliasConfig{Aliases: map[string]AliasTarget{"a": {Canonical: "b"}}}
	if !modelAliasesActive(cfg) {
		t.Errorf("expected true for non-empty canonical")
	}
}
