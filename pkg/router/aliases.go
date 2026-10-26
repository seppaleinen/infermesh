package router

import "strings"

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

// buildAliasConfig converts a plain string map into *ModelAliasConfig.
// Returns nil for empty/nil maps so aliasing is disabled (backward compatible).
// Trims alias and canonical values; skips entries with empty values.
func buildAliasConfig(raw map[string]string) *ModelAliasConfig {
	if len(raw) == 0 {
		return nil
	}
	aliases := make(map[string]AliasTarget, len(raw))
	for alias, canonical := range raw {
		alias = strings.TrimSpace(alias)
		canonical = strings.TrimSpace(canonical)
		if alias == "" || canonical == "" {
			continue
		}
		aliases[alias] = AliasTarget{Canonical: canonical}
	}
	if len(aliases) == 0 {
		return nil
	}
	return &ModelAliasConfig{Aliases: aliases}
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