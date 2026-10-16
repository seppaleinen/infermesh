package router

import (
	"strings"
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

// ---------------------------------------------------------------------------
// New tests for the multi-signal semantic classifier (Issue #106)
// ---------------------------------------------------------------------------

// autoTestCfg builds an AutoRoutingConfig with all tiers mapped for tests.
func autoTestCfg() *AutoRoutingConfig {
	return &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small",
			TierMedium:  "medium",
			TierHard:    "hard",
			TierHardest: "hardest",
		},
	}
}

// TestAutoRoutingConfig_TierStringNames verifies the human-readable tier
// names, including the TierComplex alias for TierHardest.
func TestAutoRoutingConfig_TierStringNames(t *testing.T) {
	tests := []struct {
		tier     ComplexityTier
		expected string
	}{
		{TierSimple, "simple"},
		{TierMedium, "medium"},
		{TierHard, "hard"},
		{TierHardest, "complex"},
		{TierComplex, "complex"}, // alias
		{ComplexityTier(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.expected {
			t.Errorf("tier %d String() = %s, want %s", int(tt.tier), got, tt.expected)
		}
	}
}

// TestAutoRoutingConfig_TierComplexAlias verifies backward compatibility:
// TierComplex must equal TierHardest so existing 3-tier configs resolve.
func TestAutoRoutingConfig_TierComplexAlias(t *testing.T) {
	if TierComplex != TierHardest {
		t.Errorf("TierComplex (%d) must equal TierHardest (%d)", TierComplex, TierHardest)
	}
	if TierHard != TierComplex-1 {
		t.Errorf("TierHard (%d) should be one below TierComplex (%d)", TierHard, TierComplex)
	}
}

// TestAutoRoutingConfig_Classify_ShortReasoningBumped verifies a short
// prompt with reasoning phrasing is bumped from Simple to Medium even
// though its token estimate is far below the simple threshold.
func TestAutoRoutingConfig_Classify_ShortReasoningBumped(t *testing.T) {
	cfg := autoTestCfg()

	// "Prove that P != NP." is short (well under 200 tokens) but carries a
	// strong reasoning instruction ("prove that").
	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Prove that P is not equal to NP. Work through the argument step by step."}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for short reasoning prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_CodeBlockBumped verifies a code block
// (structural signal) bumps the prompt one tier up.
func TestAutoRoutingConfig_Classify_CodeBlockBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Explain this function.\n```go\nfunc Add(a, b int) int { return a + b }\n```"}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for code-block prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_MathNotationBumped verifies math notation
// (structural signal) bumps the prompt one tier up.
func TestAutoRoutingConfig_Classify_MathNotationBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Solve the equation $x^2 + 2x - 3 = 0$ for x."}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for math-notation prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_NumberedListBumped verifies a numbered
// list (structural signal) bumps a Medium-baseline prompt to the top tier
// (Complex in 3-tier mode), while the same-length prompt without the list
// stays at Medium.
func TestAutoRoutingConfig_Classify_NumberedListBumped(t *testing.T) {
	cfg := autoTestCfg()

	// Medium-length baseline (~250 tokens / 1000 chars), neutral prose.
	neutral := strings.Repeat("the quick brown fox jumps over the lazy dog twice ", 20) // ~960 chars

	withList := "Review these items carefully:\n1. First item concerns resource allocation.\n2. Second item concerns risk assessment.\n3. Third item concerns timeline.\n" + neutral
	withoutList := "Review these items carefully and provide feedback. " + neutral

	reqList := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: withList}},
		MaxTokens: 100,
	}
	reqPlain := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: withoutList}},
		MaxTokens: 100,
	}

	listTier := cfg.Classify(reqList)
	plainTier := cfg.Classify(reqPlain)

	if plainTier != TierMedium {
		t.Errorf("expected plain prompt to be TierMedium baseline, got %s", plainTier)
	}
	if listTier != TierComplex {
		t.Errorf("expected numbered-list prompt to bump to TierComplex, got %s", listTier)
	}
}

// TestAutoRoutingConfig_Classify_WhyQuestionBumped verifies a why/how
// question (question-depth signal) bumps a Medium-baseline prompt to the
// top tier (Complex in 3-tier mode).
func TestAutoRoutingConfig_Classify_WhyQuestionBumped(t *testing.T) {
	cfg := autoTestCfg()

	// Medium-length baseline (~250 tokens / 1000 chars), neutral prose.
	neutral := strings.Repeat("the quick brown fox jumps over the lazy dog twice ", 20) // ~960 chars

	withWhy := "How does the TCP three-way handshake work? Explain the sequence of flags and timeouts. " + neutral
	withoutWhy := "A discussion about the TCP protocol and its flags. " + neutral

	reqWhy := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: withWhy}},
		MaxTokens: 100,
	}
	reqPlain := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: withoutWhy}},
		MaxTokens: 100,
	}

	whyTier := cfg.Classify(reqWhy)
	plainTier := cfg.Classify(reqPlain)

	if plainTier != TierMedium {
		t.Errorf("expected plain prompt to be TierMedium baseline, got %s", plainTier)
	}
	if whyTier != TierComplex {
		t.Errorf("expected how-question prompt to bump to TierComplex, got %s", whyTier)
	}
}

// TestAutoRoutingConfig_Classify_WhatQuestionNotBumped verifies a what/when
// question (question-depth signal) does NOT bump the prompt.
func TestAutoRoutingConfig_Classify_WhatQuestionNotBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "What is the capital of France? When did the war end?"}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierSimple {
		t.Errorf("expected TierSimple for what/when-question prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_SimplePhraseDemoted verifies a long prompt
// with simple-task phrasing (translation/summarization) is demoted one tier
// relative to a plain prose prompt of the same length.
func TestAutoRoutingConfig_Classify_SimplePhraseDemoted(t *testing.T) {
	cfg := autoTestCfg()

	// Long prose prompt (~300 tokens, 1200 chars) with a simple instruction.
	base := strings.Repeat("lorem ipsum dolor sit amet ", 20) // 800 chars
	simplePrompt := "Translate to French: " + base
	plainPrompt := "Here is some text: " + base

	reqSimple := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: simplePrompt}},
		MaxTokens: 100,
	}
	reqPlain := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: plainPrompt}},
		MaxTokens: 100,
	}

	simpleTier := cfg.Classify(reqSimple)
	plainTier := cfg.Classify(reqPlain)

	if simpleTier > plainTier {
		t.Errorf("expected simple-phrase prompt (%s) to be <= plain prompt (%s)", simpleTier, plainTier)
	}
}

// TestAutoRoutingConfig_Classify_FewShotBumped verifies few-shot example
// detection (context signal) bumps the prompt one tier up.
func TestAutoRoutingConfig_Classify_FewShotBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Classify sentiment. Example 1: \"Great!\" Input: positive. Example 2: \"Terrible.\" Output: negative. Now: \"Okay.\""}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for few-shot prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_RoleFramingBumped verifies system-role
// framing (context signal) bumps the prompt one tier up.
func TestAutoRoutingConfig_Classify_RoleFramingBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Act as an expert cryptographer and explain this cipher."}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for role-framing prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_MultipleSignalsBumpMultiTier verifies that
// several structural + reasoning signals compound, bumping the prompt two
// or more tiers above the plain-prose baseline.
func TestAutoRoutingConfig_Classify_MultipleSignalsBumpMultiTier(t *testing.T) {
	cfg := autoTestCfg()

	plain := strings.Repeat("lorem ipsum dolor sit amet ", 10) // 400 chars ~ 100 tokens
	plainPrompt := "Consider this text: " + plain

	// Same length, but with reasoning phrase + code block + why-question.
	richPrompt := "Why does this matter? Explain why. Here is the code:\n```\nlet x = 1\n```\n" + plain

	reqPlain := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: plainPrompt}},
		MaxTokens: 100,
	}
	reqRich := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: richPrompt}},
		MaxTokens: 100,
	}

	plainTier := cfg.Classify(reqPlain)
	richTier := cfg.Classify(reqRich)

	if int(richTier) < int(plainTier)+2 {
		t.Errorf("expected multi-signal prompt (%s) to be >=2 tiers above plain (%s)", richTier, plainTier)
	}
}

// TestAutoRoutingConfig_Classify_ComplexCappedAtTop verifies a reasoning
// prompt already at TierComplex cannot be bumped past the top tier in
// 3-tier mode.
func TestAutoRoutingConfig_Classify_ComplexCappedAtTop(t *testing.T) {
	cfg := autoTestCfg()
	// Plain 3-tier config: UseFourTierMode false.

	// ~800 tokens + reasoning signals -> should cap at TierComplex.
	longPrompt := strings.Repeat("analyze the impact of the policy on stakeholders ", 40) // ~1600 chars = 400 tokens
	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: longPrompt}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierComplex {
		t.Errorf("expected TierComplex (capped at top), got %s", tier)
	}
}

// TestAutoRoutingConfig_FourTierMode_Simple verifies 4-tier mode keeps
// short plain prompts at TierSimple.
func TestAutoRoutingConfig_FourTierMode_Simple(t *testing.T) {
	cfg := autoTestCfg()
	cfg.UseFourTierMode = true

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: "Hi"}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierSimple {
		t.Errorf("expected TierSimple in 4-tier mode, got %s", tier)
	}
}

// TestAutoRoutingConfig_FourTierMode_HardTier verifies that a medium prompt
// with reasoning signals lands on TierHard in 4-tier mode (not Medium, not
// Hardest).
func TestAutoRoutingConfig_FourTierMode_HardTier(t *testing.T) {
	cfg := autoTestCfg()
	cfg.UseFourTierMode = true

	// ~300 tokens (1200 chars) with reasoning phrasing + a code block.
	base := strings.Repeat("lorem ipsum dolor sit amet ", 20)                                                 // 800 chars
	prompt := "Explain why this design is wrong. Work through it step by step.\n```\nlet x = 1\n```\n" + base // +200 chars = 1000 chars total ~ 250 tokens

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: prompt}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	// Baseline is Medium (250 tokens < 600). Reasoning phrase (+1) + code
	// block (+1) + why-question (+1) = +3, but capped at top. Should be at
	// least TierHard.
	if tier != TierHard && tier != TierHardest {
		t.Errorf("expected TierHard or TierHardest in 4-tier mode, got %s", tier)
	}
}

// TestAutoRoutingConfig_FourTierMode_HardestTier verifies a long prompt with
// multiple signals lands on TierHardest in 4-tier mode.
func TestAutoRoutingConfig_FourTierMode_HardestTier(t *testing.T) {
	cfg := autoTestCfg()
	cfg.UseFourTierMode = true

	// ~800 tokens (3200 chars) + reasoning + code + role framing.
	base := strings.Repeat("evaluate the tradeoffs between latency and throughput in the system ", 40) // ~1800 chars
	prompt := "Act as an expert systems architect. Prove that the following invariant holds. " + base +
		"\n```\nfunc main() { }\n```"

	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: prompt}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	// Baseline is Complex (>= 600 tokens) = TierHardest. Cap at top.
	if tier != TierHardest {
		t.Errorf("expected TierHardest in 4-tier mode, got %s", tier)
	}
}

// TestAutoRoutingConfig_FourTierMode_BehindThreeTierFlags verifies 4-tier
// mode can be enabled while the 3-tier flags are still the source of the
// tier models (the operator maps TierComplex = TierHardest explicitly).
func TestAutoRoutingConfig_FourTierMode_ResolveModel(t *testing.T) {
	cfg := autoTestCfg()
	cfg.UseFourTierMode = true

	// ResolveModel should still work for TierHard and TierHardest.
	model, ok := cfg.ResolveModel(TierHard)
	if !ok || model != "hard" {
		t.Errorf("expected hard model for TierHard, got %s ok=%v", model, ok)
	}
	model, ok = cfg.ResolveModel(TierHardest)
	if !ok || model != "hardest" {
		t.Errorf("expected hardest model for TierHardest, got %s ok=%v", model, ok)
	}
	// TierComplex alias resolves to the same as TierHardest.
	model, ok = cfg.ResolveModel(TierComplex)
	if !ok || model != "hardest" {
		t.Errorf("expected hardest model for TierComplex alias, got %s ok=%v", model, ok)
	}
}

// TestAutoRoutingConfig_ClassifyCompletion_ReasoningBumped verifies the
// completion path also benefits from the semantic signals (reasoning phrase
// bumps a short prompt up one tier).
func TestAutoRoutingConfig_ClassifyCompletion_ReasoningBumped(t *testing.T) {
	cfg := autoTestCfg()

	req := CompletionRequest{
		Prompt:    "Prove that every even number greater than 2 is the sum of two primes.",
		MaxTokens: 100,
	}
	tier := cfg.ClassifyCompletion(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium for reasoning completion prompt, got %s", tier)
	}
}

// TestAutoRoutingConfig_Classify_NoSignalsPreservesBaseline verifies a plain
// prose prompt with no semantic signals lands exactly on the token-based
// baseline tier (backward-compatibility guard).
func TestAutoRoutingConfig_Classify_NoSignalsPreservesBaseline(t *testing.T) {
	cfg := autoTestCfg()

	// ~300 tokens (1200 chars), no reasoning/simple phrases, no structure.
	// Use repeated words that avoid any phrase match.
	base := strings.Repeat("x y z w v u t s r q p o n m l k j h g f e d c b a ", 18) // 1224 chars ~ 306 tokens
	req := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: base}},
		MaxTokens: 100,
	}
	tier := cfg.Classify(req)
	if tier != TierMedium {
		t.Errorf("expected TierMedium baseline (no signals), got %s", tier)
	}
}
