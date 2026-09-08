package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// ModelsConfig represents the YAML configuration for models.
type ModelsConfig struct {
	Models []ModelConfig `yaml:"models"`
}

// ModelConfig represents a single model declaration in the YAML config.
type ModelConfig struct {
	Name         string `yaml:"name"`
	SizeBytes    int64  `yaml:"size_bytes"`
	Quantization string `yaml:"quantization"`
	MaxTokens    int    `yaml:"max_tokens"`
	Backend      string `yaml:"backend"`
	Loaded       bool   `yaml:"loaded"`
}

// LoadModelsConfig loads model declarations from a YAML file.
func LoadModelsConfig(path string) (*ModelsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no config file, return empty
		}
		return nil, fmt.Errorf("reading models config: %w", err)
	}

	var config ModelsConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing models config: %w", err)
	}

	return &config, nil
}

// ToModelInfo converts a ModelConfig to a protocol.ModelInfo.
func (m *ModelConfig) ToModelInfo() protocol.ModelInfo {
	return protocol.ModelInfo{
		Name:         m.Name,
		Size:         m.SizeBytes,
		Quantization: m.Quantization,
		MaxTokens:    m.MaxTokens,
		Backend:      m.Backend,
		Loaded:       m.Loaded,
	}
}

// LoadModelsFromDir loads all YAML model configs from a directory.
func LoadModelsFromDir(dir string) (*ModelsConfig, error) {
	var allModels []ModelConfig

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading models dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		config, err := LoadModelsConfig(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", entry.Name(), err)
		}
		if config != nil {
			allModels = append(allModels, config.Models...)
		}
	}

	return &ModelsConfig{Models: allModels}, nil
}

// ValidateQuantization checks if a quantization string is recognized.
func ValidateQuantization(q string) bool {
	valid := map[string]bool{
		"FP16": true, "FP32": true,
		"Q4_K_M": true, "Q4_K_S": true, "Q4_0": true, "Q4_1": true,
		"Q5_K_M": true, "Q5_K_S": true, "Q5_0": true, "Q5_1": true,
		"Q6_K": true, "Q8_0": true, "Q8_1": true,
		"Q2_K": true, "Q3_K_M": true, "Q3_K_S": true,
		"IQ2_XXS": true, "IQ2_XS": true, "IQ3_XXS": true,
	}
	return valid[q]
}

// ParseQuantizationFromName extracts the quantization method from a model filename.
func ParseQuantizationFromName(name string) string {
	// Look for common quantization patterns in the filename
	// e.g., "Llama-3-8B-Q4_K_M.gguf" -> "Q4_K_M"
	// Case-insensitive match
	lower := strings.ToLower(name)
	for _, q := range []string{"q4_k_m", "q4_k_s", "q5_k_m", "q5_k_s", "q8_0", "q2_k", "fp16", "fp32", "q6_k"} {
		if strings.Contains(lower, q) {
			// Return the canonical (uppercase) form
			return strings.ToUpper(q)
		}
	}
	return ""
}