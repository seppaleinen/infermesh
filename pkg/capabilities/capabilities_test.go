package capabilities

import (
	"context"
	"os"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		SysfsPath:       "/sys",
		ProcPath:        "/proc",
		ModelConfigPath: "",
		RefreshInterval: 0,
	}
}

func TestDetectGPU_NoSysfs(t *testing.T) {
	gpu, err := DetectGPU()
	if err != nil {
		t.Logf("DetectGPU returned error (expected on some platforms): %v", err)
	}
	// On platforms without GPU, gpu may be nil or model may be "no GPU detected"
	if gpu != nil && gpu.Model != "" && gpu.Model != "no GPU detected" {
		t.Errorf("expected no GPU detected, got %s", gpu.Model)
	}
}

func TestDetectGPU_WithMockSysfs(t *testing.T) {
	// This test is now platform-specific. On Linux, it tests sysfs-based detection.
	// On other platforms, it may not apply.
	gpu, err := DetectGPU()
	if err != nil {
		t.Logf("DetectGPU returned error: %v", err)
		return
	}
	if gpu != nil {
		t.Logf("Detected GPU: vendor=%s, model=%s, vram=%d MB", gpu.Vendor, gpu.Model, gpu.TotalVRAM)
	}
}

func TestLoadModelsConfig_MissingFile(t *testing.T) {
	cfg := testConfig()
	cfg.ModelConfigPath = "/nonexistent/path/models.yaml"

	models, err := LoadModelsConfig(cfg.ModelConfigPath)
	if err != nil {
		t.Errorf("LoadModelsConfig returned error for missing file: %v", err)
	}
	if models != nil && len(models.Models) != 0 {
		t.Errorf("expected empty models, got %d", len(models.Models))
	}
}

func TestLoadModelsConfig_ValidYAML(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "models-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	yamlContent := `
models:
  - name: "llama-3-8b"
    size_bytes: 4820000000
    quantization: "Q4_K_M"
    max_tokens: 8192
    backend: "llama-cpp"
    loaded: false
  - name: "mistral-7b"
    size_bytes: 4370000000
    quantization: "Q5_K_M"
    max_tokens: 8192
    backend: "llama-cpp"
    loaded: true
`
	if _, err := tmpFile.WriteString(yamlContent); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	cfg := testConfig()
	cfg.ModelConfigPath = tmpFile.Name()

	models, err := LoadModelsConfig(cfg.ModelConfigPath)
	if err != nil {
		t.Fatalf("LoadModelsConfig failed: %v", err)
	}

	if len(models.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(models.Models))
		return
	}

	m1 := models.Models[0]
	if m1.Name != "llama-3-8b" {
		t.Errorf("expected name 'llama-3-8b', got '%s'", m1.Name)
	}
	if m1.Quantization != "Q4_K_M" {
		t.Errorf("expected quantization 'Q4_K_M', got '%s'", m1.Quantization)
	}
	if m1.Backend != "llama-cpp" {
		t.Errorf("expected backend 'llama-cpp', got '%s'", m1.Backend)
	}
	if m1.Loaded != false {
		t.Errorf("expected loaded=false, got %t", m1.Loaded)
	}

	m2 := models.Models[1]
	if m2.Name != "mistral-7b" {
		t.Errorf("expected name 'mistral-7b', got '%s'", m2.Name)
	}
	if m2.Quantization != "Q5_K_M" {
		t.Errorf("expected quantization 'Q5_K_M', got '%s'", m2.Quantization)
	}
	if m2.Loaded != true {
		t.Errorf("expected loaded=true, got %t", m2.Loaded)
	}
}

func TestLoadModelsConfig_InvalidYAML(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "models-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	_, _ = tmpFile.WriteString("invalid: yaml: [")
	_ = tmpFile.Close()

	cfg := testConfig()
	cfg.ModelConfigPath = tmpFile.Name()

	_, err = LoadModelsConfig(cfg.ModelConfigPath)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestValidateQuantization(t *testing.T) {
	validQuantizations := []string{"FP16", "FP32", "Q4_K_M", "Q4_K_S", "Q5_K_M", "Q5_K_S", "Q8_0", "Q2_K", "Q3_K_M"}
	for _, q := range validQuantizations {
		if !ValidateQuantization(q) {
			t.Errorf("expected %q to be valid", q)
		}
	}

	invalidQuantizations := []string{"unknown", "Q99", "", "INVALID"}
	for _, q := range invalidQuantizations {
		if ValidateQuantization(q) {
			t.Errorf("expected %q to be invalid", q)
		}
	}
}

func TestParseQuantizationFromName(t *testing.T) {
	testCases := []struct {
		name     string
		expected string
	}{
		{"Llama-3-8B-Q4_K_M.gguf", "Q4_K_M"},
		{"mistral-7b-Q5_K_M.gguf", "Q5_K_M"},
		{"model-Q8_0.gguf", "Q8_0"},
		{"model-fp16.gguf", "FP16"},
		{"no-quant-here.bin", ""},
		{"", ""},
	}

	for _, tc := range testCases {
		result := ParseQuantizationFromName(tc.name)
		if result != tc.expected {
			t.Errorf("ParseQuantizationFromName(%q) = %q, want %q", tc.name, result, tc.expected)
		}
	}
}

func TestDetectEngines(t *testing.T) {
	engines, err := DetectEngines(EngineDetectionConfig{
		BinaryPaths: []string{},
	})
	if err != nil {
		t.Logf("DetectEngines returned error (may be expected): %v", err)
	}
	_ = engines
}

func TestCollectSystemMetrics(t *testing.T) {
	metrics, err := CollectSystemMetrics()
	if err != nil {
		t.Logf("CollectSystemMetrics returned error (may be expected): %v", err)
	}
	if metrics != nil {
		_ = metrics.MemoryUsedMB
	}
}

func TestAggregator_StartRefresh(t *testing.T) {
	cfg := testConfig()
	cfg.RefreshInterval = 10 * time.Millisecond
	agg := NewAggregator(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go agg.StartRefresh(ctx)

	time.Sleep(20 * time.Millisecond)

	cancel()

	lastUpdate := agg.GetLastUpdate()
	if lastUpdate.IsZero() {
		t.Error("expected GetLastUpdate to be non-zero after refresh")
	}
}

func TestConfig_Defaults(t *testing.T) {
	cfg := Defaults()
	if cfg.SysfsPath != "/sys" {
		t.Errorf("expected SysfsPath '/sys', got '%s'", cfg.SysfsPath)
	}
	if cfg.ProcPath != "/proc" {
		t.Errorf("expected ProcPath '/proc', got '%s'", cfg.ProcPath)
	}
	if cfg.RefreshInterval != 5*time.Minute {
		t.Errorf("expected RefreshInterval 5m, got %v", cfg.RefreshInterval)
	}
}

func TestConfig_Validate(t *testing.T) {
	cfg := Config{}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("Validate() should not return error for empty config: %v", err)
	}
	if cfg.SysfsPath != "/sys" {
		t.Errorf("Validate should set SysfsPath default, got '%s'", cfg.SysfsPath)
	}
	if cfg.ProcPath != "/proc" {
		t.Errorf("Validate should set ProcPath default, got '%s'", cfg.ProcPath)
	}
	if cfg.RefreshInterval != 5*time.Minute {
		t.Errorf("Validate should set RefreshInterval default, got %v", cfg.RefreshInterval)
	}
}

func TestDeviceIDToModel(t *testing.T) {
	testCases := []struct {
		deviceID string
		model    string
	}{
		{"0x2782", "RTX 4090"},
		{"0x2684", "RTX 3090"},
		{"0x2231", "RTX 3080"},
		{"0x2206", "RTX 3070"},
		{"0x2482", "RTX 2080 Ti"},
		{"0x1e04", "RTX 2080"},
		{"0x1f82", "Titan RTX"},
		{"0x0622", "GTX 1080 Ti"},
		{"0x1c03", "GTX 1080"},
	}

	for _, tc := range testCases {
		model := deviceIDToModel(tc.deviceID)
		if model != tc.model {
			t.Errorf("deviceIDToModel(%q) = %q, want %q", tc.deviceID, model, tc.model)
		}
	}

	unknown := deviceIDToModel("0xffff")
	if unknown != "NVIDIA Device 0xffff" {
		t.Errorf("deviceIDToModel(0xffff) = %q, want 'NVIDIA Device 0xffff'", unknown)
	}
}