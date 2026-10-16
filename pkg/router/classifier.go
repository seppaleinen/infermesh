package router

import (
	"regexp"
	"strings"
)

// AliasTarget wraps the canonical model name that a user-facing alias
// resolves to. The wrapper exists so future per-alias metadata (e.g. a
// description or routing hints) can be added without changing the map
// value type.
type AliasTarget struct {
	Canonical string `json:"canonical"`
}

// ModelAliasConfig maps user-facing model aliases to canonical model names.
// When nil or empty, alias resolution is disabled and requests are routed
// with the literal model name the client sent (backward compatible).
//
// Canonical names are kept verbatim — they may contain characters that are
// not valid in an alias key (e.g. "@", "/", "."), so the split-on-first-"="
// rule in ParseModelAliases is load-bearing.
type ModelAliasConfig struct {
	Aliases map[string]AliasTarget `json:"aliases"`
}

// modelAliasesActive reports whether cfg is non-nil and contains at least
// one alias with a non-empty Canonical. Mirrors autoRoutingActive.
func modelAliasesActive(cfg *ModelAliasConfig) bool {
	if cfg == nil || cfg.Aliases == nil {
		return false
	}
	for _, t := range cfg.Aliases {
		if t.Canonical != "" {
			return true
		}
	}
	return false
}

// autoRoutingActive reports whether cfg is non-nil and contains at least
// one tier with a non-empty model name. Mirrors modelAliasesActive.
func autoRoutingActive(cfg *AutoRoutingConfig) bool {
	if cfg == nil || cfg.Models == nil {
		return false
	}
	for _, m := range cfg.Models {
		if m != "" {
			return true
		}
	}
	return false
}

// ParseModelAliases parses a comma-separated "alias=canonical" specification
// into a ModelAliasConfig. Returns nil for empty/whitespace input or when
// no valid alias survives parsing, so callers can use a nil receiver as a
// sentinel for "aliasing disabled".
//
// Parsing rules:
//   - Split on commas; trim each part; skip empty parts.
//   - Split each part on the FIRST "=" only, so a canonical containing "="
//     (e.g. "foo=bar=baz") is preserved verbatim.
//   - Trim key and canonical; skip if either is empty.
//   - Duplicate keys: last write wins.
func ParseModelAliases(s string) *ModelAliasConfig {
	if strings.TrimSpace(s) == "" {
		return nil
	}

	aliases := make(map[string]AliasTarget)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx < 0 {
			// No "=": skip — an alias without a canonical is not usable.
			continue
		}
		key := strings.TrimSpace(part[:idx])
		canonical := strings.TrimSpace(part[idx+1:])
		if key == "" || canonical == "" {
			continue
		}
		aliases[key] = AliasTarget{Canonical: canonical}
	}

	if len(aliases) == 0 {
		return nil
	}
	return &ModelAliasConfig{Aliases: aliases}
}

// ComplexityTier classifies a prompt by estimated reasoning demand.
// Four tiers: Simple, Medium, Hard, Hardest.
// TierComplex is an alias for TierHardest for backward compatibility.
type ComplexityTier int

const (
	// TierSimple is for short, low-complexity prompts.
	TierSimple ComplexityTier = iota
	// TierMedium is for mid-length prompts with moderate reasoning.
	TierMedium
	// TierHard is for reasoning-heavy prompts with structural complexity.
	TierHard
	// TierHardest is for the most complex prompts (multi-step reasoning,
	// code/math heavy, expert role framing).
	TierHardest
	// TierComplex is an alias for TierHardest (backward compatibility with
	// existing 3-tier configs and tests).
	TierComplex = TierHardest
)

// String returns the human-readable tier name.
func (t ComplexityTier) String() string {
	switch t {
	case TierSimple:
		return "simple"
	case TierMedium:
		return "medium"
	case TierHard:
		return "hard"
	case TierHardest:
		return "complex"
	default:
		return "unknown"
	}
}

// AutoRoutingConfig holds the configurable tier mapping and classification
// thresholds for the model="auto" alias. When nil, auto routing is disabled
// and requests with model="auto" are routed unchanged (the scheduler treats
// "auto" as a literal model name, preserving backward compatibility).
type AutoRoutingConfig struct {
	// Models maps a complexity tier to the concrete model name that
	// requests of that tier should be routed to. A missing or empty model
	// for a tier disables routing for that tier (the request fails with
	// no_workers, matching the existing no-model behavior).
	Models map[ComplexityTier]string

	// SimpleMaxTokens is the upper bound (exclusive) of estimated tokens
	// for a prompt to be classified as TierSimple. Defaults to 200.
	SimpleMaxTokens int

	// MediumMaxTokens is the upper bound (exclusive) of estimated tokens
	// for a prompt to be classified as TierMedium. Defaults to 600.
	// Prompts at or above this threshold are TierHard or TierHardest
	// depending on other signals.
	MediumMaxTokens int

	// ReasoningKeywords is a list of substrings (lower-cased) whose
	// presence in any message content bumps the prompt one tier higher.
	// Used as a cheap signal for reasoning-heavy prompts. Empty by default.
	// Deprecated: use ReasoningPhrases instead for better accuracy.
	ReasoningKeywords []string

	// ReasoningPhrases is a list of multi-word phrases that indicate
	// reasoning-heavy tasks. Case-insensitive. Empty uses default list.
	ReasoningPhrases []string

	// SimplePhrases is a list of multi-word phrases that indicate simple
	// tasks (read/write, translation, lookup). Case-insensitive. Empty uses
	// default list.
	SimplePhrases []string

	// UseFourTierMode enables 4-tier classification (Simple/Medium/Hard/Hardest).
	// When false (default), uses 3-tier mode (Simple/Medium/Complex) for
	// backward compatibility.
	UseFourTierMode bool
}

// classifierRegexes holds pre-compiled regexes for signal detection.
// Initialized once in init() for performance. All fields are read-only
// after init, so they are safe for concurrent access without a mutex.
type classifierRegexes struct {
	// Structural signals
	codeBlock       *regexp.Regexp
	mathNotation    *regexp.Regexp
	numberedList    *regexp.Regexp
	bulletPoints    *regexp.Regexp
	fewShotPatterns *regexp.Regexp
	rolePatterns    *regexp.Regexp

	// Question depth
	whyHowQuestions   *regexp.Regexp
	whatWhenQuestions *regexp.Regexp
}

var regexes classifierRegexes

func init() {
	// Pre-compile all regexes once at startup for performance.
	// Target: <50ms per classification.
	regexes = classifierRegexes{
		// Structural signals
		codeBlock:       regexp.MustCompile(`(?s)` + "```.*?```"),
		mathNotation:    regexp.MustCompile(`\$[^$]+\$|\\\[.*?\\\]|\\begin\{`),
		numberedList:    regexp.MustCompile(`(?m)^\s*\d+[\.)]`),
		bulletPoints:    regexp.MustCompile(`(?m)^\s*[-*•]\s`),
		fewShotPatterns: regexp.MustCompile(`(?i)(example\s*\d*|input:|output:|here's?\s+an?\s+example)`),
		rolePatterns:    regexp.MustCompile(`(?i)(act as|you are a|pretend to be|imagine you're|as an? \w+ expert)`),

		// Question depth
		whyHowQuestions:   regexp.MustCompile(`(?i)^(why|how)\s+(does|do|did|would|could|can|will|should|must|may|might)`),
		whatWhenQuestions: regexp.MustCompile(`(?i)^(what|when|where|who|which)\s+(is|are|was|were|does|do|did)`),
	}
}

// DefaultAutoRoutingConfig returns a config with the documented default
// thresholds. The Models map is empty, so auto routing is disabled until
// the operator configures tier models via CLI flags.
//
// Default phrase lists are shipped for reasoning and simple tasks.
func DefaultAutoRoutingConfig() AutoRoutingConfig {
	return AutoRoutingConfig{
		Models:            make(map[ComplexityTier]string),
		SimpleMaxTokens:   200,
		MediumMaxTokens:   600,
		ReasoningKeywords: nil,
		ReasoningPhrases:  nil, // uses default in classify()
		SimplePhrases:     nil, // uses default in classify()
		UseFourTierMode:   false,
	}
}

// defaultReasoningPhrases returns the default list of reasoning-indicating phrases.
func defaultReasoningPhrases() []string {
	return []string{
		"explain why", "explain how",
		"analyze the", "compare and contrast",
		"evaluate the", "critique",
		"prove that", "show that", "demonstrate",
		"think step by step", "chain of thought",
		"work through this", "walk me through",
		"what would happen if", "what if",
		"why does", "how does", "how would",
		"reason through", "logical reasoning",
		"step-by-step", "show your work",
		"derive the", "calculate the",
	}
}

// defaultSimplePhrases returns the default list of simple-task phrases.
func defaultSimplePhrases() []string {
	return []string{
		"summarize", "rewrite", "paraphrase",
		"translate to", "what is", "who is",
		"when did", "where is", "list the",
		"give me", "tell me", "define",
		"convert", "format", "fix the typo",
		"spell check", "grammar check",
		"copy paste", "extract the",
	}
}

// charsPerToken is the heuristic conversion factor used to estimate the
// number of tokens from a prompt's character count. Matches the ~4
// chars/token rule documented in the research brief.
const charsPerToken = 4

// estimateTokens estimates the number of tokens in a string using the
// ~4 chars/token heuristic. Returns 0 for empty input.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + charsPerToken - 1) / charsPerToken
}

// StructuralSignals holds detected structural complexity signals.
type StructuralSignals struct {
	HasCodeBlocks      bool
	HasMathNotation    bool
	HasNumberedList    bool
	HasBulletPoints    bool
	HasNestedStructure bool
}

// detectStructuralSignals analyzes content for structural complexity cues.
func detectStructuralSignals(content string) StructuralSignals {
	sig := StructuralSignals{}

	// Code blocks
	if regexes.codeBlock.MatchString(content) {
		sig.HasCodeBlocks = true
	}

	// Math notation
	if regexes.mathNotation.MatchString(content) {
		sig.HasMathNotation = true
	}

	// Numbered lists
	if regexes.numberedList.MatchString(content) {
		sig.HasNumberedList = true
	}

	// Bullet points
	if regexes.bulletPoints.MatchString(content) {
		sig.HasBulletPoints = true
	}

	// Nested structure (multiple indentation levels)
	lines := strings.Split(content, "\n")
	maxIndent := 0
	for _, line := range lines {
		indent := 0
		for _, ch := range line {
			if ch == ' ' || ch == '\t' {
				indent++
			} else {
				break
			}
		}
		if indent > maxIndent {
			maxIndent = indent
		}
	}
	sig.HasNestedStructure = maxIndent >= 4

	return sig
}

// countPhraseMatches counts case-insensitive phrase matches in content.
func countPhraseMatches(content string, phrases []string) int {
	lower := strings.ToLower(content)
	count := 0
	for _, phrase := range phrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			count++
		}
	}
	return count
}

// computeSemanticAdjustment computes a tier delta from semantic signals
// (structural, instruction type, question depth, context). Positive values
// push the prompt up the tier ladder; negative values push it down.
// The token-based baseline (classifyByTokens) is preserved, so this acts as
// a correction on top of length — keeping backward compatibility with the
// existing 24 auto-routing tests while adding semantic discrimination.
func (c *AutoRoutingConfig) computeSemanticAdjustment(req ChatRequest) int {
	// Concatenate all message content
	var content strings.Builder
	for _, msg := range req.Messages {
		content.WriteString(msg.Content)
		content.WriteString(" ")
	}
	fullContent := content.String()

	adjust := 0

	// 1. Structural signals: code blocks, math notation, lists, nesting.
	// These indicate domain-specific work beyond plain prose.
	structural := detectStructuralSignals(fullContent)
	if structural.HasCodeBlocks {
		adjust += 1
	}
	if structural.HasMathNotation {
		adjust += 1
	}
	if structural.HasNumberedList || structural.HasBulletPoints {
		adjust += 1
	}
	if structural.HasNestedStructure {
		adjust += 1
	}

	// 2. Instruction-type phrases: reasoning vs simple.
	// Reasoning phrases push up; simple phrases push down.
	// NOTE: when the legacy ReasoningKeywords list is configured, the
	// reasoning-phrase signal is skipped to avoid double-counting the same
	// prompt (the keyword bump is applied separately below). This keeps the
	// existing keyword-bump tests passing while still shipping default
	// phrase lists for unconfigured deployments.
	reasoningPhrases := c.ReasoningPhrases
	simplePhrases := c.SimplePhrases
	if len(c.ReasoningKeywords) == 0 {
		if len(reasoningPhrases) == 0 {
			reasoningPhrases = defaultReasoningPhrases()
		}
		if len(simplePhrases) == 0 {
			simplePhrases = defaultSimplePhrases()
		}
	}

	reasoningCount := countPhraseMatches(fullContent, reasoningPhrases)
	simpleCount := countPhraseMatches(fullContent, simplePhrases)

	if reasoningCount > simpleCount {
		adjust += 1
	} else if simpleCount > reasoningCount {
		adjust -= 1
	}

	// 3. Question depth: why/how → reasoning; what/when/where → simple.
	whyHowCount := len(regexes.whyHowQuestions.FindAllString(fullContent, -1))
	whatWhenCount := len(regexes.whatWhenQuestions.FindAllString(fullContent, -1))
	if whyHowCount > whatWhenCount {
		adjust += 1
	} else if whatWhenCount > whyHowCount {
		adjust -= 1
	}

	// 4. Context signals: few-shot examples and role framing indicate
	// specialized knowledge needs.
	if regexes.fewShotPatterns.MatchString(fullContent) {
		adjust += 1
	}
	if regexes.rolePatterns.MatchString(fullContent) {
		adjust += 1
	}

	return adjust
}

// classifyByTokens maps an estimated token count to a tier using the
// configured thresholds. This is the token-based baseline that the
// semantic adjustment is layered on top of.
func (c *AutoRoutingConfig) classifyByTokens(tokens int) ComplexityTier {
	simpleMax := c.SimpleMaxTokens
	if simpleMax <= 0 {
		simpleMax = 200
	}
	mediumMax := c.MediumMaxTokens
	if mediumMax <= 0 {
		mediumMax = 600
	}

	if tokens < simpleMax {
		return TierSimple
	}
	if tokens < mediumMax {
		return TierMedium
	}
	return TierComplex
}

// Classify estimates the complexity of a chat request using multi-signal
// scoring: structural cues, instruction-type phrases, question depth,
// few-shot detection, and role framing layered on top of the token-based
// baseline. It never returns an error; classification is best-effort by
// design (MVP heuristic, no ML).
//
// The classification is deterministic and side-effect free.
//
// Algorithm:
//  1. Compute token-based baseline tier (Simple/Medium/Complex) using the
//     configured thresholds — same as the original length-only classifier.
//  2. Compute a semantic adjustment from structural, instruction-type,
//     question-depth, and context signals.
//  3. Apply the adjustment, capped at the top tier (TierComplex in 3-tier
//     mode, TierHardest in 4-tier mode).
//  4. Legacy reasoning-keyword bump (backward compatible).
//
// Tier structure (3-tier mode, default):
//   - TierSimple: short, low-complexity prompts
//   - TierMedium: mid-length prompts with moderate reasoning
//   - TierComplex (= TierHardest): long or reasoning-heavy prompts
//
// Tier structure (4-tier mode, opt-in via --auto-use-four-tier):
//   - TierSimple: short, low-complexity prompts
//   - TierMedium: mid-length prompts with moderate reasoning
//   - TierHard: reasoning-heavy prompts with structural complexity
//   - TierHardest: most complex (multi-step reasoning, code/math, role framing)
func (c *AutoRoutingConfig) Classify(req ChatRequest) ComplexityTier {
	tokens := c.estimateChatTokens(req)

	// Optional signal: a large max_tokens request implies a longer
	// expected output and thus a more demanding prompt.
	if req.MaxTokens > 500 {
		tokens += 200
	}

	baseline := c.classifyByTokens(tokens)

	// Apply semantic adjustment on top of the token baseline.
	adjust := c.computeSemanticAdjustment(req)

	// Legacy reasoning keyword bump (backward compatible).
	if len(c.ReasoningKeywords) > 0 && c.hasReasoningKeyword(req) {
		adjust += 1
	}

	// Map the baseline + adjustment through the active tier ladder so the
	// enum values stay consistent with the configured mode. In 3-tier mode
	// the ladder is [Simple, Medium, Complex] — TierHard is not reachable.
	// In 4-tier mode the ladder is [Simple, Medium, Hard, Hardest].
	var ladder []ComplexityTier
	if c.UseFourTierMode {
		ladder = []ComplexityTier{TierSimple, TierMedium, TierHard, TierHardest}
	} else {
		ladder = []ComplexityTier{TierSimple, TierMedium, TierComplex}
	}

	// Find the baseline index in the ladder.
	baseIdx := -1
	for i, t := range ladder {
		if t == baseline {
			baseIdx = i
			break
		}
	}
	if baseIdx < 0 {
		// Fallback: baseline not in ladder (shouldn't happen, but be safe).
		return baseline
	}

	idx := baseIdx + adjust
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ladder) {
		idx = len(ladder) - 1
	}

	return ladder[idx]
}

// ClassifyCompletion estimates the complexity of a text-completion request.
// Uses the same multi-signal scoring as Classify but operates on a single
// prompt string instead of chat messages.
func (c *AutoRoutingConfig) ClassifyCompletion(req CompletionRequest) ComplexityTier {
	// Wrap as a single-message chat request for scoring
	chatReq := ChatRequest{
		Messages:  []ChatMessage{{Role: "user", Content: req.Prompt}},
		MaxTokens: req.MaxTokens,
	}
	return c.Classify(chatReq)
}

// estimateChatTokens sums the estimated token count across all message
// contents in a chat request. Role tags are not counted; the heuristic is
// intentionally coarse.
func (c *AutoRoutingConfig) estimateChatTokens(req ChatRequest) int {
	total := 0
	for i := range req.Messages {
		total += estimateTokens(req.Messages[i].Content)
	}
	return total
}

// hasReasoningKeyword returns true if any message content contains one of
// the configured reasoning keywords (case-insensitive substring match).
func (c *AutoRoutingConfig) hasReasoningKeyword(req ChatRequest) bool {
	if len(c.ReasoningKeywords) == 0 {
		return false
	}
	for i := range req.Messages {
		lower := strings.ToLower(req.Messages[i].Content)
		for _, kw := range c.ReasoningKeywords {
			if kw == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
	}
	return false
}

// ResolveModel maps a classified complexity tier to the concrete model name
// configured for that tier. It returns ("", false) when no model is
// configured for the tier, so the caller can surface a no_workers error
// rather than silently routing to a non-existent model.
func (c *AutoRoutingConfig) ResolveModel(tier ComplexityTier) (string, bool) {
	if c == nil || c.Models == nil {
		return "", false
	}
	model, ok := c.Models[tier]
	return model, ok && model != ""
}

// AutoRoutingConfigRequest is the JSON DTO for the GET/POST /meta/auto-routing
// endpoint. It uses string tier names ("simple", "medium", "hard", "hardest")
// rather than the internal ComplexityTier enum so the wire format is stable
// and human-readable. The internal AutoRoutingConfig is never serialized
// directly — conversion is explicit in toAutoRoutingConfigDTO /
// buildAutoRoutingConfig.
type AutoRoutingConfigRequest struct {
	Enabled         bool              `json:"enabled"`
	UseFourTier     bool              `json:"use_four_tier"`
	Models          map[string]string `json:"models"` // tier name → model name
	SimpleMaxTokens int               `json:"simple_max_tokens"`
	MediumMaxTokens int               `json:"medium_max_tokens"`
}

// tierToString maps a ComplexityTier to its wire-format string name.
// TierHardest and TierComplex both map to "hardest" — in 3-tier mode the
// ladder uses TierComplex as the top, but the wire name is "hardest" for
// consistency across modes.
func tierToString(tier ComplexityTier) string {
	switch tier {
	case TierSimple:
		return "simple"
	case TierMedium:
		return "medium"
	case TierHard:
		return "hard"
	case TierHardest:
		// TierHardest and TierComplex have the same underlying value (3),
		// so checking TierHardest is sufficient for both.
		return "hardest"
	default:
		return ""
	}
}

// stringToTier maps a wire-format tier name back to ComplexityTier.
// Accepted names: "simple", "medium", "hard", "hardest" (always),
// and "complex" (alias for hardest, backward compatible with 3-tier configs).
// Both "hardest" and "complex" map to TierHardest (== TierComplex, value 3).
// The UseFourTierMode flag is handled by the caller (buildAutoRoutingConfig).
func stringToTier(name string) (ComplexityTier, bool) {
	switch name {
	case "simple":
		return TierSimple, true
	case "medium":
		return TierMedium, true
	case "hard":
		return TierHard, true
	case "hardest", "complex":
		// TierHardest and TierComplex have the same value (3), so both
		// map to TierHardest. The caller decides whether this means
		// TierComplex (3-tier) or TierHardest (4-tier) — they are equal.
		return TierHardest, true
	default:
		return TierSimple, false
	}
}

// buildAutoRoutingConfig converts an AutoRoutingConfigRequest DTO into the
// internal AutoRoutingConfig. Returns nil when the request has no non-empty
// tier model, which disables auto routing (backward compatible nil
// behavior). An empty/nil models map or enabled=false also yields nil.
func buildAutoRoutingConfig(req *AutoRoutingConfigRequest) *AutoRoutingConfig {
	if req == nil || !req.Enabled || req.Models == nil {
		return nil
	}

	models := make(map[ComplexityTier]string, len(req.Models))
	hasModel := false
	for name, model := range req.Models {
		name = strings.TrimSpace(name)
		model = strings.TrimSpace(model)
		if name == "" || model == "" {
			continue
		}
		tier, ok := stringToTier(name)
		if !ok {
			continue
		}
		// TierHardest and TierComplex have the same value (3), so the
		// stringToTier mapping already returns TierHardest for both
		// "hardest" and "complex". No mode-specific remap is needed.
		models[tier] = model
		hasModel = true
	}

	if !hasModel {
		return nil
	}

	simpleMax := req.SimpleMaxTokens
	if simpleMax <= 0 {
		simpleMax = 200
	}
	if simpleMax > 1_000_000 {
		simpleMax = 1_000_000
	}
	mediumMax := req.MediumMaxTokens
	if mediumMax <= 0 {
		mediumMax = 600
	}
	if mediumMax > 1_000_000 {
		mediumMax = 1_000_000
	}
	if mediumMax <= simpleMax {
		mediumMax = simpleMax + 1
	}

	return &AutoRoutingConfig{
		Models:          models,
		SimpleMaxTokens: simpleMax,
		MediumMaxTokens: mediumMax,
		UseFourTierMode: req.UseFourTier,
	}
}

// toAutoRoutingConfigDTO converts the internal AutoRoutingConfig into the
// wire-format AutoRoutingConfigRequest. Returns a zero-value DTO with
// enabled=false and an empty models map when cfg is nil.
func toAutoRoutingConfigDTO(cfg *AutoRoutingConfig) AutoRoutingConfigRequest {
	if cfg == nil || cfg.Models == nil {
		return AutoRoutingConfigRequest{
			Enabled:         false,
			UseFourTier:     false,
			Models:          make(map[string]string),
			SimpleMaxTokens: 0,
			MediumMaxTokens: 0,
		}
	}

	models := make(map[string]string, len(cfg.Models))
	for tier, model := range cfg.Models {
		name := tierToString(tier)
		if name == "" {
			continue
		}
		models[name] = model
	}

	simpleMax := cfg.SimpleMaxTokens
	if simpleMax <= 0 {
		simpleMax = 200
	}
	mediumMax := cfg.MediumMaxTokens
	if mediumMax <= 0 {
		mediumMax = 600
	}

	return AutoRoutingConfigRequest{
		Enabled:         true,
		UseFourTier:     cfg.UseFourTierMode,
		Models:          models,
		SimpleMaxTokens: simpleMax,
		MediumMaxTokens: mediumMax,
	}
}
