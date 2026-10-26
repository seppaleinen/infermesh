package router

import "strings"

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