package router

import (
	"strings"
)

// ComplexityTier classifies a prompt by estimated reasoning demand.
type ComplexityTier int

const (
	// TierSimple is for short, low-complexity prompts.
	TierSimple ComplexityTier = iota
	// TierMedium is for mid-length prompts with moderate reasoning.
	TierMedium
	// TierComplex is for long, multi-turn, or reasoning-heavy prompts.
	TierComplex
)

// String returns the human-readable tier name.
func (t ComplexityTier) String() string {
	switch t {
	case TierSimple:
		return "simple"
	case TierMedium:
		return "medium"
	case TierComplex:
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
	// Prompts at or above this threshold are TierComplex.
	MediumMaxTokens int

	// ReasoningKeywords is a list of substrings (lower-cased) whose
	// presence in any message content bumps the prompt one tier higher.
	// Used as a cheap signal for reasoning-heavy prompts. Empty by default.
	ReasoningKeywords []string
}

// DefaultAutoRoutingConfig returns a config with the documented default
// thresholds. The Models map is empty, so auto routing is disabled until
// the operator configures tier models via CLI flags.
func DefaultAutoRoutingConfig() AutoRoutingConfig {
	return AutoRoutingConfig{
		Models:            make(map[ComplexityTier]string),
		SimpleMaxTokens:   200,
		MediumMaxTokens:   600,
		ReasoningKeywords: nil,
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

// Classify estimates the complexity of a chat request using prompt length
// (chars/token heuristic), the optional max_tokens signal, and the
// presence of reasoning keywords. It never returns an error; classification
// is best-effort by design (MVP heuristic, no ML).
//
// The classification is deterministic and side-effect free.
func (c *AutoRoutingConfig) Classify(req ChatRequest) ComplexityTier {
	tokens := c.estimateChatTokens(req)

	// Optional signal: a large max_tokens request implies a longer
	// expected output and thus a more demanding prompt.
	if req.MaxTokens > 500 {
		tokens += 200
	}

	tier := c.classifyByTokens(tokens)

	// Reasoning keywords bump the prompt one tier higher, capped at
	// TierComplex.
	if c.hasReasoningKeyword(req) && tier < TierComplex {
		tier++
	}

	return tier
}

// ClassifyCompletion estimates the complexity of a text-completion request.
func (c *AutoRoutingConfig) ClassifyCompletion(req CompletionRequest) ComplexityTier {
	tokens := estimateTokens(req.Prompt)

	if req.MaxTokens > 500 {
		tokens += 200
	}

	return c.classifyByTokens(tokens)
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

// classifyByTokens maps an estimated token count to a tier using the
// configured thresholds.
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
