package main

import (
	"reflect"
	"sort"
	"testing"
)

// TestBuildAutoRoutingArgs_Disabled verifies buildAutoRoutingArgs returns nil
// when AutoRouting is nil or Enabled is false.
func TestBuildAutoRoutingArgs_Disabled(t *testing.T) {
	tests := []struct {
		name string
		s    Settings
	}{
		{"nil AutoRouting", Settings{}},
		{"AutoRouting nil pointer", Settings{AutoRouting: nil}},
		{"enabled=false", Settings{AutoRouting: &AutoRoutingSettings{Enabled: false, TierSimple: "small"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAutoRoutingArgs(tt.s)
			if got != nil {
				t.Errorf("buildAutoRoutingArgs = %q, want nil", got)
			}
		})
	}
}

// TestBuildAutoRoutingArgs_Enabled verifies the CLI flag set for a fully
// configured 4-tier auto-routing config.
func TestBuildAutoRoutingArgs_Enabled(t *testing.T) {
	s := Settings{
		AutoRouting: &AutoRoutingSettings{
			Enabled:         true,
			UseFourTier:     true,
			TierSimple:      "small-model",
			TierMedium:      "medium-model",
			TierComplex:     "large-model",
			TierHard:        "hard-model",
			TierHardest:     "hardest-model",
			SimpleMaxTokens: 150,
			MediumMaxTokens: 500,
		},
	}
	got := buildAutoRoutingArgs(s)
	want := []string{
		"--auto-tier-simple", "small-model",
		"--auto-tier-medium", "medium-model",
		"--auto-tier-complex", "large-model",
		"--auto-tier-hard", "hard-model",
		"--auto-tier-hardest", "hardest-model",
		"--auto-use-four-tier",
		"--auto-simple-max-tokens", "150",
		"--auto-medium-max-tokens", "500",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildAutoRoutingArgs = %q, want %q", got, want)
	}
}

// TestBuildAutoRoutingArgs_ThreeTier verifies a 3-tier config (no hard tier,
// UseFourTier=false) emits only the configured tier flags.
func TestBuildAutoRoutingArgs_ThreeTier(t *testing.T) {
	s := Settings{
		AutoRouting: &AutoRoutingSettings{
			Enabled:         true,
			UseFourTier:     false,
			TierSimple:      "small",
			TierMedium:      "medium",
			TierComplex:     "large",
			SimpleMaxTokens: 200,
			MediumMaxTokens: 600,
		},
	}
	got := buildAutoRoutingArgs(s)
	want := []string{
		"--auto-tier-simple", "small",
		"--auto-tier-medium", "medium",
		"--auto-tier-complex", "large",
		"--auto-simple-max-tokens", "200",
		"--auto-medium-max-tokens", "600",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildAutoRoutingArgs = %q, want %q", got, want)
	}
}

// TestBuildAutoRoutingArgs_Partial verifies partial configs: empty tier values
// are skipped, zero token counts are skipped, and UseFourTier=false omits
// the --auto-use-four-tier flag.
func TestBuildAutoRoutingArgs_Partial(t *testing.T) {
	s := Settings{
		AutoRouting: &AutoRoutingSettings{
			Enabled:         true,
			UseFourTier:     false,
			TierSimple:      "small",
			TierMedium:      "",
			TierComplex:     "large",
			TierHard:        "   ",
			TierHardest:     "",
			SimpleMaxTokens: 0,
			MediumMaxTokens: 500,
		},
	}
	got := buildAutoRoutingArgs(s)
	want := []string{
		"--auto-tier-simple", "small",
		"--auto-tier-complex", "large",
		"--auto-medium-max-tokens", "500",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildAutoRoutingArgs = %q, want %q", got, want)
	}
}

// TestBuildAutoRoutingArgs_NoTokenFlags verifies that zero SimpleMaxTokens and
// MediumMaxTokens omit the corresponding flags (they fall back to the router
// defaults of 200/600).
func TestBuildAutoRoutingArgs_NoTokenFlags(t *testing.T) {
	s := Settings{
		AutoRouting: &AutoRoutingSettings{
			Enabled:     true,
			TierSimple:  "small",
			TierMedium:  "medium",
			TierComplex: "large",
		},
	}
	got := buildAutoRoutingArgs(s)
	want := []string{
		"--auto-tier-simple", "small",
		"--auto-tier-medium", "medium",
		"--auto-tier-complex", "large",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildAutoRoutingArgs = %q, want %q", got, want)
	}
}

// TestBuildAutoRoutingArgs_Contains verifies the produced args contain the
// expected flags (order-insensitive) and that --auto-use-four-tier is only
// present when UseFourTier is true.
func TestBuildAutoRoutingArgs_Contains(t *testing.T) {
	t.Run("four-tier contains --auto-use-four-tier", func(t *testing.T) {
		s := Settings{AutoRouting: &AutoRoutingSettings{Enabled: true, UseFourTier: true, TierSimple: "s"}}
		got := buildAutoRoutingArgs(s)
		if !containsArg(got, "--auto-use-four-tier") {
			t.Errorf("args missing --auto-use-four-tier: %q", got)
		}
	})
	t.Run("three-tier omits --auto-use-four-tier", func(t *testing.T) {
		s := Settings{AutoRouting: &AutoRoutingSettings{Enabled: true, UseFourTier: false, TierSimple: "s"}}
		got := buildAutoRoutingArgs(s)
		if containsArg(got, "--auto-use-four-tier") {
			t.Errorf("args must not contain --auto-use-four-tier in 3-tier mode: %q", got)
		}
	})
}

// containsArg reports whether the flag appears in args.
func containsArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// sortedArgs returns a copy of args sorted lexicographically so the caller can
// compare order-insensitively.
func sortedArgs(args []string) []string {
	sorted := append([]string(nil), args...)
	sort.Strings(sorted)
	return sorted
}
