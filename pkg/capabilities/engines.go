package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EngineDetectionConfig holds configuration for engine detection.
type EngineDetectionConfig struct {
	BinaryPaths []string // additional binary paths to check
}

// DetectEngines scans for installed inference engine binaries.
// It checks common binary paths and /proc/cmdline for running processes.
func DetectEngines(cfg EngineDetectionConfig) ([]string, error) {
	detected := make(map[string]bool)

	// Check common binary locations
	binaryPaths := []string{
		"/usr/local/bin",
		"/usr/bin",
		"/usr/local/cuda/bin",
		"/opt/conda/bin",
	}

	for _, binPath := range binaryPaths {
		for _, engineName := range []string{"llama-server", "llama-cli", "ollama", "lmstudio", "vllm"} {
			fullPath := filepath.Join(binPath, engineName)
			if _, err := os.Stat(fullPath); err == nil {
				detected[engineName] = true
			}
		}
	}

	// Check /proc for running processes
	procEngines := detectEnginesFromProc()
	for _, engine := range procEngines {
		detected[engine] = true
	}

	// Convert map to slice
	var engines []string
	for engine := range detected {
		engines = append(engines, engine)
	}

	if len(engines) == 0 {
		return nil, nil // no engines detected
	}

	return engines, nil
}

// detectEnginesFromProc scans /proc/*/cmdline for running engine processes.
func detectEnginesFromProc() []string {
	var engines []string

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "") {
			continue
		}
		// Only look at numeric PIDs
		pid := entry.Name()
		if _, err := fmt.Sscanf(pid, "%d", new(int)); err != nil {
			continue
		}

		cmdlinePath := filepath.Join("/proc", pid, "cmdline")
		data, err := os.ReadFile(cmdlinePath)
		if err != nil {
			continue
		}

		cmdline := string(data)
		for _, engine := range []string{"llama-server", "llama-cli", "ollama", "lmstudio", "vllm"} {
			if strings.Contains(cmdline, engine) {
				engines = append(engines, engine)
				break
			}
		}
	}

	return engines
}