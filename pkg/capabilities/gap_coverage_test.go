package capabilities

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestToGPUInfo verifies the conversion from a detected GPU to the
// protocol GPUInfo struct.
func TestToGPUInfo_WithGPU(t *testing.T) {
	g := &GPU{
		Index:     0,
		Vendor:    "NVIDIA",
		Model:     "RTX 4090",
		DeviceID:  "0x2782",
		TotalVRAM: 24576,
		FreeVRAM:  12288,
		Util:      42,
	}
	info := g.ToGPUInfo()
	if info.Vendor != "NVIDIA" {
		t.Fatalf("Vendor = %q, want NVIDIA", info.Vendor)
	}
	if info.Model != "RTX 4090" {
		t.Fatalf("Model = %q, want RTX 4090", info.Model)
	}
	if info.TotalVRAM != 24576 || info.FreeVRAM != 12288 {
		t.Fatalf("VRAM = %d/%d, want 24576/12288", info.TotalVRAM, info.FreeVRAM)
	}
}

// TestToGPUInfo_Empty checks the no-GPU conversion.
func TestToGPUInfo_NoGPU(t *testing.T) {
	g := &GPU{}
	info := g.ToGPUInfo()
	var zero protocol.GPUInfo
	if info != zero {
		t.Fatalf("ToGPUInfo() = %+v, want zero value", info)
	}
}

// TestLoadModelsFromDir loads model configs from a directory containing
// yaml files, subdirectories, and non-yaml files.
func TestLoadModelsFromDir(t *testing.T) {
	dir := t.TempDir()

	cfg := "models:\n  - name: model-a\n    quantization: Q4_K_M\n"
	if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write models.yml: %v", err)
	}
	cfg2 := "models:\n  - name: model-b\n    quantization: FP16\n"
	if err := os.WriteFile(filepath.Join(dir, "more.yaml"), []byte(cfg2), 0o644); err != nil {
		t.Fatalf("write more.yaml: %v", err)
	}
	// Non-yaml file must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write readme.txt: %v", err)
	}
	// Subdirectory must be skipped.
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	config, err := LoadModelsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadModelsFromDir() err = %v", err)
	}
	if config == nil {
		t.Fatal("LoadModelsFromDir() = nil, want config")
	}
	if len(config.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(config.Models))
	}
}

// TestLoadModelsFromDir_Missing verifies the missing-directory case
// returns nil, nil.
func TestLoadModelsFromDir_Missing(t *testing.T) {
	config, err := LoadModelsFromDir("/nonexistent/path/for/infermesh-test")
	if err != nil {
		t.Fatalf("LoadModelsFromDir() err = %v, want nil", err)
	}
	if config != nil {
		t.Fatalf("LoadModelsFromDir() = %v, want nil", config)
	}
}

// TestCollectRuntimeMetrics verifies runtime metrics collect without
// error on the current host.
func TestCollectRuntimeMetrics(t *testing.T) {
	metrics, err := CollectRuntimeMetrics()
	if err != nil {
		t.Fatalf("CollectRuntimeMetrics() err = %v", err)
	}
	if metrics == nil {
		t.Fatal("CollectRuntimeMetrics() = nil, want metrics")
	}
}
